// Package collector reads the WhatsApp bridge SQLite store (messages.db) and
// exports allowlisted chats as evidence: JSON + CSV rows, downloaded media,
// and a SHA256 manifest per run.
//
// Design notes:
//   - Read-only: the store is opened with SQLite mode=ro. The collector never
//     writes to the bridge database.
//   - Allowlist-only: only chat JIDs listed in the config are read. Every
//     other chat in the store is ignored.
//   - Dedup: a run skips messages already recorded in the state file
//     (output_dir/state.json), which stores content hashes only, never
//     message text.
//   - Media goes through the bridge REST API (POST /api/download), which
//     owns the whatsmeow download + original-URL fallback + .enc decrypt.
//     The collector only copies the resulting file into the run folder.
//   - Privacy: with redact=true (the default), sender JIDs are replaced by a
//     stable pseudonym (first 8 hex chars of their SHA256) and phone-like
//     digit runs in message text are masked. Chat JIDs are kept in exports
//     as the evidence source identifier.
package collector

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// BridgeConfig describes how to reach the bridge.
type BridgeConfig struct {
	// Base URL of the bridge REST API, e.g. http://127.0.0.1:8080/api.
	APIURL string `yaml:"api_url"`
	// Path to the file holding the bridge bearer token (store/.bridge-token).
	TokenFile string `yaml:"token_file"`
	// Path to the bridge SQLite store (store/messages.db).
	DBPath string `yaml:"db_path"`
	// Path to the bridge binary. Used only when Autostart is true.
	Binary string `yaml:"binary"`
	// Start the bridge as a child process when true; otherwise assume it is
	// already running.
	Autostart bool `yaml:"autostart"`
	// Seconds to wait for /api/health to answer 200 before giving up and
	// continuing in degraded mode (DB reads still work; media downloads
	// will fail until the bridge is up).
	HealthTimeoutSeconds int `yaml:"health_timeout_seconds"`
}

// CollectionConfig describes what to collect and where to put it.
type CollectionConfig struct {
	// Output root. Each run writes to output_dir/YYYYMMDD/.
	OutputDir string `yaml:"output_dir"`
	// Chat JIDs to collect, e.g. 120363000000000000@g.us (group) or
	// 15551234567@s.whatsapp.net (1:1). Nothing else is read.
	Allowlist []string `yaml:"allowlist"`
	// Default lookback for --once runs, in hours.
	LookbackHours int `yaml:"lookback_hours"`
	// Mask sender identities and phone-like numbers in exports.
	Redact bool `yaml:"redact"`
}

// MediaConfig controls media downloads (via the bridge).
type MediaConfig struct {
	// Download media for messages that have any. Requires the bridge.
	Download bool `yaml:"download"`
	// Attempts per message before giving up (default 3).
	MaxAttempts int `yaml:"max_attempts"`
	// Seconds to wait between attempts.
	RetryDelaySeconds int `yaml:"retry_delay_seconds"`
}

// DriveUploadConfig is the Google Drive backend. Auth is a credentials
// file (service-account JSON or OAuth authorized-user JSON) given by
// path in config; the file itself is never committed (see .gitignore).
// Exactly one of FolderID / FolderPath selects the remote root folder.
type DriveUploadConfig struct {
	// Path to the service-account / OAuth JSON credentials file.
	CredentialsFile string `yaml:"credentials_file"`
	// Drive folder ID of the remote root (e.g. the Evidence folder).
	FolderID string `yaml:"folder_id"`
	// Alternative to FolderID: a folder path from the Drive root
	// (e.g. "Evidence"), resolved/created segment by segment.
	FolderPath string `yaml:"folder_path"`
}

