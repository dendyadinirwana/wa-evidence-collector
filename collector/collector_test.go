package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const testSchema = `
CREATE TABLE chats (
	jid TEXT PRIMARY KEY,
	name TEXT,
	last_message_time TIMESTAMP,
	ephemeral_expiration INTEGER NOT NULL DEFAULT 0,
	ephemeral_setting_timestamp INTEGER NOT NULL DEFAULT 0,
	last_read_time TIMESTAMP);
CREATE TABLE messages (
	id TEXT,
	chat_jid TEXT,
	sender TEXT,
	content TEXT,
	timestamp TIMESTAMP,
	is_from_me BOOLEAN,
	media_type TEXT,
	filename TEXT,
	url TEXT,
	media_key BLOB,
	file_sha256 BLOB,
	file_enc_sha256 BLOB,
	file_length INTEGER,
	deleted_at TIMESTAMP,
	quoted_message_id TEXT,
	PRIMARY KEY (id, chat_jid),
	FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);`

const (
	// Fictional fixture JIDs (555 range / all-zero group IDs). Not real.
	groupA = "120363000000000000@g.us"
	groupB = "120363000000000001@g.us"
	dmA    = "15551234567@s.whatsapp.net"
)

// seedStore creates a temp messages.db with the bridge schema and a few
// fake rows. base is the "now" the timestamps are relative to.
func seedStore(t *testing.T, base time.Time) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Group A', ?), (?, 'Group B', ?), (?, 'DM A', ?)`,
		groupA, base, groupB, base, dmA, base)
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, file_length, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	// In window: group A text + group A image + DM text.
	mustInsert(t, db, ins, "m1", groupA, "15550000001@s.whatsapp.net", "hello group A", base.Add(-2*time.Hour), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "m2", groupA, "15550000002@s.whatsapp.net", "photo here", base.Add(-1*time.Hour), false, "image", "photo.jpg", "https://example.invalid/x.enc", 1234, nil)
	mustInsert(t, db, ins, "m3", dmA, "15551234567@s.whatsapp.net", "hi dm", base.Add(-30*time.Minute), false, "", "", "", 0, nil)
	// Out of window (too old).
	mustInsert(t, db, ins, "m0", groupA, "15550000001@s.whatsapp.net", "old", base.Add(-48*time.Hour), false, "", "", "", 0, nil)
	// Deleted message inside the window: must be skipped.
	mustInsert(t, db, ins, "m4", groupA, "15550000001@s.whatsapp.net", "deleted", base.Add(-90*time.Minute), false, "", "", "", 0, base.Add(-80*time.Minute))
	// Other chat (not allowlisted) inside the window: must be skipped.
	mustInsert(t, db, ins, "m5", groupB, "15550000003@s.whatsapp.net", "other group", base.Add(-45*time.Minute), false, "", "", "", 0, nil)
	return path
}

