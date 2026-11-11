package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestParseWhatsAppMarkup(t *testing.T) {
	cases := []struct {
		in   string
		want [][]Segment
	}{
		{"hello world", [][]Segment{{{Text: "hello world"}}}},
		{"*bold* plain", [][]Segment{{{Text: "bold", Bold: true}, {Text: " plain"}}}},
		{"_italic_ and *bold*", [][]Segment{{{Text: "italic", Italic: true}, {Text: " and "}, {Text: "bold", Bold: true}}}},
		{"a *b* c _d_ e", [][]Segment{{{Text: "a "}, {Text: "b", Bold: true}, {Text: " c "}, {Text: "d", Italic: true}, {Text: " e"}}}},
		{"*unclosed", [][]Segment{{{Text: "unclosed", Bold: true}}}},
		// A marker of the other kind inside a styled run is literal text.
		{"a *b_c* d", [][]Segment{{{Text: "a "}, {Text: "b_c", Bold: true}, {Text: " d"}}}},
		{"line1\n\nline3", [][]Segment{{{Text: "line1"}}, nil, {{Text: "line3"}}}},
		{"", [][]Segment{nil}},
		{"**", [][]Segment{nil}},
	}
	for _, tc := range cases {
		got := ParseWhatsAppMarkup(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseWhatsAppMarkup(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestWrapChatText(t *testing.T) {
	f, err := loadChatFonts()
	if err != nil {
		t.Fatal(err)
	}
	maxW := float64(renderBubbleW - renderPadX*2)
	long := "AGENDA RAPAT hari ini jam 09.00 WIB sampai selesai di Zoom Meeting dengan pembahasan prioritas"
	lines := wrapSegments(ParseWhatsAppMarkup(long)[0], maxW, f)
	if len(lines) < 2 {
		t.Fatalf("long text wrapped to %d lines, want >= 2", len(lines))
	}
	for _, ln := range lines {
		w := 0.0
		for i, wd := range ln.words {
			if i > 0 {
				w += f.measure(styleRegular, renderBodySize, " ")
			}
			w += f.measure(styleFor(wd), renderBodySize, wd.text)
		}
		if w > maxW+1 {
			t.Fatalf("wrapped line too wide: %.1f > %.1f", w, maxW)
		}
	}
	short := wrapSegments(ParseWhatsAppMarkup("hi")[0], maxW, f)
	if len(short) != 1 {
		t.Fatalf("short text wrapped to %d lines, want 1", len(short))
	}
}

func renderPNGHeight(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func TestRenderChatPNG(t *testing.T) {
	msg := RenderMessage{
		Sender:    "sender:abcd1234",
		Timestamp: time.Date(2026, 11, 11, 3, 8, 0, 0, time.UTC),
		Text:      "AGENDA *RAPAT* hari ini\n1. 09.00 WIB - Selesai\nTempat: _Zoom Meeting_",
	}
	out := filepath.Join(t.TempDir(), "render.png")
	if err := RenderChat([]RenderMessage{msg}, RenderDatePill(msg.Timestamp), out); err != nil {
		t.Fatal(err)
	}
	w, h := renderPNGHeight(t, out)
	if w != renderWidth {
		t.Fatalf("width = %d, want %d", w, renderWidth)
	}
	if h < renderMinHeight {
		t.Fatalf("height = %d, want >= %d", h, renderMinHeight)
	}
	// The image must not be blank: the bubble color must cover a large
	// area and plenty of non-white pixels must exist (text, pill, avatar).
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	bubble, other := 0, 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			r8, g8, b8 := uint8(r>>8), uint8(g>>8), uint8(bl>>8)
			if r8 == 235 && g8 == 235 && b8 == 235 {
				bubble++
			}
			if r8 != 255 || g8 != 255 || b8 != 255 {
				other++
			}
		}
	}
	if bubble < 50000 {
		t.Fatalf("bubble pixels = %d, want >= 50000", bubble)
	}
	if other < 1000 {
		t.Fatalf("non-white pixels = %d, want >= 1000", other)
	}
}

func TestRenderChatMultiMessage(t *testing.T) {
	base := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	one := []RenderMessage{{Sender: "sender:abcd1234", Timestamp: base, Text: "satu"}}
	two := append(append([]RenderMessage{}, one...),
		RenderMessage{Sender: "sender:abcd1234", Timestamp: base.Add(5 * time.Minute), Text: "dua"})
	out1 := filepath.Join(t.TempDir(), "one.png")
	if err := RenderChat(one, "11 November 2026", out1); err != nil {
		t.Fatal(err)
	}
	out2 := filepath.Join(t.TempDir(), "two.png")
	if err := RenderChat(two, "11 November 2026", out2); err != nil {
		t.Fatal(err)
	}
	_, h1 := renderPNGHeight(t, out1)
	_, h2 := renderPNGHeight(t, out2)
	if h2 <= h1 {
		t.Fatalf("two messages height %d <= one message %d", h2, h1)
	}
}

func TestRenderChatNoMessages(t *testing.T) {
	if err := RenderChat(nil, "", filepath.Join(t.TempDir(), "x.png")); err == nil {
		t.Fatal("expected error for zero messages")
	}
}

func TestRenderChatLongTextCap(t *testing.T) {
	msg := RenderMessage{
		Sender:    "sender:abcd1234",
		Timestamp: time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC),
		Text:      strings.Repeat("kata ", 6000), // 30000 runes, over the cap
	}
	out := filepath.Join(t.TempDir(), "long.png")
	if err := RenderChat([]RenderMessage{msg}, "11 November 2026", out); err != nil {
		t.Fatal(err)
	}
	_, h := renderPNGHeight(t, out)
	if h > 20000 {
		t.Fatalf("height %d unbounded for a 30000-rune text", h)
	}
}

func TestRenderFileName(t *testing.T) {
	ts := time.Date(2026, 11, 11, 3, 8, 0, 0, time.UTC) // 10:08 WIB
	got := RenderFileName(ts, groupA)
	want := "1008_" + ChatShort(groupA) + "_chat.png"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if dm := RenderFileName(ts, dmA); !strings.HasPrefix(dm, "1008_dm-") {
		t.Fatalf("dm render name = %q", dm)
	}
}

func TestRenderDatePill(t *testing.T) {
	ts := time.Date(2026, 11, 11, 3, 8, 0, 0, time.UTC)
	if got := RenderDatePill(ts); got != "11 November 2026" {
		t.Fatalf("got %q", got)
	}
}

// seedRenderStore seeds a store with one row of each render-rule kind:
// text-only (render), image with failed download (no render), link text
// (no render), blank text (no render). All senders/contents are fictional.
func seedRenderStore(t *testing.T, base time.Time) string {
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
	if _, err := db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Group A', ?)`, groupA, base); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, file_length, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	mustInsert(t, db, ins, "r1", groupA, "15550000001@s.whatsapp.net", "plain text message", base.Add(-2*time.Hour), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "r2", groupA, "15550000002@s.whatsapp.net", "photo here", base.Add(-90*time.Minute), false, "image", "photo.jpg", "https://example.invalid/x.enc", 1234, nil)
	mustInsert(t, db, ins, "r3", groupA, "15550000001@s.whatsapp.net", "see https://example.invalid/info", base.Add(-60*time.Minute), false, "", "", "", 0, nil)
	mustInsert(t, db, ins, "r4", groupA, "15550000001@s.whatsapp.net", "   ", base.Add(-30*time.Minute), false, "", "", "", 0, nil)
	return path
}

func TestCollectRenderRules(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dbPath := seedRenderStore(t, base)
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
	if sum.New != 4 || sum.Renders != 1 || sum.RenderFailed != 0 || sum.MediaFailed != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	raw, err := os.ReadFile(filepath.Join(sum.RunDir, "evidence-120000.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env EvidenceFile
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	byID := map[string]EvidenceRow{}
	for _, r := range env.Rows {
		byID[r.MessageID] = r
	}
	if byID["r1"].ChatRender == "" || !strings.HasSuffix(byID["r1"].ChatRender, "_chat.png") {
		t.Fatalf("r1 ChatRender = %q, want a *_chat.png", byID["r1"].ChatRender)
	}
	for _, id := range []string{"r2", "r3", "r4"} {
		if byID[id].ChatRender != "" {
			t.Fatalf("%s ChatRender = %q, want empty", id, byID[id].ChatRender)
		}
	}
	// Exactly one file in media/: the render.
	mediaFiles, _ := filepath.Glob(filepath.Join(sum.RunDir, "media", "*"))
	if len(mediaFiles) != 1 || !strings.HasSuffix(mediaFiles[0], "_chat.png") {
		t.Fatalf("media files = %v", mediaFiles)
	}
	// Excel: r1 has the render semantics; r3 is a Link; r2 plain text.
	_, xrows, err := ReadRecapXLSX(filepath.Join(sum.RunDir, "Rekap-Evidence-20261111.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	if len(xrows) != 4 {
		t.Fatalf("xlsx rows = %d, want 4", len(xrows))
	}
	byContent := map[string][]string{}
	for _, r := range xrows {
		byContent[r[5]] = r
	}
	r1 := byContent["plain text message"]
	if r1[6] != "Screenshot chat (render)" || r1[9] != "dibuat dari teks pesan" || !strings.HasSuffix(r1[7], "_chat.png") {
		t.Fatalf("r1 xlsx row = %v", r1)
	}
	if byContent["see https://example.invalid/info"][6] != "Link" {
		t.Fatalf("r3 xlsx row = %v", byContent["see https://example.invalid/info"])
	}
	if byContent["photo here"][6] != "Pesan teks" {
		t.Fatalf("r2 xlsx row = %v", byContent["photo here"])
	}
}