// S3UploadConfig is the S3-compatible backend (AWS S3, MinIO,
// Cloudflare R2, ...). Auth comes from the environment by default
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY); AccessKey/SecretKey in
// the config file are a fallback and must never be committed.
type S3UploadConfig struct {
	// Custom endpoint, e.g. https://<account>.r2.cloudflarestorage.com
	// or http://127.0.0.1:9000 for MinIO. Empty = AWS S3 default.
	Endpoint string `yaml:"endpoint"`
	// Region (R2 uses "auto").
	Region string `yaml:"region"`
	// Bucket name.
	Bucket string `yaml:"bucket"`
	// Key prefix under the bucket, e.g. "evidence". May be empty.
	Prefix string `yaml:"prefix"`
	// Fallback credentials (prefer the environment variables).
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// Base URL used to build the returned object URL, e.g.
	// https://cdn.example.com. Empty = endpoint/bucket/key.
	PublicURLBase string `yaml:"public_url_base"`
	// Use path-style addressing (endpoint/bucket/key). Required for
	// MinIO and R2; leave true unless the endpoint needs virtual-host
	// style (bucket.endpoint/key).
	UsePathStyle bool `yaml:"use_path_style"`
}

// UploadConfig controls the optional remote upload of a run's exports.
// Both backends are OFF by default; local-only remains the default.
type UploadConfig struct {
	// Enable remote upload after a run. When false, nothing is
	// uploaded and the Link Drive column stays empty.
	Enabled bool `yaml:"enabled"`
	// Backend: "drive" or "s3".
	Backend string `yaml:"backend"`
	// Attempts per file before giving up (default 3).
	MaxAttempts int `yaml:"max_attempts"`
	// Seconds to wait between attempts.
	RetryDelaySeconds int `yaml:"retry_delay_seconds"`
	// When true, any upload failure fails the whole run. When false
	// (default), failures are logged, counted, and the local export
	// is kept; the row's link stays empty.
	Strict bool `yaml:"strict"`
	// Backend-specific settings.
	Drive DriveUploadConfig `yaml:"drive"`
	S3    S3UploadConfig    `yaml:"s3"`
}

// DedupConfig controls semantic dedup (the second pass after exact
// dedup by message ID + content hash). It groups messages that are the
// same event reworded or cross-posted, so one event exports as one row.
// Pure Go, offline: token-set overlap (Jaccard) plus shared key
// entities (links incl. Zoom IDs, explicit dates/times, media keys)
// plus time proximity. Merging is deliberately conservative.
type DedupConfig struct {
	// Enable the semantic pass. When false, every new message is its
	// own row (exact dedup still applies).
	Enabled bool `yaml:"enabled"`
	// Token overlap (Jaccard) at or above this merges two messages.
	Threshold float64 `yaml:"threshold"`
	// Token overlap at or above this (but below Threshold) keeps both
	// rows and adds a "mirip dengan ..." note.
	NoteThreshold float64 `yaml:"note_threshold"`
	// Token overlap at or above this lets a correction message
	// (carrying a marker from CorrectionMarkers) merge with the
	// message it corrects. Lower than Threshold on purpose: a
	// correction rewords the original.
	CorrectionThreshold float64 `yaml:"correction_threshold"`
	// Messages further apart than this (by message timestamp) are
	// never merged or noted.
	TimeWindowHours int `yaml:"time_window_hours"`
	// Whole words that mark a message as a correction of an earlier
	// one (lowercase, matched as whole words).
	CorrectionMarkers []string `yaml:"correction_markers"`
}

// Validate checks the dedup section. It does nothing when dedup is
// disabled.
func (d *DedupConfig) Validate() error {
	if !d.Enabled {
		return nil
	}
	if d.Threshold <= 0 || d.Threshold > 1 {
		return fmt.Errorf("dedup.threshold must be in (0, 1]")
	}
	if d.NoteThreshold <= 0 || d.NoteThreshold >= d.Threshold {
		return fmt.Errorf("dedup.note_threshold must be in (0, dedup.threshold)")
	}
	if d.CorrectionThreshold <= 0 || d.CorrectionThreshold > d.Threshold {
		return fmt.Errorf("dedup.correction_threshold must be in (0, dedup.threshold]")
	}
	if d.TimeWindowHours <= 0 {
		return fmt.Errorf("dedup.time_window_hours must be > 0")
	}
	return nil
}

