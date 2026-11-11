package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubS3 is a minimal fake S3: HEAD for existence, PUT for upload.
// It stores objects in memory (bucket/key -> bytes) and records the
// requests it saw.
type stubS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	requests []string
}

func newStubS3() *stubS3 {
	return &stubS3{objects: map[string][]byte{}}
}

func (s *stubS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	// Path style: /<bucket>/<key>.
	key := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case "HEAD":
		data, ok := s.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(http.StatusOK)
	case "PUT":
		data, _ := io.ReadAll(r.Body)
		s.objects[key] = data
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("ETag", `"fake-etag"`)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><PutObjectResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ETag>"fake-etag"</ETag></PutObjectResult>`))
	default:
		http.Error(w, "stub: unhandled "+r.Method, http.StatusNotFound)
	}
}

// stubS3Uploader builds an S3 uploader pointed at the stub server with
// fake static credentials. It returns the uploader and the stub.
func stubS3Uploader(t *testing.T, prefix, publicBase string) (*S3Uploader, *stubS3) {
	t.Helper()
	stub := newStubS3()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	cfg := S3UploadConfig{
		Endpoint:      srv.URL,
		Region:        "test",
		Bucket:        "test-bucket",
		Prefix:        prefix,
		AccessKey:     "fake-access-key",
		SecretKey:     "fake-secret-key",
		PublicURLBase: publicBase,
		UsePathStyle:  true,
	}
	u, err := NewS3Uploader(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return u, stub
}

func TestS3UploaderUploadAndExists(t *testing.T) {
	ctx := context.Background()
	u, stub := stubS3Uploader(t, "evidence", "")

	dir := t.TempDir()
	local := filepath.Join(dir, "a.jpg")
	if err := os.WriteFile(local, []byte("fake-jpeg-bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	url, err := u.Upload(ctx, local, "20261111/media/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "evidence/20261111/media/a.jpg"
	if !strings.HasSuffix(url, "/test-bucket/"+wantKey) {
		t.Fatalf("url = %q, want it to end with /test-bucket/%s", url, wantKey)
	}
	stub.mu.Lock()
	got, ok := stub.objects["test-bucket/"+wantKey]
	stub.mu.Unlock()
	if !ok {
		t.Fatalf("object %q not stored; objects = %v", wantKey, stub.objects)
	}
	if string(got) != "fake-jpeg-bytes" {
		t.Fatalf("stored bytes = %q", got)
	}
	// Idempotency: same size exists, different size does not.
	exists, err := u.Exists(ctx, "20261111/media/a.jpg", 15)
	if err != nil || !exists {
		t.Fatalf("Exists(same size) = %v, %v", exists, err)
	}
	exists, err = u.Exists(ctx, "20261111/media/a.jpg", 999)
	if err != nil || exists {
		t.Fatalf("Exists(other size) = %v, %v", exists, err)
	}
	exists, err = u.Exists(ctx, "20261111/media/nope.jpg", 1)
	if err != nil || exists {
		t.Fatalf("Exists(missing) = %v, %v", exists, err)
	}
	// ExistingURL needs no request and matches the upload URL.
	before := len(stub.requests)
	gotURL, err := u.ExistingURL(ctx, "20261111/media/a.jpg")
	if err != nil || gotURL != url {
		t.Fatalf("ExistingURL = %q, %v (upload returned %q)", gotURL, err, url)
	}
	if len(stub.requests) != before {
		t.Fatalf("ExistingURL made a request")
	}
}

func TestS3UploaderPublicBaseAndNoPrefix(t *testing.T) {
	ctx := context.Background()
	u, stub := stubS3Uploader(t, "", "https://cdn.example.invalid")

	dir := t.TempDir()
	local := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(local, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	url, err := u.Upload(ctx, local, "20261111/dokumen/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://cdn.example.invalid/20261111/dokumen/a.txt" {
		t.Fatalf("url = %q", url)
	}
	stub.mu.Lock()
	_, ok := stub.objects["test-bucket/20261111/dokumen/a.txt"]
	stub.mu.Unlock()
	if !ok {
		t.Fatalf("object stored without prefix: %v", stub.objects)
	}
}
