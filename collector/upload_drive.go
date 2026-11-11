package collector

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Google Drive backend for remote upload.
//
// Auth: a credentials file (service-account JSON or OAuth
// authorized-user JSON) given by DriveUploadConfig.CredentialsFile.
// The file is read by the Google client library; it is never logged
// and must never be committed (see .gitignore: credentials/).
//
// Layout: the remote root is a Drive folder (by folder ID, or by path
// resolved/created from the Drive root). Under it the collector mirrors
// the local layout: <root>/<YYYYMMDD>/media/<evidence> and
// <root>/<YYYYMMDD>/dokumen/<exports>.
//
// Idempotency: before uploading, the backend lists the target folder
// for a file with the same name and compares sizes. Same name + same
// size = already uploaded, return its webViewLink without uploading.
// This compares name + size only, NOT a content hash (Drive does store
// an md5Checksum, but the interface contract is name + size; see
// upload.go). A different file with the same name and size would be
// mistaken for the same file; file names embed message IDs and
// timestamps, so that is unlikely in practice.
//
// Sharing: uploaded files are NOT shared. The returned webViewLink
// opens the file for the credentials' owner; share it from Drive if
// others need access.

const driveFolderMimeType = "application/vnd.google-apps.folder"

// DriveUploader is a Uploader backed by Google Drive.
type DriveUploader struct {
	svc     *drive.Service
	rootID  string
	folders map[string]string // parentID + "/" + name -> folder ID (cache)
}

// NewDriveUploader builds a Drive uploader from config: it loads the
// credentials file, verifies the root folder exists, and resolves a
// folder path to an ID when FolderID is empty.
func NewDriveUploader(ctx context.Context, cfg DriveUploadConfig) (*DriveUploader, error) {
	svc, err := drive.NewService(ctx, option.WithCredentialsFile(cfg.CredentialsFile))
	if err != nil {
		return nil, fmt.Errorf("drive client (credentials %s): %w", cfg.CredentialsFile, err)
	}
	u := &DriveUploader{svc: svc, folders: map[string]string{}}
	rootID, err := u.resolveRoot(ctx, cfg)
	if err != nil {
		return nil, err
	}
	u.rootID = rootID
	return u, nil
}

// NewDriveUploaderWithService builds a Drive uploader around an
// already-constructed service (used by tests with a stub endpoint and
// no auth). rootID is the remote root folder ID.
func NewDriveUploaderWithService(svc *drive.Service, rootID string) *DriveUploader {
	return &DriveUploader{svc: svc, rootID: rootID, folders: map[string]string{}}
}

// Name is the backend name, for logs.
func (u *DriveUploader) Name() string { return "drive" }

// resolveRoot returns the remote root folder ID: FolderID when set
// (verified to exist and be a folder), otherwise FolderPath resolved
// (and created) segment by segment from the Drive root.
func (u *DriveUploader) resolveRoot(ctx context.Context, cfg DriveUploadConfig) (string, error) {
	if cfg.FolderID != "" {
		f, err := u.svc.Files.Get(cfg.FolderID).Fields("id,mimeType,name").Context(ctx).Do()
		if err != nil {
			return "", fmt.Errorf("drive root folder %q: %w", cfg.FolderID, err)
		}
		if f.MimeType != driveFolderMimeType {
			return "", fmt.Errorf("drive root folder %q is not a folder (mimeType %s)", cfg.FolderID, f.MimeType)
		}
		return f.Id, nil
	}
	parentID := "root"
	for _, seg := range strings.Split(cfg.FolderPath, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		id, err := u.folderID(ctx, parentID, seg)
		if err != nil {
			return "", fmt.Errorf("drive root path %q: %w", cfg.FolderPath, err)
		}
		parentID = id
	}
	return parentID, nil
}

