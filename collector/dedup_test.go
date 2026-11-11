package collector

import (
	"strings"
	"testing"
	"time"
)

// All fixtures below are fictional (555 range / all-zero group IDs).
// No real JIDs, phone numbers, names, or chat content.

func dedupCfg() DedupConfig { return defaultConfig().Dedup }

func dedupMsg(id, chat, sender, content string, ts time.Time) Message {
	return Message{ID: id, ChatJID: chat, Sender: sender, Content: content, Timestamp: ts}
}

func TestSignatureExtraction(t *testing.T) {
	cfg := dedupCfg()
	base := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	m := dedupMsg("x1", groupA, "15550000001@s.whatsapp.net",
		"Undangan rapat sinkronisasi STRANAS PPDT 2027-2029, Rabu 11 November 2026 jam 09:00-12:00 via Zoom https://zoom.us/j/5551112222?pwd=abc",
		base)
	sig := signatureOf(m, cfg)

	// Zoom link: normalized (query stripped), Zoom ID extracted.
	if len(sig.links) != 1 || len(sig.zoomIDs) != 1 {
		t.Fatalf("links=%v zoom=%v", sig.links, sig.zoomIDs)
	}
	for l := range sig.links {
		if strings.Contains(l, "?") || strings.Contains(l, "pwd") {
			t.Fatalf("link not normalized: %q", l)
		}
	}
	if _, ok := sig.zoomIDs["5551112222"]; !ok {
		t.Fatalf("zoom id missing: %v", sig.zoomIDs)
	}
	// Date: month-name form -> "11-11".
	if _, ok := sig.dates["11-11"]; !ok {
		t.Fatalf("dates = %v, want 11-11", sig.dates)
	}
	// Times: range start + end.
	if len(sig.times) != 2 {
		t.Fatalf("times = %v, want {540 720}", sig.times)
	}
	if _, ok := sig.times[540]; !ok {
		t.Fatalf("09:00 missing from times %v", sig.times)
	}
	if _, ok := sig.times[720]; !ok {
		t.Fatalf("12:00 missing from times %v", sig.times)
	}
	// Tokens: stopwords and the date/time/link fragments are gone;
	// the topic words stay.
	for _, want := range []string{"sinkronisasi", "stranas", "ppdt", "2027", "2029", "rabu"} {
		if _, ok := sig.tokens[want]; !ok {
			t.Fatalf("token %q missing: %v", want, sig.tokens)
		}
	}
	for _, gone := range []string{"undangan", "rapat", "jam", "zoom", "09", "00", "11", "5551112222"} {
		if _, ok := sig.tokens[gone]; ok {
			t.Fatalf("token %q should have been dropped: %v", gone, sig.tokens)
		}
	}
	if sig.correction {
		t.Fatal("plain invitation flagged as a correction")
	}
}

func TestSignatureDatesTimes(t *testing.T) {
	cfg := dedupCfg()
	base := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		text  string
		dates []string
		times []int
	}{
		{"Rapat 12/11/2026 jam 9 pagi", []string{"12-11"}, []int{540}},
		{"Rapat 12 Nov 2026 pukul 09.30", []string{"12-11"}, []int{570}},
		{"Rapat pukul 19.00 WIB", nil, []int{1140}},
		{"Rapat jam 13:00-15:00", nil, []int{780, 900}},
		{"Rapat 11 November", []string{"11-11"}, nil},
		{"Versi 2.7.1 sudah rilis", nil, nil}, // not a time
	}
	for _, tc := range cases {
		sig := signatureOf(dedupMsg("x", groupA, "s", tc.text, base), cfg)
		for _, d := range tc.dates {
			if _, ok := sig.dates[d]; !ok {
				t.Fatalf("%q: dates = %v, want %v", tc.text, sig.dates, tc.dates)
			}
		}
		if len(sig.dates) != len(tc.dates) {
			t.Fatalf("%q: dates = %v, want %v", tc.text, sig.dates, tc.dates)
		}
		for _, tm := range tc.times {
			if _, ok := sig.times[tm]; !ok {
				t.Fatalf("%q: times = %v, want %v", tc.text, sig.times, tc.times)
			}
		}
		if len(sig.times) != len(tc.times) {
			t.Fatalf("%q: times = %v, want %v", tc.text, sig.times, tc.times)
		}
	}
}

