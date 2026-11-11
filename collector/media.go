package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// BridgeClient talks to the bridge REST API. It only uses read endpoints
// (health, download). It never sends messages.
type BridgeClient struct {
	apiURL string
	token  string
	client *http.Client
}

// NewBridgeClient builds a client. The bearer token is read from tokenFile
// (store/.bridge-token, mode 0600). An empty token file means "no auth"
// (the bridge can run without a token when none is configured).
func NewBridgeClient(apiURL, tokenFile string) (*BridgeClient, error) {
	b := &BridgeClient{
		apiURL: strings.TrimSuffix(apiURL, "/"),
		client: &http.Client{Timeout: 120 * time.Second},
	}
	if tokenFile != "" {
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read bridge token %s: %w", tokenFile, err)
			}
			// No token file: the bridge may be running without auth.
		} else {
			b.token = strings.TrimSpace(string(raw))
		}
	}
	return b, nil
}

// Health checks GET /api/health. It returns (true, nil) when the bridge
// answers 200 (connected). A 503 means the bridge is up but WhatsApp is
// disconnected: reads from the local store still work, media downloads
// will fail until it reconnects.
func (b *BridgeClient) Health(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.apiURL+"/health", nil)
	if err != nil {
		return false, err
	}
	b.auth(req)
	resp, err := b.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusServiceUnavailable:
		return false, nil
	default:
		return false, fmt.Errorf("health: unexpected status %d", resp.StatusCode)
	}
}

type downloadRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

type downloadResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename"`
	Path     string `json:"path"`
}

// DownloadMedia asks the bridge to download one message's media
// (POST /api/download). On success it returns the local path the bridge
// saved the file to, under its store directory. The bridge owns the
// whatsmeow download plus the original-URL fallback and .enc decryption.
func (b *BridgeClient) DownloadMedia(ctx context.Context, chatJID, messageID string) (string, error) {
	body, err := json.Marshal(downloadRequest{MessageID: messageID, ChatJID: chatJID})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL+"/download", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	b.auth(req)
	resp, err := b.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("download read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusInternalServerError {
		return "", fmt.Errorf("download: bridge status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out downloadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("download: bad bridge response: %w", err)
	}
	if !out.Success {
		return "", fmt.Errorf("download failed: %s", out.Message)
	}
	if out.Path == "" {
		return "", fmt.Errorf("download: bridge reported success but no path")
	}
	return out.Path, nil
}

func (b *BridgeClient) auth(req *http.Request) {
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
}