func mustInsert(t *testing.T, db *sql.DB, q string, id, chat, sender, content string, ts time.Time, fromMe bool, mediaType, filename, url string, length int64, deletedAt interface{}) {
	t.Helper()
	_, err := db.Exec(q, id, chat, sender, content, ts.UTC().Format("2006-01-02 15:04:05+00:00"), fromMe, mediaType, filename, url, length, deletedAt)
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func TestFetchMessages(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedStore(t, base)
	db, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	msgs, err := FetchMessages(context.Background(), db, []string{groupA, dmA}, base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, m := range msgs {
		got = append(got, m.ID)
	}
	want := []string{"m1", "m2", "m3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got ids %v, want %v", got, want)
	}
	if msgs[0].ChatName != "Group A" {
		t.Fatalf("chat name = %q, want Group A", msgs[0].ChatName)
	}
	if msgs[1].MediaType != "image" || msgs[1].FileLen != 1234 {
		t.Fatalf("media row = %+v", msgs[1])
	}
	// Deterministic content hash, stable across calls.
	if msgs[0].ContentHash() != msgs[0].ContentHash() || len(msgs[0].ContentHash()) != 64 {
		t.Fatalf("bad content hash %q", msgs[0].ContentHash())
	}
}

func TestOpenStoreMissing(t *testing.T) {
	if _, err := OpenStore(filepath.Join(t.TempDir(), "nope.db")); err == nil {
		t.Fatal("expected error for missing store file")
	}
}

func TestRedact(t *testing.T) {
	s1 := RedactSender("15551234567@s.whatsapp.net")
	s2 := RedactSender("15551234567@s.whatsapp.net")
	if s1 != s2 || !strings.HasPrefix(s1, "sender:") || len(s1) != len("sender:")+8 {
		t.Fatalf("unstable sender pseudonym: %q", s1)
	}
	if s1 == RedactSender("15557654321@s.whatsapp.net") {
		t.Fatal("different senders got the same pseudonym")
	}
	if strings.Contains(s1, "15551234567") {
		t.Fatal("pseudonym leaks the phone number")
	}
	content := RedactContent("call me at +1 555 123 4567 or ping 15551234567@s.whatsapp.net, see https://wa.me/15551234567")
	if strings.Contains(content, "555") || strings.Contains(content, "s.whatsapp.net") || strings.Contains(content, "wa.me") {
		t.Fatalf("redaction missed something: %q", content)
	}
	if got := RedactContent("meeting in 2026, room 12, 3 people"); got != "meeting in 2026, room 12, 3 people" {
		t.Fatalf("redaction mangled short numbers: %q", got)
	}
	if got := ChatShort(groupA); !strings.HasPrefix(got, "g-") || strings.Contains(got, "120363") {
		t.Fatalf("bad chat short label: %q", got)
	}
}

func TestValidateConfig(t *testing.T) {
	cfg := defaultConfig()
	cfg.Collection.Allowlist = []string{groupA}
	cfg.Schedules = []ScheduleEntry{{Spec: "30 6 * * 1-6", Label: "morning", WindowHours: 2}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := *cfg
	bad.Collection.Allowlist = []string{"not-a-jid"}
	if err := bad.Validate(); err == nil {
		t.Fatal("bad JID accepted")
	}
	bad = *cfg
	bad.Schedules = []ScheduleEntry{{Spec: "not a cron", Label: "x", WindowHours: 1}}
	if err := bad.Validate(); err == nil {
		t.Fatal("bad cron spec accepted")
	}
	bad = *cfg
	bad.Collection.Allowlist = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("empty allowlist accepted")
	}
	if err := ValidateCronSpec("@every 1h"); err != nil {
		t.Fatalf("@every rejected: %v", err)
	}
}

// stubBridge serves /api/health and /api/download for tests. downloadPath
// is the file it reports as downloaded; failDownload makes it fail.
func stubBridge(t *testing.T, downloadPath string, failDownload bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","connected":true}`))
	})
	mux.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if failDownload {
			_, _ = w.Write([]byte(`{"success":false,"message":"media expired"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"success":true,"message":"ok","filename":"image.jpg","path":%q}`, downloadPath)
	})
	return httptest.NewServer(mux)
}

