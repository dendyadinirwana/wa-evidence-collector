package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3-compatible backend for remote upload (AWS S3, MinIO, Cloudflare
// R2, ...).
//
// Auth: the default AWS credential chain when AccessKey/SecretKey are
// empty (environment AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY,
// shared config files, ...); static credentials from the config file
// when both are set. Config-file credentials must never be committed
// (see .gitignore: credentials/).
//
// Layout: keys are <prefix>/<YYYYMMDD>/media/<evidence> and
// <prefix>/<YYYYMMDD>/dokumen/<exports> (prefix empty = no prefix).
//
// Idempotency: before uploading, the backend HEADs the key and
// compares ContentLength. Same key + same size = already uploaded,
// return its URL without uploading. This compares key + size only,
// NOT a content hash; a different object with the same key and size
// would be mistaken for the same object. Keys embed message IDs and
// timestamps, so that is unlikely in practice.
//
// URLs: the returned URL is PublicURLBase + "/" + key when configured,
// otherwise endpoint + "/" + bucket + "/" + key (path style). For a
// private bucket this URL is only a pattern: it needs signing or a
// public-read setup to open. The backend never changes bucket policy
// or object ACLs.

// S3Uploader is a Uploader backed by S3-compatible storage.
type S3Uploader struct {
	client     *s3.Client
	bucket     string
	prefix     string
	endpoint   string
	publicBase string
	pathStyle  bool
}

// NewS3Uploader builds an S3 uploader from config: region, credentials
// (static from config or the default chain), and a custom endpoint
// resolver for S3-compatible endpoints.
func NewS3Uploader(ctx context.Context, cfg S3UploadConfig) (*S3Uploader, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	}
	if cfg.Endpoint != "" {
		endpoint := cfg.Endpoint
		opts = append(opts, config.WithEndpointResolverWithOptions(
			aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: endpoint, SigningRegion: region}, nil
			})))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3 config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.UsePathStyle
	})
	return &S3Uploader{
		client:     client,
		bucket:     cfg.Bucket,
		prefix:     strings.Trim(cfg.Prefix, "/"),
		endpoint:   strings.TrimRight(cfg.Endpoint, "/"),
		publicBase: strings.TrimRight(cfg.PublicURLBase, "/"),
		pathStyle:  cfg.UsePathStyle,
	}, nil
}

// NewS3UploaderWithClient builds an S3 uploader around an
// already-constructed client (used by tests with a stub endpoint).
func NewS3UploaderWithClient(client *s3.Client, bucket, prefix, endpoint, publicBase string, pathStyle bool) *S3Uploader {
	return &S3Uploader{
		client:     client,
		bucket:     bucket,
		prefix:     strings.Trim(prefix, "/"),
		endpoint:   strings.TrimRight(endpoint, "/"),
		publicBase: strings.TrimRight(publicBase, "/"),
		pathStyle:  pathStyle,
	}
}

// Name is the backend name, for logs.
func (u *S3Uploader) Name() string { return "s3" }

// key returns the object key for a remote path, with the prefix.
func (u *S3Uploader) key(remotePath string) string {
	remotePath = strings.Trim(remotePath, "/")
	if u.prefix == "" {
		return remotePath
	}
	return u.prefix + "/" + remotePath
}

// url returns the object URL for a key.
func (u *S3Uploader) url(key string) string {
	if u.publicBase != "" {
		return u.publicBase + "/" + key
	}
	if u.pathStyle {
		return u.endpoint + "/" + u.bucket + "/" + key
	}
	// Virtual-host style: https://<bucket>.<host>/<key>.
	ep := strings.TrimPrefix(strings.TrimPrefix(u.endpoint, "https://"), "http://")
	scheme := "https://"
	if strings.HasPrefix(u.endpoint, "http://") {
		scheme = "http://"
	}
	return scheme + u.bucket + "." + ep + "/" + key
}

// Upload uploads localPath to remotePath (as an object key) and returns
// its URL.
func (u *S3Uploader) Upload(ctx context.Context, localPath, remotePath string) (string, error) {
	key := u.key(remotePath)
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("s3: open %s: %w", localPath, err)
	}
	defer f.Close()
	_, err = u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
		Body:   f,
	})
	if err != nil {
		return "", fmt.Errorf("s3: put %q: %w", key, err)
	}
	return u.url(key), nil
}

// Exists reports whether remotePath already exists with the given size
// (HEAD + ContentLength comparison). A missing key is not an error.
func (u *S3Uploader) Exists(ctx context.Context, remotePath string, size int64) (bool, error) {
	key := u.key(remotePath)
	out, err := u.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NoSuchKey
		if errors.As(err, &notFound) {
			return false, nil
		}
		// The SDK also surfaces a 404 as a generic API error; treat
		// any 404 as "not found" so a stub or proxy that does not
		// return the typed NoSuchKey still works.
		var apiErr interface{ ErrorCode() string }
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NotFound" {
			return false, nil
		}
		return false, fmt.Errorf("s3: head %q: %w", key, err)
	}
	return out.ContentLength != nil && *out.ContentLength == size, nil
}

// ExistingURL returns the URL of the already-present object at
// remotePath (no request needed: the URL is derived from the key).
func (u *S3Uploader) ExistingURL(ctx context.Context, remotePath string) (string, error) {
	return u.url(u.key(remotePath)), nil
}