func TestSignatureCorrectionAndMedia(t *testing.T) {
	cfg := dedupCfg()
	base := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	for _, text := range []string{
		"Ralat: rapat diundur jadi jam 13:00",
		"Koreksi jadwal, mohon maaf",
		"Revisi undangan: ruang berubah",
		"Update: link Zoom baru",
	} {
		sig := signatureOf(dedupMsg("x", groupA, "s", text, base), cfg)
		if !sig.correction {
			t.Fatalf("%q: correction not detected", text)
		}
	}
	if sig := signatureOf(dedupMsg("x", groupA, "s", "Rapat jam 09:00", base), cfg); sig.correction {
		t.Fatal("plain message flagged as a correction")
	}
	// Media identity: same URL -> same key; same filename+length -> same key.
	m1 := dedupMsg("x", groupA, "s", "KAK rapat", base)
	m1.MediaType, m1.URL = "document", "https://example.invalid/kak.enc"
	m2 := dedupMsg("y", groupB, "s", "Berikut KAK-nya", base)
	m2.MediaType, m2.URL = "document", "https://example.invalid/kak.enc"
	if !sharedKeys(signatureOf(m1, cfg).mediaKeys, signatureOf(m2, cfg).mediaKeys) {
		t.Fatal("same media URL did not share a media key")
	}
	m3 := dedupMsg("z", groupA, "s", "Foto lain", base)
	m3.MediaType, m3.URL = "image", "https://example.invalid/other.enc"
	if sharedKeys(signatureOf(m1, cfg).mediaKeys, signatureOf(m3, cfg).mediaKeys) {
		t.Fatal("different media URLs shared a media key")
	}
}

func TestJaccard(t *testing.T) {
	set := func(words ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, w := range words {
			m[w] = struct{}{}
		}
		return m
	}
	if got := jaccard(set("a", "b"), set("a", "b")); got != 1 {
		t.Fatalf("identical = %v, want 1", got)
	}
	if got := jaccard(set("a"), set("b")); got != 0 {
		t.Fatalf("disjoint = %v, want 0", got)
	}
	if got := jaccard(set(), set()); got != 0 {
		t.Fatalf("empty/empty = %v, want 0", got)
	}
	if got := jaccard(set("a", "b", "c"), set("b", "c", "d")); got != 0.5 {
		t.Fatalf("half overlap = %v, want 0.5", got)
	}
}