func testConfig(t *testing.T, dbPath, apiURL, outDir string) *Config {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), ".bridge-token")
	if err := os.WriteFile(tokenFile, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Bridge.APIURL = apiURL
	cfg.Bridge.TokenFile = tokenFile
	cfg.Bridge.DBPath = dbPath
	cfg.Collection.OutputDir = outDir
	cfg.Collection.Allowlist = []string{groupA, dmA}
	cfg.Collection.LookbackHours = 24
	cfg.Collection.Redact = true
	cfg.Media.Download = true
	cfg.Media.MaxAttempts = 2
	cfg.Media.RetryDelaySeconds = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCollectEndToEnd(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedStore(t, base)
	outDir := t.TempDir()

	// Fake downloaded media file the stub bridge "returns".
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
	ctx := context.Background()
	sum, err := coll.Collect(ctx, "test", base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Scanned != 3 || sum.New != 3 || sum.SkippedSeen != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.MediaOK != 1 || sum.MediaFailed != 0 {
		t.Fatalf("media counts = %+v", sum)
	}
	// Both text-only rows (m1, m3) get a chat render; the media row does not.
	if sum.Renders != 2 || sum.RenderFailed != 0 {
		t.Fatalf("render counts = %+v", sum)
	}
	// JSON export parses and holds 3 redacted rows.
	raw, err := os.ReadFile(filepath.Join(sum.RunDir, "evidence-120000.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env EvidenceFile
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Rows) != 3 {
		t.Fatalf("rows = %d", len(env.Rows))
	}
	for _, r := range env.Rows {
		if strings.Contains(r.Sender, "1555") || strings.Contains(r.Content, "1555") {
			t.Fatalf("redaction leaked into row %+v", r)
		}
		if r.MediaType == "" {
			if r.ChatRender == "" || !strings.HasSuffix(r.ChatRender, "_chat.png") {
				t.Fatalf("text row %s ChatRender = %q, want a *_chat.png", r.MessageID, r.ChatRender)
			}
		} else if r.ChatRender != "" {
			t.Fatalf("media row %s ChatRender = %q, want empty", r.MessageID, r.ChatRender)
		}
	}
	// Media file landed in media/ with the hashed chat prefix, plus one
	// chat render per text-only row (m1, m3).
	mediaFiles, _ := filepath.Glob(filepath.Join(sum.RunDir, "media", "*"))
	if len(mediaFiles) != 3 {
		t.Fatalf("media files = %v", mediaFiles)
	}
	renders := 0
	for _, f := range mediaFiles {
		if strings.HasSuffix(f, "_chat.png") {
			renders++
		}
	}
	if renders != 2 {
		t.Fatalf("render files = %d, want 2 (media files %v)", renders, mediaFiles)
	}
	// Manifest verifies.
	manifest, err := os.ReadFile(filepath.Join(sum.RunDir, "manifest.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "evidence-120000.json") || !strings.Contains(string(manifest), "media/") {
		t.Fatalf("manifest incomplete:\n%s", manifest)
	}
	if !strings.Contains(string(manifest), "Rekap-Evidence-20261111.xlsx") ||
		!strings.Contains(string(manifest), "Rekap-Evidence-202611-gabungan.xlsx") {
		t.Fatalf("manifest misses xlsx files:\n%s", manifest)
	}
	// Daily Excel recap: reopens cleanly, right header, 3 redacted rows.
	xlsxPath := filepath.Join(sum.RunDir, "Rekap-Evidence-20261111.xlsx")
	header, xrows, err := ReadRecapXLSX(xlsxPath)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(header) != fmt.Sprint(wantRecapHeader) {
		t.Fatalf("xlsx header = %v", header)
	}
	if len(xrows) != 3 {
		t.Fatalf("xlsx rows = %d, want 3", len(xrows))
	}
	for _, r := range xrows {
		if strings.Contains(r[3], "1555") || strings.Contains(r[5], "1555") {
			t.Fatalf("xlsx redaction leaked into row %v", r)
		}
		if r[2] != ChatShort(groupA) && r[2] != ChatShort(dmA) {
			t.Fatalf("xlsx grup = %q, want a hashed label", r[2])
		}
	}
	// Text-only rows (m1, m3) carry the render semantics; the media row (m2) does not.
	if xrows[0][6] != "Screenshot chat (render)" || xrows[0][9] != "dibuat dari teks pesan" || !strings.HasSuffix(xrows[0][7], "_chat.png") {
		t.Fatalf("m1 xlsx row = %v", xrows[0])
	}
	if xrows[1][6] != "Gambar" {
		t.Fatalf("m2 xlsx row = %v", xrows[1])
	}
	if xrows[2][6] != "Screenshot chat (render)" || xrows[2][9] != "dibuat dari teks pesan" || !strings.HasSuffix(xrows[2][7], "_chat.png") {
		t.Fatalf("m3 xlsx row = %v", xrows[2])
	}
	// Monthly combined file at the output root: same 3 rows.
	mheader, mrows, err := ReadRecapXLSX(filepath.Join(outDir, "Rekap-Evidence-202611-gabungan.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(mheader) != fmt.Sprint(wantRecapHeader) {
		t.Fatalf("monthly header = %v", mheader)
	}
	if len(mrows) != 3 {
		t.Fatalf("monthly rows = %d, want 3", len(mrows))
	}
	// Second run over the same window: everything skipped as seen.
	sum2, err := coll.Collect(ctx, "test", base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.New != 0 || sum2.SkippedSeen != 3 {
		t.Fatalf("second run summary = %+v", sum2)
	}
}

func TestCollectMediaFailure(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedStore(t, base)
	outDir := t.TempDir()
	srv := stubBridge(t, "", true) // downloads always fail
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
	// The run must still succeed; the failure is recorded, not fatal.
	// The two text-only rows still get renders even when media fails.
	if sum.New != 3 || sum.MediaFailed != 1 || sum.Renders != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	raw, _ := os.ReadFile(filepath.Join(sum.RunDir, "evidence-120000.json"))
	var env EvidenceFile
	_ = json.Unmarshal(raw, &env)
	for _, r := range env.Rows {
		if r.MediaType == "image" && r.MediaStatus != "failed" {
			t.Fatalf("image row status = %q, want failed", r.MediaStatus)
		}
	}
}

func TestLoadConfigExample(t *testing.T) {
	// The shipped example must parse and validate.
	cfg, err := LoadConfig("../config.yaml.example")
	if err != nil {
		t.Fatalf("config.yaml.example invalid: %v", err)
	}
	if len(cfg.Schedules) != 2 {
		t.Fatalf("example schedules = %d, want 2", len(cfg.Schedules))
	}
}

// seedAgendaStore seeds a store with 9 fictional messages (base 12:00 UTC,
// window 09:00-12:00) covering the agenda cases: a reworded cross-post
// pair (a1+a2, same Zoom link), an "s.d. selesai" event (a3), a dotted
// date (a4), a relative date (a5, "besok"), a date-only message (a6),
// a time-only message (a7), a greeting with no date/time (a8), and a
// range event (a9).
func seedAgendaStore(t *testing.T, base time.Time) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Group A', ?), (?, 'Group B', ?), (?, 'DM A', ?)`,
		groupA, base, groupB, base, dmA, base)
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, file_length, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	at := func(min int) time.Time { return base.Add(-time.Duration(180-min) * time.Minute) } // 09:00 + min
	mustInsert(t, db, ins, "a1", groupA, "15550000001@s.whatsapp.net",
		"Undangan Rapat Sinkronisasi STRANAS PPDT, Rabu 11 November 2026 jam 09:00-12:00 via Zoom https://zoom.us/j/5551112222", at(5), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "a2", groupB, "15550000002@s.whatsapp.net",
		"Rapat sinkronisasi STRANAS PPDT jam 9-12 hari ini, Zoom https://zoom.us/j/5551112222", at(20), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "a3", groupA, "15550000001@s.whatsapp.net",
		"Rapat persiapan laporan DT, 12 November 2026 pukul 13:00 s.d. selesai, Ruang Rapat Utama", at(30), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "a4", groupA, "15550000001@s.whatsapp.net",
		"Sosialisasi cuti ASN, 11.11.2026 jam 10:00, Aula Gedung A", at(40), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "a5", dmA, "15551234567@s.whatsapp.net",
		"Besok jam 07:30 upacara Hari Pahlawan di Lapangan Parkir", at(50), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "a6", groupA, "15550000001@s.whatsapp.net",
		"Rapat koordinasi tim data tanggal 12 November", at(60), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "a7", groupB, "15550000002@s.whatsapp.net",
		"Rapat jam 15:00", at(70), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "a8", dmA, "15551234567@s.whatsapp.net",
		"Selamat pagi semuanya, semangat hari ini!", at(80), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "a9", groupB, "15550000002@s.whatsapp.net",
		"Webinar BIDIKTAMU, 11 November 2026, 09:00 s.d. 12:00, live YouTube", at(90), false, "", "", "", 0, nil)
	return path
}

func TestCollectAgenda(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedAgendaStore(t, base)
	outDir := t.TempDir()
	srv := stubBridge(t, "", false)
	defer srv.Close()

	cfg := testConfig(t, dbPath, srv.URL+"/api", outDir)
	cfg.Collection.Allowlist = []string{groupA, groupB, dmA}
	coll, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := coll.Collect(context.Background(), "test", base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	// 9 messages -> 8 rows (a1+a2 merged), 7 candidates (a8 has no
	// date/time), 5 of them complete.
	if sum.Scanned != 9 || sum.New != 8 || sum.SkippedSeen != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Merged != 1 {
		t.Fatalf("merged = %d, want 1 (a1+a2)", sum.Merged)
	}
	if sum.Agenda != 7 || sum.AgendaComplete != 5 {
		t.Fatalf("agenda = %d complete = %d, want 7 / 5", sum.Agenda, sum.AgendaComplete)
	}

	// Agenda JSON: 7 candidates in row order.
	raw, err := os.ReadFile(filepath.Join(sum.RunDir, "Agenda-20261111.json"))
	if err != nil {
		t.Fatal(err)
	}
	var af AgendaFile
	if err := json.Unmarshal(raw, &af); err != nil {
		t.Fatal(err)
	}
	if af.Version != 1 || len(af.Candidates) != 7 {
		t.Fatalf("agenda file = version %d, %d candidates", af.Version, len(af.Candidates))
	}
	strptr := func(s string) *string { return &s }
	dur := func(n int) *int { return &n }
	want := []AgendaCandidate{
		{Title: "Rapat Sinkronisasi STRANAS PPDT", Date: "2026-11-11", Start: "09:00", End: strptr("12:00"), DurationMin: dur(180),
			Location: "Zoom", Link: "https://zoom.us/j/5551112222", Status: "lengkap", RowMessageID: "a1"},
		{Title: "Rapat persiapan laporan DT, Ruang Rapat Utama", Date: "2026-11-12", Start: "13:00", End: strptr("15:00"), DurationMin: dur(120),
			Location: "Ruang Rapat Utama", Status: "lengkap", EndAssumed: true, RowMessageID: "a3"},
		{Title: "Sosialisasi cuti ASN, Aula Gedung A", Date: "2026-11-11", Start: "10:00",
			Location: "Aula Gedung A", Status: "lengkap", RowMessageID: "a4"},
		{Title: "upacara Hari Pahlawan di Lapangan Parkir", Date: "2026-11-12", Start: "07:30",
			Location: "Lapangan Parkir", Status: "lengkap", RowMessageID: "a5"},
		{Title: "Rapat koordinasi tim data", Date: "2026-11-12", Start: "",
			Status: "belum lengkap", Missing: []string{"jam"}, RowMessageID: "a6"},
		{Title: "Rapat", Date: "", Start: "15:00",
			Status: "belum lengkap", Missing: []string{"tanggal"}, RowMessageID: "a7"},
		{Title: "Webinar BIDIKTAMU, live YouTube", Date: "2026-11-11", Start: "09:00", End: strptr("12:00"), DurationMin: dur(180),
			Status: "lengkap", RowMessageID: "a9"},
	}
	for i, w := range want {
		got := af.Candidates[i]
		if got.Title != w.Title || got.Date != w.Date || got.Start != w.Start || got.Status != w.Status ||
			got.Location != w.Location || got.Link != w.Link || got.RowMessageID != w.RowMessageID ||
			got.EndAssumed != w.EndAssumed || fmt.Sprint(got.Missing) != fmt.Sprint(w.Missing) {
			t.Fatalf("candidate %d = %+v, want %+v", i, got, w)
		}
		if (got.End == nil) != (w.End == nil) || (got.End != nil && *got.End != *w.End) {
			t.Fatalf("candidate %d end = %v, want %v", i, got.End, w.End)
		}
		if (got.DurationMin == nil) != (w.DurationMin == nil) || (got.DurationMin != nil && *got.DurationMin != *w.DurationMin) {
			t.Fatalf("candidate %d duration = %v, want %v", i, got.DurationMin, w.DurationMin)
		}
	}
	// The merged candidate lists both sources; the "besok" candidate
	// notes where its date came from; the assumed end is noted too.
	if len(af.Candidates[0].Sources) != 2 {
		t.Fatalf("candidate 0 sources = %d, want 2 (a1+a2)", len(af.Candidates[0].Sources))
	}
	if !strings.Contains(af.Candidates[3].Note, "tanggal dari \"besok\"") {
		t.Fatalf("candidate 3 note = %q, want the 'besok' resolution noted", af.Candidates[3].Note)
	}
	if !strings.Contains(af.Candidates[1].Note, "asumsi 2 jam") {
		t.Fatalf("candidate 1 note = %q, want the 2h assumption noted", af.Candidates[1].Note)
	}
	// The greeting (a8) produced no candidate; the links are raw.
	for _, c := range af.Candidates {
		if c.RowMessageID == "a8" {
			t.Fatal("a8 (greeting) produced a candidate")
		}
		if strings.Contains(c.Title, "1555") || strings.Contains(c.Location, "1555") {
			t.Fatalf("candidate %s leaks a phone number: %+v", c.RowMessageID, c)
		}
	}
	if af.Candidates[0].Link != "https://zoom.us/j/5551112222" {
		t.Fatalf("candidate 0 link = %q (the join link must stay raw)", af.Candidates[0].Link)
	}

	// The daily Excel has an "Agenda" sheet with the 7 candidates.
	header, arows, err := ReadAgendaSheet(filepath.Join(sum.RunDir, "Rekap-Evidence-20261111.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(header) != fmt.Sprint(agendaHeader) {
		t.Fatalf("agenda header = %v", header)
	}
	if len(arows) != 7 {
		t.Fatalf("agenda rows = %d, want 7", len(arows))
	}
	if arows[0][0] != "Rapat Sinkronisasi STRANAS PPDT" || arows[0][1] != "2026-11-11" ||
		arows[0][2] != "09:00" || arows[0][3] != "12:00" || arows[0][4] != "180" ||
		arows[0][5] != "Zoom" || arows[0][6] != "https://zoom.us/j/5551112222" || arows[0][7] != "lengkap" {
		t.Fatalf("agenda row 0 = %v", arows[0])
	}
	if !strings.Contains(arows[0][9], "(a1)") || !strings.Contains(arows[0][9], "(a2)") {
		t.Fatalf("agenda row 0 sources = %q, want both a1 and a2", arows[0][9])
	}
	if arows[1][3] != "15:00" || arows[1][4] != "120" || !strings.Contains(arows[1][10], "asumsi 2 jam") {
		t.Fatalf("agenda row 1 = %v", arows[1])
	}
	if arows[4][7] != "belum lengkap" || arows[4][8] != "jam" {
		t.Fatalf("agenda row 4 = %v", arows[4])
	}
	if arows[5][7] != "belum lengkap" || arows[5][8] != "tanggal" {
		t.Fatalf("agenda row 5 = %v", arows[5])
	}
	// The manifest lists the Agenda JSON (it is in the run folder).
	manifest, err := os.ReadFile(filepath.Join(sum.RunDir, "manifest.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "Agenda-20261111.json") {
		t.Fatalf("manifest misses the agenda file:\n%s", manifest)
	}
	// Second run over the same window: everything skipped as seen.
	sum2, err := coll.Collect(context.Background(), "test", base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.New != 0 || sum2.SkippedSeen != 9 || sum2.Agenda != 0 {
		t.Fatalf("second run summary = %+v", sum2)
	}
}

// window 09:00-12:00) covering the semantic-dedup cases: a reworded
// cross-post pair (m10+m11, same Zoom link), a same-topic pair at
// different times (m12 09:00-12:00 vs m13 13:00), a correction pair
// (m14 10:00, m15 ralat to 13:00), and a merely similar message (m16).
func seedDedupStore(t *testing.T, base time.Time) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Group A', ?), (?, 'Group B', ?), (?, 'DM A', ?)`,
		groupA, base, groupB, base, dmA, base)
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, file_length, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	at := func(min int) time.Time { return base.Add(-time.Duration(180-min) * time.Minute) } // 09:00 + min
	mustInsert(t, db, ins, "m10", groupA, "15550000001@s.whatsapp.net",
		"Undangan rapat sinkronisasi STRANAS PPDT 2027-2029, Rabu 11 November 2026 jam 09:00 via Zoom https://zoom.us/j/5551112222", at(5), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "m11", groupB, "15550000002@s.whatsapp.net",
		"Rapat sinkronisasi STRANAS PPDT jam 9 pagi ini, Zoom: https://zoom.us/j/5551112222", at(20), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "m12", groupA, "15550000001@s.whatsapp.net",
		"Rapat persiapan penyusunan laporan perkembangan status daerah tertinggal 2026, Rabu 11 November jam 09:00-12:00 via Zoom https://zoom.us/j/5553334444", at(30), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "m13", groupB, "15550000002@s.whatsapp.net",
		"Rapat persiapan laporan perkembangan status DT jam 13:00", at(45), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "m14", groupA, "15550000001@s.whatsapp.net",
		"Rapat koordinasi tim data, Kamis 12 November jam 10:00, ruang rapat utama", at(60), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "m15", groupA, "15550000002@s.whatsapp.net",
		"Ralat: rapat koordinasi tim data diundur jadi jam 13:00, tetap di ruang rapat utama", at(75), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "m16", dmA, "15551234567@s.whatsapp.net",
		"Rapat persiapan penyusunan STRANAS PPDT 2027-2029", at(90), false, "", "", "", 0, nil)
	return path
}

func TestCollectSemanticDedup(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedDedupStore(t, base)
	outDir := t.TempDir()
	srv := stubBridge(t, "", false)
	defer srv.Close()

	cfg := testConfig(t, dbPath, srv.URL+"/api", outDir)
	cfg.Collection.Allowlist = []string{groupA, groupB, dmA}
	coll, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := coll.Collect(context.Background(), "test", base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	// 7 messages -> 5 rows: [m10+m11], [m12], [m13], [m14+m15], [m16].
	if sum.Scanned != 7 || sum.New != 5 || sum.SkippedSeen != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Merged != 2 {
		t.Fatalf("merged = %d, want 2", sum.Merged)
	}
	// Rows carrying a "mirip dengan" note: m10+m11 (vs m16), m12, m13, m16.
	if sum.SimilarNotes != 4 {
		t.Fatalf("similar notes = %d, want 4", sum.SimilarNotes)
	}
	// Renders: m13, the m14+m15 group (one PNG, two messages), m16.
	// The m10+m11 and m12 rows carry Zoom links, so no render.
	if sum.Renders != 3 || sum.RenderFailed != 0 {
		t.Fatalf("render counts = %+v", sum)
	}
	mediaFiles, _ := filepath.Glob(filepath.Join(sum.RunDir, "media", "*"))
	if len(mediaFiles) != 3 {
		t.Fatalf("media files = %v, want 3 renders", mediaFiles)
	}

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
	if len(env.Rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(env.Rows))
	}
	// Row 0: m10+m11 merged across two groups; canonical is m10.
	r0 := env.Rows[0]
	if !r0.Merged || r0.IsCorrection || len(r0.Sources) != 2 {
		t.Fatalf("row0 = %+v", r0)
	}
	if r0.Sources[0].ChatJID != groupA || r0.Sources[1].ChatJID != groupB {
		t.Fatalf("row0 sources = %v, want groupA + groupB", r0.Sources)
	}
	if !strings.Contains(r0.Note, "digabung dari 2 sumber, 2 grup") {
		t.Fatalf("row0 note = %q", r0.Note)
	}
	if !strings.Contains(r0.Note, "mirip dengan m16") {
		t.Fatalf("row0 note = %q, want a 'mirip dengan m16' note", r0.Note)
	}
	if r0.ChatRender != "" {
		t.Fatalf("row0 render = %q, want empty (Zoom link is the evidence)", r0.ChatRender)
	}
	// Rows 1+2: same topic, different time: kept separate, with notes.
	if env.Rows[1].Merged || env.Rows[2].Merged {
		t.Fatal("m12/m13 merged despite the time difference")
	}
	if !strings.Contains(env.Rows[1].Note, "mirip dengan m13 (jam berbeda: 09:00-12:00 vs 13:00)") {
		t.Fatalf("row1 note = %q", env.Rows[1].Note)
	}
	if !strings.Contains(env.Rows[2].Note, "mirip dengan m12 (jam berbeda: 13:00 vs 09:00-12:00)") {
		t.Fatalf("row2 note = %q", env.Rows[2].Note)
	}
	// Row 3: m14+m15 merged as a correction; canonical is the ralat.
	r3 := env.Rows[3]
	if !r3.Merged || !r3.IsCorrection || len(r3.Sources) != 2 {
		t.Fatalf("row3 = %+v", r3)
	}
	if !strings.Contains(r3.Content, "Ralat:") {
		t.Fatalf("row3 content = %q, want the correction text as canonical", r3.Content)
	}
	if !strings.Contains(r3.Note, "digabung dari 2 sumber, 1 grup (ralat)") {
		t.Fatalf("row3 note = %q", r3.Note)
	}
	if r3.ChatRender == "" {
		t.Fatal("row3 (text-only merged row) has no chat render")
	}
	// Row 4: m16 alone, with a similarity note back to m10.
	if env.Rows[4].Merged || !strings.Contains(env.Rows[4].Note, "mirip dengan m10") {
		t.Fatalf("row4 = %+v", env.Rows[4])
	}

	// Daily Excel recap: 5 rows; the merged row lists both groups and
	// both senders, and its Konteks carries the merge note.
	_, xrows, err := ReadRecapXLSX(filepath.Join(sum.RunDir, "Rekap-Evidence-20261111.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	if len(xrows) != 5 {
		t.Fatalf("xlsx rows = %d, want 5", len(xrows))
	}
	wantGrup := ChatShort(groupA) + ", " + ChatShort(groupB)
	if xrows[0][2] != wantGrup {
		t.Fatalf("row0 grup = %q, want %q", xrows[0][2], wantGrup)
	}
	if !strings.Contains(xrows[0][3], "sender:") || !strings.Contains(xrows[0][3], ", ") {
		t.Fatalf("row0 pengirim = %q, want both senders", xrows[0][3])
	}
	if !strings.Contains(xrows[0][10], "digabung dari 2 sumber, 2 grup") {
		t.Fatalf("row0 konteks = %q", xrows[0][10])
	}
	if !strings.Contains(xrows[1][10], "jam berbeda: 09:00-12:00 vs 13:00") {
		t.Fatalf("row1 konteks = %q", xrows[1][10])
	}
	if !strings.Contains(xrows[3][10], "(ralat)") {
		t.Fatalf("row3 konteks = %q", xrows[3][10])
	}
	if xrows[3][6] != "Screenshot chat (render)" {
		t.Fatalf("row3 jenis evidence = %q", xrows[3][6])
	}
	// Second run over the same window: everything skipped as seen.
	sum2, err := coll.Collect(context.Background(), "test", base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.New != 0 || sum2.SkippedSeen != 7 {
		t.Fatalf("second run summary = %+v", sum2)
	}
}
