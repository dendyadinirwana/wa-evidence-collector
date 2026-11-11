package collector

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeUploader is an in-memory Uploader for tests: it records every
// Upload call, can be told to fail the first N calls for a path
// (transient failure), and keeps a set of "already present" files with
// sizes for the Exists check.
type fakeUploader struct {
	uploaded  map[string]string // remotePath -> localPath
	uploads   []string          // remotePaths in call order
	failFirst map[string]int    // remotePath -> failures remaining
	present   map[string]int64  // remotePath -> size
	existing  map[string]string // remotePath -> URL to return for ExistingURL
	uploadErr error             // when set, every Upload fails with this
	existsErr error             // when set, every Exists fails with this
}

func newFakeUploader() *fakeUploader {
	return &fakeUploader{
		uploaded:  map[string]string{},
		failFirst: map[string]int{},
		present:   map[string]int64{},
		existing:  map[string]string{},
	}
}

func (f *fakeUploader) Name() string { return "fake" }

func (f *fakeUploader) Upload(ctx context.Context, localPath, remotePath string) (string, error) {
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	if n, ok := f.failFirst[remotePath]; ok && n > 0 {
		f.failFirst[remotePath] = n - 1
		return "", fmt.Errorf("fake transient failure for %s", remotePath)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return "", err
	}
	f.uploads = append(f.uploads, remotePath)
	f.uploaded[remotePath] = localPath
	f.present[remotePath] = info.Size()
	return "https://fake.example/" + remotePath, nil
}

func (f *fakeUploader) Exists(ctx context.Context, remotePath string, size int64) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	s, ok := f.present[remotePath]
	return ok && s == size, nil
}

func (f *fakeUploader) ExistingURL(ctx context.Context, remotePath string) (string, error) {
	if url, ok := f.existing[remotePath]; ok {
		return url, nil
	}
	return "https://fake.example/" + remotePath, nil
}

func TestUploadOneIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(local, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	fu := newFakeUploader()
	// Pretend the file is already remote with the same size.
	fu.present["20261111/media/a.txt"] = 5
	fu.existing["20261111/media/a.txt"] = "https://fake.example/existing-a"

	url, err := uploadOne(ctx, fu, local, "20261111/media/a.txt", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://fake.example/existing-a" {
		t.Fatalf("url = %q, want the existing URL", url)
	}
	if len(fu.uploads) != 0 {
		t.Fatalf("uploads = %v, want none (idempotent skip)", fu.uploads)
	}
	// Same name, different size: must upload.
	fu.present["20261111/media/b.txt"] = 999
	url, err = uploadOne(ctx, fu, local, "20261111/media/b.txt", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://fake.example/20261111/media/b.txt" {
		t.Fatalf("url = %q", url)
	}
	if len(fu.uploads) != 1 {
		t.Fatalf("uploads = %v, want 1", fu.uploads)
	}
}