// ScheduleEntry is one built-in schedule: a cron spec plus the collection
// window it covers. Spec is a 5-field cron expression
// (minute hour day-of-month month day-of-week) or a descriptor such as
// @daily or @every 1h.
type ScheduleEntry struct {
	Spec        string `yaml:"spec"`
	Label       string `yaml:"label"`
	WindowHours int    `yaml:"window_hours"`
}

// AgendaConfig controls agenda extraction: parsing date, time,
// location, and title from message text into agenda candidates (one
// per message group; see collector/agenda.go). It never creates
// calendar events; producing candidates is the scope.
type AgendaConfig struct {
	// Enable agenda extraction. When false, no Agenda file or sheet
	// is written.
	Enabled bool `yaml:"enabled"`
	// Minutes added to the start time when the text says the event
	// runs "sampai selesai" / "s.d. selesai" with no end time
	// (default 120 = 2 hours, the operator's standing rule). The
	// computed end is flagged as an assumption on the candidate.
	UntilFinishedMinutes int `yaml:"until_finished_minutes"`
	// Timezone event times are read in. Only "WIB" (fixed UTC+7) and
	// its IANA name "Asia/Jakarta" are supported.
	Timezone string `yaml:"timezone"`
}

// Validate checks the agenda section. It does nothing when agenda is
// disabled.
func (a *AgendaConfig) Validate() error {
	if !a.Enabled {
		return nil
	}
	if a.UntilFinishedMinutes <= 0 {
		return fmt.Errorf("agenda.until_finished_minutes must be > 0")
	}
	switch a.Timezone {
	case "WIB", "Asia/Jakarta":
	default:
		return fmt.Errorf("agenda.timezone %q: want \"WIB\" or \"Asia/Jakarta\"", a.Timezone)
	}
	return nil
}

// Config is the whole collector configuration file.
type Config struct {
	Bridge     BridgeConfig     `yaml:"bridge"`
	Collection CollectionConfig `yaml:"collection"`
	Media      MediaConfig      `yaml:"media"`
	Dedup      DedupConfig      `yaml:"dedup"`
	Upload     UploadConfig     `yaml:"upload"`
	Agenda     AgendaConfig     `yaml:"agenda"`
	Schedules  []ScheduleEntry  `yaml:"schedules"`
}

// LoadConfig reads, parses, and validates a YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

func defaultConfig() *Config {
	return &Config{
		Bridge: BridgeConfig{
			APIURL:               "http://127.0.0.1:8080/api",
			TokenFile:            "./bridge/store/.bridge-token",
			DBPath:               "./bridge/store/messages.db",
			Binary:               "./bridge/whatsapp-bridge",
			Autostart:            true,
			HealthTimeoutSeconds: 30,
		},
		Collection: CollectionConfig{
			OutputDir:     "./evidence",
			LookbackHours: 24,
			Redact:        true,
		},
		Media: MediaConfig{
			Download:          true,
			MaxAttempts:       3,
			RetryDelaySeconds: 30,
		},
		Dedup: DedupConfig{
			Enabled:             true,
			Threshold:           0.55,
			NoteThreshold:       0.35,
			CorrectionThreshold: 0.30,
			TimeWindowHours:     48,
			CorrectionMarkers:   []string{"ralat", "koreksi", "revisi", "perubahan", "update", "perbaikan", "diperbaiki"},
		},
		Upload: UploadConfig{
			Enabled:           false,
			Backend:           "drive",
			MaxAttempts:       3,
			RetryDelaySeconds: 10,
			Strict:            false,
			S3: S3UploadConfig{
				Region:       "auto",
				UsePathStyle: true,
			},
		},
		Agenda: AgendaConfig{
			Enabled:              true,
			UntilFinishedMinutes: 120,
			Timezone:             "WIB",
		},
	}
}