// TestGroupMessagesCases covers the six required fictional pairs (plus a
// date-conflict pair) through the real grouping function.
func TestGroupMessagesCases(t *testing.T) {
	cfg := dedupCfg()
	base := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

	t.Run("a reworded same event merges", func(t *testing.T) {
		msgs := []Message{
			dedupMsg("a1", groupA, "15550000001@s.whatsapp.net",
				"Undangan rapat persiapan penyusunan laporan perkembangan status daerah tertinggal 2026, Rabu 11 November jam 09:00-12:00 via Zoom", at(0)),
			dedupMsg("a2", groupA, "15550000002@s.whatsapp.net",
				"Rapat persiapan laporan perkembangan status DT jam 09:00 hari Rabu", at(30)),
		}
		groups := groupMessages(msgs, cfg)
		if len(groups) != 1 || len(groups[0].members) != 2 {
			t.Fatalf("groups = %d, want 1 group of 2", len(groups))
		}
	})

	t.Run("b cross-post to two groups merges", func(t *testing.T) {
		msgs := []Message{
			dedupMsg("b1", groupA, "15550000001@s.whatsapp.net",
				"Pengumuman: apel pagi Senin 16 November jam 07:30 di lapangan utama", at(0)),
			dedupMsg("b2", groupB, "15550000002@s.whatsapp.net",
				"Pengumuman: apel pagi Senin 16 November jam 07:30 di lapangan utama", at(15)),
		}
		groups := groupMessages(msgs, cfg)
		if len(groups) != 1 || len(groups[0].members) != 2 {
			t.Fatalf("groups = %d, want 1 group of 2", len(groups))
		}
		if len(groups[0].notes) != 0 {
			t.Fatalf("merged group carries notes: %v", groups[0].notes)
		}
	})

	t.Run("c same topic different time keeps both with a note", func(t *testing.T) {
		msgs := []Message{
			dedupMsg("c1", groupA, "15550000001@s.whatsapp.net",
				"Rapat persiapan penyusunan laporan perkembangan status daerah tertinggal 2026, Rabu 11 November jam 09:00-12:00 via Zoom", at(0)),
			dedupMsg("c2", groupB, "15550000002@s.whatsapp.net",
				"Rapat persiapan laporan perkembangan status DT jam 13:00", at(30)),
		}
		groups := groupMessages(msgs, cfg)
		if len(groups) != 2 {
			t.Fatalf("groups = %d, want 2 (no merge)", len(groups))
		}
		for _, g := range groups {
			if len(g.notes) != 1 {
				t.Fatalf("group %s notes = %v, want 1 time-conflict note", g.firstID(), g.notes)
			}
			for _, note := range g.notes {
				if !strings.Contains(note, "jam berbeda") {
					t.Fatalf("note = %q, want a 'jam berbeda' note", note)
				}
			}
		}
		if got := groups[0].notes["c2"]; !strings.Contains(got, "09:00-12:00 vs 13:00") {
			t.Fatalf("c1 note = %q", got)
		}
		if got := groups[1].notes["c1"]; !strings.Contains(got, "13:00 vs 09:00-12:00") {
			t.Fatalf("c2 note = %q", got)
		}
	})

	t.Run("d correction updates the same row", func(t *testing.T) {
		msgs := []Message{
			dedupMsg("d1", groupA, "15550000001@s.whatsapp.net",
				"Rapat koordinasi tim data, Kamis 12 November jam 10:00, ruang rapat utama", at(0)),
			dedupMsg("d2", groupA, "15550000002@s.whatsapp.net",
				"Ralat: rapat koordinasi tim data diundur jadi jam 13:00, tetap di ruang rapat utama", at(30)),
		}
		groups := groupMessages(msgs, cfg)
		if len(groups) != 1 || len(groups[0].members) != 2 {
			t.Fatalf("groups = %d, want 1 group of 2", len(groups))
		}
		if !groups[0].correction {
			t.Fatal("merged group not flagged as a correction")
		}
		// Canonical is the correction itself (it supersedes).
		if groups[0].members[groups[0].canonical].ID != "d2" {
			t.Fatalf("canonical = %s, want d2 (the correction)", groups[0].members[groups[0].canonical].ID)
		}
	})

	t.Run("e different events sharing words do not merge", func(t *testing.T) {
		msgs := []Message{
			dedupMsg("e1", groupA, "15550000001@s.whatsapp.net",
				"Rapat persiapan penyusunan STRANAS PPDT 2027-2029", at(0)),
			dedupMsg("e2", groupA, "15550000002@s.whatsapp.net",
				"Rapat persiapan penetapan daerah tertinggal 2026", at(30)),
		}
		groups := groupMessages(msgs, cfg)
		if len(groups) != 2 {
			t.Fatalf("groups = %d, want 2", len(groups))
		}
		for _, g := range groups {
			if len(g.notes) != 0 {
				t.Fatalf("group %s notes = %v, want none", g.firstID(), g.notes)
			}
		}
	})

	t.Run("f forwarded copies merge", func(t *testing.T) {
		msgs := []Message{
			dedupMsg("f1", groupA, "15550000001@s.whatsapp.net",
				"Undangan diskusi revisi Perpres PPDT, Jumat 13 November jam 13:00 via Zoom", at(0)),
			dedupMsg("f2", groupB, "15550000002@s.whatsapp.net",
				"Diteruskan: Undangan diskusi revisi Perpres PPDT, Jumat 13 November jam 13:00 via Zoom", at(20)),
		}
		groups := groupMessages(msgs, cfg)
		if len(groups) != 1 || len(groups[0].members) != 2 {
			t.Fatalf("groups = %d, want 1 group of 2", len(groups))
		}
	})

	t.Run("g same event different date keeps both with a note", func(t *testing.T) {
		msgs := []Message{
			dedupMsg("g1", groupA, "15550000001@s.whatsapp.net",
				"Rapat persiapan penyusunan laporan perkembangan status daerah tertinggal 2026, Rabu 11 November jam 09:00", at(0)),
			dedupMsg("g2", groupA, "15550000002@s.whatsapp.net",
				"Rapat persiapan penyusunan laporan perkembangan status daerah tertinggal 2026, Kamis 12 November jam 09:00", at(30)),
		}
		groups := groupMessages(msgs, cfg)
		if len(groups) != 2 {
			t.Fatalf("groups = %d, want 2 (no merge)", len(groups))
		}
		for _, g := range groups {
			for _, note := range g.notes {
				if !strings.Contains(note, "tanggal berbeda") {
					t.Fatalf("note = %q, want a 'tanggal berbeda' note", note)
				}
			}
		}
	})

	t.Run("h shared zoom link merges despite short text", func(t *testing.T) {
		msgs := []Message{
			dedupMsg("h1", groupA, "15550000001@s.whatsapp.net",
				"Undangan rapat sinkronisasi STRANAS PPDT 2027-2029, Rabu 11 November 2026 jam 09:00 via Zoom https://zoom.us/j/5551112222", at(0)),
			dedupMsg("h2", groupB, "15550000002@s.whatsapp.net",
				"Link rapatnya: https://zoom.us/j/5551112222", at(15)),
		}
		groups := groupMessages(msgs, cfg)
		if len(groups) != 1 || len(groups[0].members) != 2 {
			t.Fatalf("groups = %d, want 1 group of 2", len(groups))
		}
	})

	t.Run("i outside the time window never merges", func(t *testing.T) {
		msgs := []Message{
			dedupMsg("i1", groupA, "15550000001@s.whatsapp.net",
				"Pengumuman: apel pagi Senin 16 November jam 07:30 di lapangan utama", at(0)),
			dedupMsg("i2", groupB, "15550000002@s.whatsapp.net",
				"Pengumuman: apel pagi Senin 16 November jam 07:30 di lapangan utama", at(49*60)),
		}
		groups := groupMessages(msgs, cfg)
		if len(groups) != 2 {
			t.Fatalf("groups = %d, want 2 (outside 48h window)", len(groups))
		}
	})

	t.Run("j disabled dedup keeps singletons", func(t *testing.T) {
		off := cfg
		off.Enabled = false
		msgs := []Message{
			dedupMsg("j1", groupA, "15550000001@s.whatsapp.net", "Pengumuman: apel pagi", at(0)),
			dedupMsg("j2", groupB, "15550000002@s.whatsapp.net", "Pengumuman: apel pagi", at(15)),
		}
		groups := groupMessages(msgs, off)
		if len(groups) != 2 {
			t.Fatalf("groups = %d, want 2 singletons", len(groups))
		}
	})
}

func TestDedupConfigValidate(t *testing.T) {
	cfg := defaultConfig()
	cfg.Collection.Allowlist = []string{groupA}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default dedup config rejected: %v", err)
	}
	bad := *cfg
	bad.Dedup.Threshold = 1.5
	if err := bad.Validate(); err == nil {
		t.Fatal("threshold > 1 accepted")
	}
	bad = *cfg
	bad.Dedup.NoteThreshold = 0.6 // >= threshold 0.55
	if err := bad.Validate(); err == nil {
		t.Fatal("note_threshold >= threshold accepted")
	}
	bad = *cfg
	bad.Dedup.CorrectionThreshold = 0.6
	if err := bad.Validate(); err == nil {
		t.Fatal("correction_threshold > threshold accepted")
	}
	bad = *cfg
	bad.Dedup.TimeWindowHours = 0
	if err := bad.Validate(); err == nil {
		t.Fatal("time_window_hours 0 accepted")
	}
	bad = *cfg
	bad.Dedup.Enabled = false
	bad.Dedup.Threshold = 99 // invalid but disabled: ignored
	if err := bad.Validate(); err != nil {
		t.Fatalf("disabled dedup should skip validation: %v", err)
	}
}