func TestUploadOneRetriesThenSucceeds(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(local, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	fu := newFakeUploader()
	fu.failFirst["20261111/media/a.txt"] = 2
	url, err := uploadOne(ctx, fu, local, "20261111/media/a.txt", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://fake.example/20261111/media/a.txt" {
		t.Fatalf("url = %q", url)
	}
}

func TestUploadOneFailsAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(local, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	fu := newFakeUploader()
	fu.failFirst["20261111/media/a.txt"] = 99
	if _, err := uploadOne(ctx, fu, local, "20261111/media/a.txt", 3, 0); err == nil {
		t.Fatal("expected error after max attempts")
	}
	// An Exists error is fatal immediately (not retried as an upload).
	fu2 := newFakeUploader()
	fu2.existsErr = fmt.Errorf("boom")
	if _, err := uploadOne(ctx, fu2, local, "20261111/media/a.txt", 3, 0); err == nil {
		t.Fatal("expected error from Exists")
	}
}

func TestRemotePaths(t *testing.T) {
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	if got := remoteMediaPath(day, "x.jpg"); got != "20261111/media/x.jpg" {
		t.Fatalf("remoteMediaPath = %q", got)
	}
	if got := remoteDocPath(day, "y.xlsx"); got != "20261111/dokumen/y.xlsx" {
		t.Fatalf("remoteDocPath = %q", got)
	}
	segs, name := splitRemotePath("20261111/media/x.jpg")
	if fmt.Sprint(segs) != "[20261111 media]" || name != "x.jpg" {
		t.Fatalf("splitRemotePath = %v %q", segs, name)
	}
	segs, name = splitRemotePath("Rekap-Evidence-202611-gabungan.xlsx")
	if len(segs) != 0 || name != "Rekap-Evidence-202611-gabungan.xlsx" {
		t.Fatalf("splitRemotePath = %v %q", segs, name)
	}
}

func TestUploadConfigValidation(t *testing.T) {
	// Disabled: nothing is validated.
	cfg := defaultConfig()
	cfg.Collection.Allowlist = []string{groupA}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	// Enabled with an unknown backend.
	bad := defaultConfig()
	bad.Collection.Allowlist = []string{groupA}
	bad.Upload.Enabled = true
	bad.Upload.Backend = "ftp"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown backend accepted")
	}
	// Drive without credentials file.
	bad = defaultConfig()
	bad.Collection.Allowlist = []string{groupA}
	bad.Upload.Enabled = true
	bad.Upload.Backend = "drive"
	bad.Upload.Drive.FolderID = "00000000000000000000000000000000"
	if err := bad.Validate(); err == nil {
		t.Fatal("drive without credentials_file accepted")
	}
	// Drive with a missing credentials file.
	bad.Upload.Drive.CredentialsFile = filepath.Join(t.TempDir(), "nope.json")
	if err := bad.Validate(); err == nil {
		t.Fatal("drive with missing credentials file accepted")
	}
	// Drive with credentials file but no root folder.
	cred := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(cred, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	bad.Upload.Drive.CredentialsFile = cred
	bad.Upload.Drive.FolderID = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("drive without folder_id/folder_path accepted")
	}
	// Drive valid.
	bad.Upload.Drive.FolderID = "00000000000000000000000000000000"
	if err := bad.Validate(); err != nil {
		t.Fatalf("valid drive config rejected: %v", err)
	}
	// S3 without bucket / endpoint.
	bad = defaultConfig()
	bad.Collection.Allowlist = []string{groupA}
	bad.Upload.Enabled = true
	bad.Upload.Backend = "s3"
	if err := bad.Validate(); err == nil {
		t.Fatal("s3 without bucket accepted")
	}
	bad.Upload.S3.Bucket = "wa-evidence-example"
	if err := bad.Validate(); err == nil {
		t.Fatal("s3 without endpoint accepted")
	}
	bad.Upload.S3.Endpoint = "https://000000000000.r2.cloudflarestorage.com"
	if err := bad.Validate(); err != nil {
		t.Fatalf("valid s3 config rejected: %v", err)
	}
	// Max attempts < 1.
	bad.Upload.MaxAttempts = 0
	if err := bad.Validate(); err == nil {
		t.Fatal("max_attempts 0 accepted")
	}
	// BuildUploader: disabled -> (nil, nil).
	u, err := BuildUploader(context.Background(), defaultConfig().Upload)
	if err != nil || u != nil {
		t.Fatalf("disabled BuildUploader = %v, %v", u, err)
	}
	// BuildUploader: unknown backend -> error.
	ucfg := defaultConfig().Upload
	ucfg.Enabled = true
	ucfg.Backend = "ftp"
	if _, err := BuildUploader(context.Background(), ucfg); err == nil {
		t.Fatal("unknown backend accepted by BuildUploader")
	}
}

