package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// stubDriveFile is one file/folder in the fake Drive.
type stubDriveFile struct {
	id          string
	name        string
	parentID    string
	mimeType    string
	size        int64
	webViewLink string
}

// stubDrive is an in-memory fake of the Drive v3 HTTP API: just enough
// for the uploader (Files.List, Files.Get, Files.Create with and
// without media). It records every request path for assertions.
type stubDrive struct {
	mu       sync.Mutex
	files    map[string]*stubDriveFile // id -> file
	children map[string][]string       // parentID -> file ids
	next     int
	sessions map[string]map[string]interface{} // resumable session -> metadata
	requests []string
}

func newStubDrive() *stubDrive {
	s := &stubDrive{
		files:    map[string]*stubDriveFile{},
		children: map[string][]string{},
		sessions: map[string]map[string]interface{}{},
	}
	// The Drive root itself.
	s.files["root"] = &stubDriveFile{id: "root", name: "My Drive", mimeType: driveFolderMimeType}
	return s
}

func (s *stubDrive) nextID() string {
	s.next++
	return fmt.Sprintf("id-%d", s.next)
}

func (s *stubDrive) add(f *stubDriveFile) {
	s.files[f.id] = f
	s.children[f.parentID] = append(s.children[f.parentID], f.id)
	if f.webViewLink == "" && f.mimeType != driveFolderMimeType {
		f.webViewLink = "https://drive.google.com/file/d/" + f.id + "/view"
	}
}

func (s *stubDrive) writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// driveFileJSON is the wire shape the client parses (only the fields
// the uploader asks for).
func driveFileJSON(f *stubDriveFile) map[string]interface{} {
	m := map[string]interface{}{"id": f.id, "name": f.name, "mimeType": f.mimeType}
	if f.mimeType != driveFolderMimeType {
		m["size"] = fmt.Sprint(f.size)
		m["webViewLink"] = f.webViewLink
	}
	return m
}

var (
	driveQueryName   = regexp.MustCompile(`name = '((?:[^']|'')*)'`)
	driveQueryParent = regexp.MustCompile(`'([^']+)' in parents`)
)