// Validate checks the config for mistakes that would make a run useless or
// surprising. It does not check that the bridge is actually running.
func (c *Config) Validate() error {
	if c.Collection.OutputDir == "" {
		return fmt.Errorf("collection.output_dir is required")
	}
	if len(c.Collection.Allowlist) == 0 {
		return fmt.Errorf("collection.allowlist is empty: nothing would be collected")
	}
	for _, jid := range c.Collection.Allowlist {
		if !isJID(jid) {
			return fmt.Errorf("collection.allowlist: %q is not a chat JID (expected <id>@g.us, <phone>@s.whatsapp.net or <id>@lid)", jid)
		}
	}
	if c.Collection.LookbackHours <= 0 {
		return fmt.Errorf("collection.lookback_hours must be > 0")
	}
	if c.Media.MaxAttempts < 1 {
		return fmt.Errorf("media.max_attempts must be >= 1")
	}
	if c.Media.RetryDelaySeconds < 0 {
		return fmt.Errorf("media.retry_delay_seconds must be >= 0")
	}
	if c.Bridge.HealthTimeoutSeconds < 0 {
		return fmt.Errorf("bridge.health_timeout_seconds must be >= 0")
	}
	if err := c.Dedup.Validate(); err != nil {
		return err
	}
	if err := c.Upload.Validate(); err != nil {
		return err
	}
	if err := c.Agenda.Validate(); err != nil {
		return err
	}
	for _, s := range c.Schedules {
		if s.Label == "" {
			return fmt.Errorf("schedules: every entry needs a label")
		}
		if s.WindowHours <= 0 {
			return fmt.Errorf("schedules %q: window_hours must be > 0", s.Label)
		}
		if err := ValidateCronSpec(s.Spec); err != nil {
			return fmt.Errorf("schedules %q: %w", s.Label, err)
		}
	}
	return nil
}

// Validate checks the upload section. It does nothing when upload is
// disabled (local-only is the default); when enabled it requires a known
// backend and the backend's mandatory settings. It does not contact the
// remote service.
func (u *UploadConfig) Validate() error {
	if !u.Enabled {
		return nil
	}
	switch u.Backend {
	case "drive":
		if u.Drive.CredentialsFile == "" {
			return fmt.Errorf("upload.drive.credentials_file is required when upload is enabled with backend drive")
		}
		if _, err := os.Stat(u.Drive.CredentialsFile); err != nil {
			return fmt.Errorf("upload.drive.credentials_file %q: %w", u.Drive.CredentialsFile, err)
		}
		if u.Drive.FolderID == "" && u.Drive.FolderPath == "" {
			return fmt.Errorf("upload.drive needs folder_id or folder_path when upload is enabled with backend drive")
		}
	case "s3":
		if u.S3.Bucket == "" {
			return fmt.Errorf("upload.s3.bucket is required when upload is enabled with backend s3")
		}
		if u.S3.Endpoint == "" {
			return fmt.Errorf("upload.s3.endpoint is required when upload is enabled with backend s3 (set it to the S3-compatible endpoint; for plain AWS S3 use https://s3.<region>.amazonaws.com)")
		}
		if u.S3.Region == "" {
			return fmt.Errorf("upload.s3.region is required when upload is enabled with backend s3 (R2 uses \"auto\")")
		}
	default:
		return fmt.Errorf("upload.backend %q: want \"drive\" or \"s3\"", u.Backend)
	}
	if u.MaxAttempts < 1 {
		return fmt.Errorf("upload.max_attempts must be >= 1")
	}
	if u.RetryDelaySeconds < 0 {
		return fmt.Errorf("upload.retry_delay_seconds must be >= 0")
	}
	return nil
}

// isJID is a shape check only: <something>@<known server>.
func isJID(s string) bool {
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	switch s[at+1:] {
	case "g.us", "s.whatsapp.net", "lid":
		return true
	default:
		return false
	}
}
