package collector

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RowSource is one message behind an exported row. A row is usually
// one message; after semantic dedup it can be several (the same event
// reworded or cross-posted), and every source is listed here with its
// own chat, sender, timestamp, text, and evidence file. Senders and
// content are redacted exactly like the row itself when redact=true.
type RowSource struct {
	MessageID   string `json:"message_id"`
	ChatJID     string `json:"chat_jid"`
	ChatName    string `json:"chat_name"`
	Sender      string `json:"sender"`
	Timestamp   string `json:"timestamp"` // RFC3339 UTC
	Content     string `json:"content"`
	MediaType   string `json:"media_type"`
	MediaFile   string `json:"media_file,omitempty"`   // file name inside media/, "" when none
	MediaStatus string `json:"media_status,omitempty"` // ok | failed | skipped | not-applicable
	ChatRender  string `json:"chat_render,omitempty"`  // "" (a merged row has one render, on the row)
	LinkDrive   string `json:"link_drive,omitempty"`   // remote URL of this source's evidence file
}

// EvidenceRow is one exported message. It is the unit of evidence: a row is
// self-contained (source chat, sender, time, text, media reference, hash).
type EvidenceRow struct {
	MessageID   string `json:"message_id" csv:"message_id"`
	ChatJID     string `json:"chat_jid" csv:"chat_jid"`
	ChatName    string `json:"chat_name" csv:"chat_name"`
	Sender      string `json:"sender" csv:"sender"`
	Timestamp   string `json:"timestamp" csv:"timestamp"` // RFC3339 UTC
	IsFromMe    bool   `json:"is_from_me" csv:"is_from_me"`
	Content     string `json:"content" csv:"content"`
	MediaType   string `json:"media_type" csv:"media_type"`
	MediaFile   string `json:"media_file" csv:"media_file"`     // file name inside media/, "" when none
	MediaStatus string `json:"media_status" csv:"media_status"` // ok | failed | skipped | not-applicable
	// ChatRender is the PNG file name inside media/ of a chat render (a
	// RENDER drawn from the message text, never a screenshot) generated
	// for a text-only row. "" when the row already had a file or link.
	ChatRender string `json:"chat_render" csv:"chat_render"`
	// LinkDrive is the remote URL of the row's evidence file (media file
	// or chat render) after a successful remote upload. "" while exports
	// stay local or when the upload failed / was skipped.
	LinkDrive   string `json:"link_drive" csv:"link_drive"`
	ContentHash string `json:"content_hash" csv:"content_hash"`
	// Sources lists every message behind this row (one entry for a
	// plain row, several after a semantic merge). JSON only: the CSV
	// stays flat (13 columns), and the Excel recap notes the merge in
	// its Konteks column instead.
	Sources []RowSource `json:"sources,omitempty"`
	// Merged is true when the row combines 2+ messages (same event
	// reworded or cross-posted). JSON only.
	Merged bool `json:"merged,omitempty"`
	// IsCorrection is true when a correction message joined the row.
	// JSON only; the Excel Konteks column notes "(ralat)".
	IsCorrection bool `json:"is_correction,omitempty"`
	// Note carries the merge/similarity notes for the row
	// ("digabung dari 2 sumber, 2 grup", "mirip dengan <id> (jam
	// berbeda: ...)"). JSON only; the Excel Konteks column appends it
	// to the run label.
	Note string `json:"note,omitempty"`
}

// EvidenceFile is the JSON export envelope.
type EvidenceFile struct {
	Version     int           `json:"version"`
	Label       string        `json:"label"`
	WindowStart string        `json:"window_start"`
	WindowEnd   string        `json:"window_end"`
	GeneratedAt string        `json:"generated_at"`
	Rows        []EvidenceRow `json:"rows"`
}

var csvHeader = []string{
	"timestamp", "chat_jid", "chat_name", "sender", "message_id", "is_from_me",
	"content", "media_type", "media_file", "media_status", "content_hash",
	"chat_render", "link_drive",
}

// WriteJSON writes the evidence envelope as indented JSON.
func WriteJSON(path string, env EvidenceFile) error {
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

// WriteCSV writes the same rows as CSV (RFC4180 via encoding/csv).
func WriteCSV(path string, rows []EvidenceRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range rows {
		rec := []string{
			r.Timestamp, r.ChatJID, r.ChatName, r.Sender, r.MessageID,
			boolStr(r.IsFromMe), r.Content, r.MediaType, r.MediaFile,
			r.MediaStatus, r.ContentHash, r.ChatRender, r.LinkDrive,
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// WriteManifest writes a sha256sum-format manifest (one "<hex>  <name>" line
// per file, sorted by name) covering every file in dir except the manifest
// itself. It lets anyone verify a run folder was not tampered with later.
// Extra names are additional files listed in the manifest, given as paths
// relative to dir (e.g. "../Rekap-Evidence-202611-gabungan.xlsx" for the
// monthly file that lives one level up); they are hashed from dir too, so
// `sha256sum -c manifest.sha256` works from inside dir.
func WriteManifest(dir string, extra ...string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == "manifest.sha256" {
			continue
		}
		files = append(files, e.Name())
	}
	// Include media/ files with their relative path.
	mediaDir := filepath.Join(dir, "media")
	if infos, err := os.ReadDir(mediaDir); err == nil {
		for _, info := range infos {
			if !info.IsDir() {
				files = append(files, filepath.Join("media", info.Name()))
			}
		}
	}
	files = append(files, extra...)
	sort.Strings(files)
	manifestPath := filepath.Join(dir, "manifest.sha256")
	f, err := os.Create(manifestPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, name := range files {
		sum, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(f, "%s  %s\n", sum, name); err != nil {
			return "", err
		}
	}
	return manifestPath, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FileSHA256 is the exported helper for tests and callers.
func FileSHA256(path string) (string, error) { return fileSHA256(path) }

// RunDir returns the per-date run directory for a run starting at t
// (output_dir/YYYYMMDD) and creates it, including its media/ subfolder.
func RunDir(outputDir string, t time.Time) (string, error) {
	dir := filepath.Join(outputDir, t.Format("20060102"))
	if err := os.MkdirAll(filepath.Join(dir, "media"), 0755); err != nil {
		return "", err
	}
	return dir, nil
}