func (s *stubDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)

	switch {
	// Files.List: GET /drive/v3/files?q=...
	case r.Method == "GET" && r.URL.Path == "/drive/v3/files":
		name := ""
		if m := driveQueryName.FindStringSubmatch(r.URL.Query().Get("q")); m != nil {
			name = strings.ReplaceAll(m[1], "''", "'")
		}
		parent := ""
		if m := driveQueryParent.FindStringSubmatch(r.URL.Query().Get("q")); m != nil {
			parent = m[1]
		}
		var out []map[string]interface{}
		for _, id := range s.children[parent] {
			f := s.files[id]
			if name != "" && f.name != name {
				continue
			}
			out = append(out, driveFileJSON(f))
		}
		if out == nil {
			out = []map[string]interface{}{}
		}
		s.writeJSON(w, map[string]interface{}{"files": out})

	// Files.Get: GET /drive/v3/files/<id>
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/drive/v3/files/"):
		id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		f, ok := s.files[id]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		s.writeJSON(w, driveFileJSON(f))

	// Files.Create without media (folder create): POST /drive/v3/files
	case r.Method == "POST" && r.URL.Path == "/drive/v3/files":
		var meta map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		name, _ := meta["name"].(string)
		mimeType, _ := meta["mimeType"].(string)
		parent := "root"
		if parents, ok := meta["parents"].([]interface{}); ok && len(parents) > 0 {
			parent, _ = parents[0].(string)
		}
		f := &stubDriveFile{id: s.nextID(), name: name, parentID: parent, mimeType: mimeType}
		s.add(f)
		s.writeJSON(w, driveFileJSON(f))

	// Media upload: POST /upload/drive/v3/files?uploadType=multipart
	case r.Method == "POST" && r.URL.Path == "/upload/drive/v3/files" && r.URL.Query().Get("uploadType") == "multipart":
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, "bad content-type", http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		var meta map[string]interface{}
		var media []byte
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			data, _ := io.ReadAll(part)
			if strings.Contains(part.Header.Get("Content-Type"), "application/json") {
				_ = json.Unmarshal(data, &meta)
			} else {
				media = data
			}
		}
		name, _ := meta["name"].(string)
		parent := "root"
		if parents, ok := meta["parents"].([]interface{}); ok && len(parents) > 0 {
			parent, _ = parents[0].(string)
		}
		f := &stubDriveFile{id: s.nextID(), name: name, parentID: parent, size: int64(len(media))}
		s.add(f)
		s.writeJSON(w, driveFileJSON(f))

	// Resumable upload initiation: POST ...?uploadType=resumable
	case r.Method == "POST" && r.URL.Path == "/upload/drive/v3/files" && r.URL.Query().Get("uploadType") == "resumable":
		var meta map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&meta)
		session := fmt.Sprintf("session-%d", s.next)
		s.next++
		s.sessions[session] = meta
		w.Header().Set("Location", "/upload-session/"+session)
		w.WriteHeader(http.StatusOK)

	// Resumable upload chunk: PUT /upload-session/<id>
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/upload-session/"):
		session := strings.TrimPrefix(r.URL.Path, "/upload-session/")
		meta, ok := s.sessions[session]
		if !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		media, _ := io.ReadAll(r.Body)
		name, _ := meta["name"].(string)
		parent := "root"
		if parents, ok := meta["parents"].([]interface{}); ok && len(parents) > 0 {
			parent, _ = parents[0].(string)
		}
		f := &stubDriveFile{id: s.nextID(), name: name, parentID: parent, size: int64(len(media))}
		s.add(f)
		delete(s.sessions, session)
		s.writeJSON(w, driveFileJSON(f))

	default:
		http.Error(w, "stub: unhandled "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// stubDriveService builds a drive.Service pointed at the stub server
// (no auth). The endpoint keeps the realistic /drive/v3/ base path.
func stubDriveService(t *testing.T, stub *stubDrive) *drive.Service {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	svc, err := drive.NewService(context.Background(),
		option.WithoutAuthentication(),
		option.WithEndpoint(srv.URL+"/drive/v3/"))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestDriveUploaderUploadAndExists(t *testing.T) {
	ctx := context.Background()
	stub := newStubDrive()
	// Pre-seed the remote root folder.
	root := &stubDriveFile{id: "root-id", name: "Evidence", mimeType: driveFolderMimeType}
	stub.add(root)

	svc := stubDriveService(t, stub)
	u := NewDriveUploaderWithService(svc, "root-id")

	dir := t.TempDir()
	local := filepath.Join(dir, "a.jpg")
	if err := os.WriteFile(local, []byte("fake-jpeg-bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	url, err := u.Upload(ctx, local, "20261111/media/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "https://drive.google.com/file/d/") {
		t.Fatalf("url = %q, want a drive view link", url)
	}
	// The date + media folders were created under the root.
	if len(stub.children["root-id"]) != 1 {
		t.Fatalf("root children = %v, want the date folder", stub.children["root-id"])
	}
	dateID := stub.children["root-id"][0]
	if stub.files[dateID].name != "20261111" {
		t.Fatalf("date folder = %q", stub.files[dateID].name)
	}
	if len(stub.children[dateID]) != 1 || stub.files[stub.children[dateID][0]].name != "media" {
		t.Fatalf("date children = %v, want the media folder", stub.children[dateID])
	}
	mediaID := stub.children[dateID][0]
	if len(stub.children[mediaID]) != 1 || stub.files[stub.children[mediaID][0]].name != "a.jpg" {
		t.Fatalf("media children = %v, want a.jpg", stub.children[mediaID])
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
	gotURL, err := u.ExistingURL(ctx, "20261111/media/a.jpg")
	if err != nil || gotURL != url {
		t.Fatalf("ExistingURL = %q, %v (upload returned %q)", gotURL, err, url)
	}
	// A second upload of the same path is a plain upload (the caller
	// is responsible for the Exists check); it must not error.
	if _, err := u.Upload(ctx, local, "20261111/media/a.jpg"); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	// The client used multipart upload for this small file.
	sawMultipart := false
	for _, req := range stub.requests {
		if strings.Contains(req, "uploadType=multipart") {
			sawMultipart = true
		}
	}
	if !sawMultipart {
		t.Fatalf("no multipart upload seen; requests = %v", stub.requests)
	}
}

func TestDriveUploaderResolveRootByPath(t *testing.T) {
	ctx := context.Background()
	stub := newStubDrive()
	svc := stubDriveService(t, stub)
	u := NewDriveUploaderWithService(svc, "")
	// resolveRoot with a folder path creates the chain from "root".
	rootID, err := u.resolveRoot(ctx, DriveUploadConfig{FolderPath: "Evidence/2026"})
	if err != nil {
		t.Fatal(err)
	}
	if rootID == "" || rootID == "root" {
		t.Fatalf("rootID = %q", rootID)
	}
	// Evidence under root, 2026 under Evidence.
	if len(stub.children["root"]) != 1 || stub.files[stub.children["root"][0]].name != "Evidence" {
		t.Fatalf("root children = %v", stub.children["root"])
	}
	evID := stub.children["root"][0]
	if len(stub.children[evID]) != 1 || stub.files[stub.children[evID][0]].name != "2026" {
		t.Fatalf("Evidence children = %v", stub.children[evID])
	}
	// Resolving the same path again reuses the folders (cache + list).
	rootID2, err := u.resolveRoot(ctx, DriveUploadConfig{FolderPath: "Evidence/2026"})
	if err != nil || rootID2 != rootID {
		t.Fatalf("second resolve = %q, %v (first %q)", rootID2, err, rootID)
	}
	if len(stub.children["root"]) != 1 {
		t.Fatalf("duplicate Evidence folder created: %v", stub.children["root"])
	}
}

func TestDriveUploaderResolveRootByID(t *testing.T) {
	ctx := context.Background()
	stub := newStubDrive()
	root := &stubDriveFile{id: "root-id", name: "Evidence", mimeType: driveFolderMimeType}
	stub.add(root)
	// A non-folder with that ID must be rejected.
	notFolder := &stubDriveFile{id: "file-id", name: "x.txt", mimeType: "text/plain"}
	stub.add(notFolder)

	svc := stubDriveService(t, stub)
	u := NewDriveUploaderWithService(svc, "")
	rootID, err := u.resolveRoot(ctx, DriveUploadConfig{FolderID: "root-id"})
	if err != nil || rootID != "root-id" {
		t.Fatalf("resolveRoot = %q, %v", rootID, err)
	}
	if _, err := u.resolveRoot(ctx, DriveUploadConfig{FolderID: "file-id"}); err == nil {
		t.Fatal("non-folder root accepted")
	}
	if _, err := u.resolveRoot(ctx, DriveUploadConfig{FolderID: "missing-id"}); err == nil {
		t.Fatal("missing root accepted")
	}
}

func TestDriveUploaderNameEscaping(t *testing.T) {
	// A file name with a quote must not break the list query.
	ctx := context.Background()
	stub := newStubDrive()
	root := &stubDriveFile{id: "root-id", name: "Evidence", mimeType: driveFolderMimeType}
	stub.add(root)
	svc := stubDriveService(t, stub)
	u := NewDriveUploaderWithService(svc, "root-id")

	dir := t.TempDir()
	local := filepath.Join(dir, "it's.jpg")
	if err := os.WriteFile(local, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Upload(ctx, local, "20261111/media/it's.jpg"); err != nil {
		t.Fatal(err)
	}
	exists, err := u.Exists(ctx, "20261111/media/it's.jpg", 1)
	if err != nil || !exists {
		t.Fatalf("Exists(quoted name) = %v, %v", exists, err)
	}
}