// folderID returns the ID of the folder named name under parentID,
// creating it when it does not exist. Results are cached per run.
func (u *DriveUploader) folderID(ctx context.Context, parentID, name string) (string, error) {
	key := parentID + "/" + name
	if id, ok := u.folders[key]; ok {
		return id, nil
	}
	found, err := u.findFile(ctx, parentID, name)
	if err != nil {
		return "", err
	}
	if found != nil {
		if found.MimeType != driveFolderMimeType {
			return "", fmt.Errorf("drive: %q under %q exists but is not a folder", name, parentID)
		}
		u.folders[key] = found.Id
		return found.Id, nil
	}
	created, err := u.svc.Files.Create(&drive.File{
		Name:     name,
		MimeType: driveFolderMimeType,
		Parents:  []string{parentID},
	}).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("drive: create folder %q: %w", name, err)
	}
	u.folders[key] = created.Id
	return created.Id, nil
}

// findFile returns the (first) non-trashed file named name directly
// under folderID, or nil when there is none.
func (u *DriveUploader) findFile(ctx context.Context, folderID, name string) (*drive.File, error) {
	q := fmt.Sprintf("name = '%s' and '%s' in parents and trashed = false", driveEscape(name), folderID)
	list, err := u.svc.Files.List().Q(q).Fields("files(id,name,size,webViewLink,mimeType)").PageSize(10).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("drive: list %q in %q: %w", name, folderID, err)
	}
	if len(list.Files) == 0 {
		return nil, nil
	}
	return list.Files[0], nil
}

// targetFolder resolves the folder chain of remotePath (all segments
// except the last) under the root and returns the folder ID plus the
// file name.
func (u *DriveUploader) targetFolder(ctx context.Context, remotePath string) (string, string, error) {
	segments, name := splitRemotePath(remotePath)
	if name == "" {
		return "", "", fmt.Errorf("drive: remote path %q has no file name", remotePath)
	}
	parentID := u.rootID
	for _, seg := range segments {
		id, err := u.folderID(ctx, parentID, seg)
		if err != nil {
			return "", "", err
		}
		parentID = id
	}
	return parentID, name, nil
}

// Upload uploads localPath to remotePath (folders created as needed)
// and returns the file's webViewLink.
func (u *DriveUploader) Upload(ctx context.Context, localPath, remotePath string) (string, error) {
	folderID, name, err := u.targetFolder(ctx, remotePath)
	if err != nil {
		return "", err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("drive: open %s: %w", localPath, err)
	}
	defer f.Close()
	contentType := mime.TypeByExtension(filepath.Ext(localPath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	created, err := u.svc.Files.Create(&drive.File{
		Name:    name,
		Parents: []string{folderID},
	}).Media(f, googleapi.ContentType(contentType)).Fields("id,name,size,webViewLink").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("drive: upload %q: %w", remotePath, err)
	}
	if created.WebViewLink != "" {
		return created.WebViewLink, nil
	}
	// Fallback when the API does not return a view link.
	return "https://drive.google.com/file/d/" + created.Id + "/view", nil
}

// Exists reports whether remotePath already exists with the given size.
func (u *DriveUploader) Exists(ctx context.Context, remotePath string, size int64) (bool, error) {
	folderID, name, err := u.targetFolder(ctx, remotePath)
	if err != nil {
		return false, err
	}
	found, err := u.findFile(ctx, folderID, name)
	if err != nil {
		return false, err
	}
	return found != nil && found.Size == size, nil
}

// ExistingURL returns the webViewLink of the already-present file at
// remotePath.
func (u *DriveUploader) ExistingURL(ctx context.Context, remotePath string) (string, error) {
	folderID, name, err := u.targetFolder(ctx, remotePath)
	if err != nil {
		return "", err
	}
	found, err := u.findFile(ctx, folderID, name)
	if err != nil {
		return "", err
	}
	if found == nil {
		return "", fmt.Errorf("drive: %q not found", remotePath)
	}
	if found.WebViewLink != "" {
		return found.WebViewLink, nil
	}
	return "https://drive.google.com/file/d/" + found.Id + "/view", nil
}

// driveEscape escapes a value for a Drive search query: a literal
// single quote is written as two single quotes.
func driveEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