func TestCollectWithFakeUpload(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedStore(t, base)
	outDir := t.TempDir()

	mediaSrc := filepath.Join(t.TempDir(), "image_20261111_110000_m2.jpg")
	if err := os.WriteFile(mediaSrc, []byte("fake-jpeg-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	srv := stubBridge(t, mediaSrc, false)
	defer srv.Close()

	cfg := testConfig(t, dbPath, srv.URL+"/api", outDir)
	coll, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	fu := newFakeUploader()
	coll.uploader = fu // inject the fake (upload enabled at runtime only)

	sum, err := coll.Collect(context.Background(), "test", base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	// 3 rows: m1 text (render), m2 image (media), m3 text (render).
	if sum.UploadOK == 0 || sum.UploadFailed != 0 {
		t.Fatalf("upload counts = ok %d failed %d skipped %d", sum.UploadOK, sum.UploadFailed, sum.UploadSkipped)
	}
	// Evidence files (1 media + 2 renders) + export files (json, csv,
	// xlsx, agenda json, manifest) + monthly file = 9 uploads.
	if sum.UploadOK != 9 {
		t.Fatalf("UploadOK = %d, want 9 (3 evidence + 6 exports)", sum.UploadOK)
	}
	if len(sum.UploadedFiles) != 9 {
		t.Fatalf("UploadedFiles = %d entries, want 9", len(sum.UploadedFiles))
	}
	// Remote layout: evidence under <date>/media/, exports under
	// <date>/dokumen/, monthly at the root.
	for remote := range fu.uploaded {
		if strings.Contains(remote, "media/") && !strings.HasPrefix(remote, "20261111/media/") {
			t.Fatalf("media remote path = %q", remote)
		}
		if strings.Contains(remote, "dokumen/") && !strings.HasPrefix(remote, "20261111/dokumen/") {
			t.Fatalf("dokumen remote path = %q", remote)
		}
	}
	if _, ok := fu.uploaded["Rekap-Evidence-202611-gabungan.xlsx"]; !ok {
		t.Fatalf("monthly file not uploaded at root: %v", fu.uploads)
	}

	// JSON: every row carries its evidence URL in link_drive.
	raw, err := os.ReadFile(filepath.Join(sum.RunDir, "evidence-120000.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env EvidenceFile
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Version != 4 {
		t.Fatalf("envelope version = %d, want 4 (sources/merged)", env.Version)
	}
	for _, r := range env.Rows {
		if r.LinkDrive == "" || !strings.HasPrefix(r.LinkDrive, "https://fake.example/20261111/media/") {
			t.Fatalf("row %s link_drive = %q", r.MessageID, r.LinkDrive)
		}
		if r.MediaType == "image" && !strings.Contains(r.LinkDrive, "image_") {
			t.Fatalf("media row link = %q", r.LinkDrive)
		}
		if r.MediaType == "" && !strings.HasSuffix(r.LinkDrive, "_chat.png") {
			t.Fatalf("render row link = %q", r.LinkDrive)
		}
	}

	// CSV: link_drive is the last column and holds the same URLs.
	csvRaw, err := os.ReadFile(filepath.Join(sum.RunDir, "evidence-120000.csv"))
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(strings.NewReader(string(csvRaw))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if recs[0][len(recs[0])-1] != "link_drive" {
		t.Fatalf("csv header = %v", recs[0])
	}
	for _, rec := range recs[1:] {
		if !strings.HasPrefix(rec[len(rec)-1], "https://fake.example/20261111/media/") {
			t.Fatalf("csv link_drive = %q", rec[len(rec)-1])
		}
	}

	// Excel: Link Drive column (index 8) holds the same URLs.
	_, xrows, err := ReadRecapXLSX(filepath.Join(sum.RunDir, "Rekap-Evidence-20261111.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	if len(xrows) != 3 {
		t.Fatalf("xlsx rows = %d", len(xrows))
	}
	for _, r := range xrows {
		if !strings.HasPrefix(r[8], "https://fake.example/20261111/media/") {
			t.Fatalf("xlsx Link Drive = %q", r[8])
		}
	}
	// The monthly file (rebuilt from the daily file) carries them too.
	_, mrows, err := ReadRecapXLSX(filepath.Join(outDir, "Rekap-Evidence-202611-gabungan.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range mrows {
		if !strings.HasPrefix(r[8], "https://fake.example/20261111/media/") {
			t.Fatalf("monthly Link Drive = %q", r[8])
		}
	}
}

func TestCollectUploadFailureNonFatal(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedStore(t, base)
	outDir := t.TempDir()

	mediaSrc := filepath.Join(t.TempDir(), "image_20261111_110000_m2.jpg")
	if err := os.WriteFile(mediaSrc, []byte("fake-jpeg-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	srv := stubBridge(t, mediaSrc, false)
	defer srv.Close()

	cfg := testConfig(t, dbPath, srv.URL+"/api", outDir)
	cfg.Upload.MaxAttempts = 2
	cfg.Upload.RetryDelaySeconds = 0 // keep the failure test fast
	coll, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	fu := newFakeUploader()
	fu.uploadErr = fmt.Errorf("remote is down")
	coll.uploader = fu

	// Non-strict (default): the run still succeeds; links stay empty.
	sum, err := coll.Collect(context.Background(), "test", base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatalf("non-strict upload failure failed the run: %v", err)
	}
	if sum.UploadFailed == 0 {
		t.Fatalf("UploadFailed = 0, want > 0")
	}
	raw, _ := os.ReadFile(filepath.Join(sum.RunDir, "evidence-120000.json"))
	var env EvidenceFile
	_ = json.Unmarshal(raw, &env)
	for _, r := range env.Rows {
		if r.LinkDrive != "" {
			t.Fatalf("row %s link_drive = %q, want empty", r.MessageID, r.LinkDrive)
		}
	}
	// Local exports still exist.
	if _, err := os.Stat(filepath.Join(sum.RunDir, "Rekap-Evidence-20261111.xlsx")); err != nil {
		t.Fatalf("daily xlsx missing after failed upload: %v", err)
	}
}

func TestCollectUploadStrictFailsRun(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedStore(t, base)
	outDir := t.TempDir()

	mediaSrc := filepath.Join(t.TempDir(), "image_20261111_110000_m2.jpg")
	if err := os.WriteFile(mediaSrc, []byte("fake-jpeg-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	srv := stubBridge(t, mediaSrc, false)
	defer srv.Close()

	cfg := testConfig(t, dbPath, srv.URL+"/api", outDir)
	cfg.Upload.Strict = true
	cfg.Upload.MaxAttempts = 2
	cfg.Upload.RetryDelaySeconds = 0 // keep the failure test fast
	coll, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	fu := newFakeUploader()
	fu.uploadErr = fmt.Errorf("remote is down")
	coll.uploader = fu

	if _, err := coll.Collect(context.Background(), "test", base.Add(-3*time.Hour), base); err == nil {
		t.Fatal("strict upload failure did not fail the run")
	}
}

func TestCollectUploadDisabledStaysLocal(t *testing.T) {
	// No uploader injected (the default): Link Drive stays empty and
	// the run is identical to v0.3.0.
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedStore(t, base)
	outDir := t.TempDir()

	mediaSrc := filepath.Join(t.TempDir(), "image_20261111_110000_m2.jpg")
	if err := os.WriteFile(mediaSrc, []byte("fake-jpeg-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	srv := stubBridge(t, mediaSrc, false)
	defer srv.Close()

	cfg := testConfig(t, dbPath, srv.URL+"/api", outDir)
	coll, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := coll.Collect(context.Background(), "test", base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	if sum.UploadOK != 0 || sum.UploadFailed != 0 || len(sum.UploadedFiles) != 0 {
		t.Fatalf("upload counts with upload disabled = %+v", sum)
	}
	raw, _ := os.ReadFile(filepath.Join(sum.RunDir, "evidence-120000.json"))
	var env EvidenceFile
	_ = json.Unmarshal(raw, &env)
	for _, r := range env.Rows {
		if r.LinkDrive != "" {
			t.Fatalf("row %s link_drive = %q, want empty", r.MessageID, r.LinkDrive)
		}
	}
	_, xrows, err := ReadRecapXLSX(filepath.Join(sum.RunDir, "Rekap-Evidence-20261111.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range xrows {
		if r[8] != "" {
			t.Fatalf("xlsx Link Drive = %q, want empty", r[8])
		}
	}
}
