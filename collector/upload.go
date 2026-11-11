package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Remote upload of a run's exports.
//
// Design: one Uploader interface, two backends (Google Drive and
// S3-compatible). Both are OFF by default; local-only remains the
// default. Upload runs after the rows are built but BEFORE the export
// files are written, so the per-row remote URLs land in the final
// JSON/CSV/Excel (Link Drive column). The export files themselves
// (JSON, CSV, XLSX, manifest, monthly recap) are uploaded afterwards;
// their URLs are recorded in the run Summary and logged.
//
// Remote layout mirrors the local one under the configured root/prefix:
//
//	<root>/<YYYYMMDD>/media/<evidence files>     (downloaded media + chat renders)
//	<root>/<YYYYMMDD>/dokumen/<export files>      (evidence-*.json/.csv, recap xlsx, manifest)
//	<root>/Rekap-Evidence-<YYYYMM>-gabungan.xlsx (monthly recap, at the root)
//
// Idempotency: before uploading, the backend checks whether a file with
// the same remote path already exists with the same size; if so the
// upload is skipped and the existing URL is returned. This compares
// name + size only, NOT a content hash: a different file with the same
// name and size would be mistaken for the same file. In practice the
// file names embed message IDs and timestamps, so collisions are
// unlikely, but a re-run that regenerates a file with different content
// at the same size would not be re-uploaded.
//
// Retries: each file is attempted up to Upload.MaxAttempts times with
// Upload.RetryDelaySeconds between attempts. A failure is logged,
// counted, and (unless Strict) leaves the row's link empty and the
// local file in place; the run continues.

// Uploader uploads one local file to a remote path (relative to the
// configured root/prefix, slash-separated) and returns the URL to
// record for it (a view URL for Drive, an object URL for S3).
type Uploader interface {
	// Upload uploads localPath to remotePath and returns its URL.
	// Upload may assume the file does not exist yet (the caller
	// checks Exists first); overwriting an identical file is
	// acceptable but must not create a duplicate.
	Upload(ctx context.Context, localPath, remotePath string) (string, error)
	// Exists reports whether remotePath already exists with the given
	// size in bytes (idempotency check). A backend error is returned
	// as an error, not as "does not exist".
	Exists(ctx context.Context, remotePath string, size int64) (bool, error)
	// ExistingURL returns the URL of a remote file that already
	// exists (after Exists returned true), without uploading.
	ExistingURL(ctx context.Context, remotePath string) (string, error)
	// Name is the backend name ("drive" or "s3"), for logs.
	Name() string
}

// BuildUploader builds the configured uploader. It returns (nil, nil)
// when upload is disabled (local-only). The caller must have validated
// the config first.
func BuildUploader(ctx context.Context, cfg UploadConfig) (Uploader, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	switch cfg.Backend {
	case "drive":
		return NewDriveUploader(ctx, cfg.Drive)
	case "s3":
		return NewS3Uploader(ctx, cfg.S3)
	default:
		return nil, fmt.Errorf("upload.backend %q: want \"drive\" or \"s3\"", cfg.Backend)
	}
}

// uploadOne uploads a single file with retries. It first checks
// Exists: when the remote file is already there with the same size it
// returns the existing URL without uploading. On success it returns the
// remote URL; on failure after all attempts it returns the last error.
func uploadOne(ctx context.Context, u Uploader, localPath, remotePath string, maxAttempts, retryDelaySeconds int) (string, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", localPath, err)
	}
	exists, err := u.Exists(ctx, remotePath, info.Size())
	if err != nil {
		return "", fmt.Errorf("exists check %s: %w", remotePath, err)
	}
	if exists {
		// Already there with the same size: return its URL without
		// uploading again.
		url, err := u.ExistingURL(ctx, remotePath)
		if err != nil {
			return "", fmt.Errorf("existing url %s: %w", remotePath, err)
		}
		return url, nil
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		url, err := u.Upload(ctx, localPath, remotePath)
		if err == nil {
			return url, nil
		}
		lastErr = err
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(retryDelaySeconds) * time.Second):
			}
		}
	}
	return "", fmt.Errorf("upload %s after %d attempts: %w", remotePath, maxAttempts, lastErr)
}

// remoteDateDir is the remote directory for a run date: "<YYYYMMDD>".
func remoteDateDir(t time.Time) string { return t.Format("20060102") }

// remoteMediaPath is the remote path of an evidence file in media/.
func remoteMediaPath(t time.Time, fileName string) string {
	return remoteDateDir(t) + "/media/" + fileName
}

// remoteDocPath is the remote path of an export file in dokumen/.
func remoteDocPath(t time.Time, fileName string) string {
	return remoteDateDir(t) + "/dokumen/" + fileName
}

// splitRemotePath splits a slash-separated remote path into its folder
// segments and the file name. "20261111/media/a.jpg" ->
// (["20261111", "media"], "a.jpg").
func splitRemotePath(remotePath string) ([]string, string) {
	parts := strings.Split(strings.Trim(remotePath, "/"), "/")
	if len(parts) == 0 {
		return nil, ""
	}
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// fileBase is filepath.Base, kept as a tiny helper for tests.
func fileBase(path string) string { return filepath.Base(path) }
