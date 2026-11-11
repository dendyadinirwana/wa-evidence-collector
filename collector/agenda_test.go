package collector

import (
	"strings"
	"testing"
	"time"
)

// Reference message time for the parse tests: Wed 11 Nov 2026,
// 09:00 WIB (02:00 UTC). "hari ini" = 11 Nov, "besok" = 12 Nov,
// "Senin depan" = Mon 16 Nov, "Jumat ini" = Fri 13 Nov.
var agendaRef = time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)

func TestParseAgendaFormats(t *testing.T) {
	// date, start, end, untilFinished, endOnly, location, link
	type want struct {
		date, start, end string
		untilFinished    bool
		endOnly          bool
		location, link   string
	}
	cases := []struct {
		name string
		text string
		want want
	}{
		{
			"month name + range + zoom",
			"Undangan Rapat Sinkronisasi STRANAS PPDT, Rabu 11 November 2026 jam 09:00-12:00 via Zoom https://zoom.us/j/5551112222",
			want{"2026-11-11", "09:00", "12:00", false, false, "Zoom", "https://zoom.us/j/5551112222"},
		},
		{
			"abbrev month + bare hour",
			"Rapat 11 Nov 2026 jam 7",
			want{"2026-11-11", "07:00", "", false, false, "", ""},
		},
		{
			"slash date + pukul dotted time",
			"Rapat 11/11/2026 pukul 07.00 WIB",
			want{"2026-11-11", "07:00", "", false, false, "", ""},
		},
		{
			"dotted date with year",
			"Rapat 11.11.2026 jam 09:00",
			want{"2026-11-11", "09:00", "", false, false, "", ""},
		},
		{
			"dotted date without year",
			"Rapat 11.11 jam 09:00",
			want{"2026-11-11", "09:00", "", false, false, "", ""},
		},
		{
			"month name without year",
			"Rapat 11 November jam 09:00",
			want{"2026-11-11", "09:00", "", false, false, "", ""},
		},
		{
			"month name with explicit past year",
			"Rapat 11 November 2025 jam 09:00",
			want{"2025-11-11", "09:00", "", false, false, "", ""},
		},
		{
			"two digit year",
			"Rapat 11/11/26 jam 09:00",
			want{"2026-11-11", "09:00", "", false, false, "", ""},
		},
		{
			"besok resolves against message date",
			"Rapat besok jam 07:30",
			want{"2026-11-12", "07:30", "", false, false, "", ""},
		},
		{
			"lusa",
			"Rapat lusa jam 09:00",
			want{"2026-11-13", "09:00", "", false, false, "", ""},
		},
		{
			"hari ini",
			"Rapat hari ini jam 09:00",
			want{"2026-11-11", "09:00", "", false, false, "", ""},
		},
		{
			"kemarin",
			"Rapat kemarin jam 09:00 berjalan lancar",
			want{"2026-11-10", "09:00", "", false, false, "", ""},
		},
		{
			"Senin depan",
			"Rapat Senin depan jam 09:00",
			want{"2026-11-16", "09:00", "", false, false, "", ""},
		},
		{
			"Jumat ini",
			"Rapat Jumat ini jam 09:00",
			want{"2026-11-13", "09:00", "", false, false, "", ""},
		},
		{
			"bare weekday",
			"Rapat Senin jam 09:00",
			want{"2026-11-16", "09:00", "", false, false, "", ""},
		},
		{
			"bare weekday today",
			"Rapat Rabu jam 09:00",
			want{"2026-11-11", "09:00", "", false, false, "", ""},
		},
		{
			"weekday minggu depan",
			"Rapat Senin minggu depan jam 09:00",
			want{"2026-11-16", "09:00", "", false, false, "", ""},
		},
		{
			"tanggal N",
			"Rapat tanggal 12 jam 09:00",
			want{"2026-11-12", "09:00", "", false, false, "", ""},
		},
		{
			"sampai selesai",
			"Rapat jam 09:00 s.d. selesai",
			want{"", "09:00", "", true, false, "", ""},
		},
		{
			"sampai selesai long form",
			"Rapat 11 November jam 09:00 sampai selesai",
			want{"2026-11-11", "09:00", "", true, false, "", ""},
		},
		{
			"s.d. range",
			"Rapat 09:00 s.d. 12:00",
			want{"", "09:00", "12:00", false, false, "", ""},
		},
		{
			"dotted range",
			"Rapat 09.00-12.00 WIB",
			want{"", "09:00", "12:00", false, false, "", ""},
		},
		{
			"jam 7 malam",
			"Rapat jam 7 malam",
			want{"", "19:00", "", false, false, "", ""},
		},
		{
			"jam 2 siang",
			"Rapat jam 2 siang",
			want{"", "14:00", "", false, false, "", ""},
		},
		{
			"jam 12 malam is midnight",
			"Rapat jam 12 malam",
			want{"", "00:00", "", false, false, "", ""},
		},
		{
			"two unconnected times are not a range",
			"Rapat jam 09:00 dan jam 13:00",
			want{"", "09:00", "", false, false, "", ""},
		},
		{
			"end only",
			"Rapat s.d. jam 12:00, 11 November",
			want{"2026-11-11", "", "12:00", false, true, "", ""},
		},
		{
			"overnight range",
			"Rapat 22:00-01:00",
			want{"", "22:00", "01:00", false, false, "", ""},
		},
		{
			"di venue",
			"Rapat di Ruang Rapat Utama, 11 November jam 09:00",
			want{"2026-11-11", "09:00", "", false, false, "Ruang Rapat Utama", ""},
		},
		{
			"tempat colon",
			"Rapat tempat: Aula Gedung A, 11 November jam 09:00",
			want{"2026-11-11", "09:00", "", false, false, "Aula Gedung A", ""},
		},
		{
			"lokasi colon",
			"Lokasi: Hotel Vasaka, 11 November jam 09:00",
			want{"2026-11-11", "09:00", "", false, false, "Hotel Vasaka", ""},
		},
		{
			"meet link",
			"Rapat via Google Meet https://meet.google.com/abc-defg-hij jam 09:00 tanggal 11 November",
			want{"2026-11-11", "09:00", "", false, false, "Google Meet", "https://meet.google.com/abc-defg-hij"},
		},
		{
			"generic link is not a meeting link",
			"Rapat 11 November jam 09:00, info: https://example.invalid/info",
			want{"2026-11-11", "09:00", "", false, false, "", "https://example.invalid/info"},
		},
		{
			"date only",
			"Rapat koordinasi tim data tanggal 12 November",
			want{"2026-11-12", "", "", false, false, "", ""},
		},
		{
			"time only",
			"Rapat jam 15:00",
			want{"", "15:00", "", false, false, "", ""},
		},
		{
			"no date no time",
			"Selamat pagi semuanya, semangat hari ini!",
			want{"", "", "", false, false, "", ""},
		},
		{
			"version number is not a date",
			"Versi 2.7.1 dirilis",
			want{"", "", "", false, false, "", ""},
		},
		{
			"invalid date ignored",
			"Rapat 31 Februari jam 09:00",
			want{"", "09:00", "", false, false, "", ""},
		},
		{
			"minggu depan alone is Monday of next week",
			"Rapat minggu depan jam 09:00",
			want{"2026-11-16", "09:00", "", false, false, "", ""},
		},
		{
			"relative date with an event word but no time",
			"Rapat besok",
			want{"2026-11-12", "", "", false, false, "", ""},
		},
		{
			"relative date without time or event word is ignored",
			"Besok libur",
			want{"", "", "", false, false, "", ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parseAgendaText(tc.text, agendaRef)
			gotDate := ""
			if p.hasDate {
				gotDate = p.date.Format("2006-01-02")
			}
			gotStart, gotEnd := "", ""
			if p.hasStart {
				gotStart = formatMin(p.start)
			}
			if p.hasEnd {
				gotEnd = formatMin(p.end % (24 * 60))
			}
			if gotDate != tc.want.date || gotStart != tc.want.start || gotEnd != tc.want.end {
				t.Fatalf("parse %q: date=%q start=%q end=%q, want date=%q start=%q end=%q",
					tc.text, gotDate, gotStart, gotEnd, tc.want.date, tc.want.start, tc.want.end)
			}
			if p.untilFinished != tc.want.untilFinished {
				t.Fatalf("parse %q: untilFinished=%v, want %v", tc.text, p.untilFinished, tc.want.untilFinished)
			}
			if p.endOnly != tc.want.endOnly {
				t.Fatalf("parse %q: endOnly=%v, want %v", tc.text, p.endOnly, tc.want.endOnly)
			}
			if p.location != tc.want.location {
				t.Fatalf("parse %q: location=%q, want %q", tc.text, p.location, tc.want.location)
			}
			if p.link != tc.want.link {
				t.Fatalf("parse %q: link=%q, want %q", tc.text, p.link, tc.want.link)
			}
			// A relative date must be noted; an explicit date must not.
			if tc.want.date != "" && strings.Contains(tc.text, "besok") && p.relativeNote == "" {
				t.Fatalf("parse %q: relative date not noted", tc.text)
			}
		})
	}
}

func formatMin(mins int) string {
	return time.Date(0, 1, 1, mins/60, mins%60, 0, 0, time.UTC).Format("15:04")
}

func TestAgendaTitle(t *testing.T) {
	cases := []struct {
		text, want string
	}{
		{
			"Undangan Rapat Sinkronisasi STRANAS PPDT, Rabu 11 November 2026 jam 09:00-12:00 via Zoom https://zoom.us/j/5551112222",
			"Rapat Sinkronisasi STRANAS PPDT",
		},
		{
			"Assalamualaikum wr wb\nUndangan rapat koordinasi tim data, 12 November jam 10:00",
			"rapat koordinasi tim data",
		},
		{
			"Rapat persiapan laporan DT, 12 November 2026 pukul 13:00 s.d. selesai, Ruang Rapat Utama",
			"Rapat persiapan laporan DT, Ruang Rapat Utama",
		},
		{
			"Webinar BIDIKTAMU, 11 November 2026, 09:00 s.d. 12:00, live YouTube",
			"Webinar BIDIKTAMU, live YouTube",
		},
		{
			"Rapat jam 09:00. Peserta wajib hadir tepat waktu.",
			"Rapat",
		},
		{
			"Rabu, 11 November 2026 jam 09:00 via Zoom",
			"",
		},
	}
	for _, tc := range cases {
		p := parseAgendaText(tc.text, agendaRef)
		if got := titleFromText(tc.text, p.spans); got != tc.want {
			t.Fatalf("title %q: got %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestExtractAgendaGroup(t *testing.T) {
	cfg := defaultConfig().Agenda
	row := EvidenceRow{
		MessageID: "a1",
		Sources: []RowSource{
			{MessageID: "a1", ChatJID: groupA, Sender: "sender:00000001", Timestamp: "2026-11-11T02:05:00Z"},
			{MessageID: "a2", ChatJID: groupB, Sender: "sender:00000002", Timestamp: "2026-11-11T02:20:00Z"},
		},
	}
	mk := func(id, chat, content string, ts time.Time) Message {
		return Message{ID: id, ChatJID: chat, Content: content, Timestamp: ts}
	}
	base := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)

	// Merged cross-post pair: one candidate, two sources, canonical text.
	g := messageGroup{
		members: []Message{
			mk("a1", groupA, "Undangan Rapat Sinkronisasi STRANAS PPDT, Rabu 11 November 2026 jam 09:00-12:00 via Zoom https://zoom.us/j/5551112222", base.Add(5*time.Minute)),
			mk("a2", groupB, "Rapat sinkronisasi STRANAS PPDT jam 9-12 hari ini, Zoom https://zoom.us/j/5551112222", base.Add(20*time.Minute)),
		},
		canonical: 0,
		notes:     map[string]string{},
	}
	cand := extractAgenda(g, row, cfg, true)
	if cand == nil {
		t.Fatal("merged pair produced no candidate")
	}
	if cand.Status != "lengkap" || cand.Date != "2026-11-11" || cand.Start != "09:00" || cand.End == nil || *cand.End != "12:00" {
		t.Fatalf("candidate = %+v", cand)
	}
	if cand.DurationMin == nil || *cand.DurationMin != 180 {
		t.Fatalf("duration = %v, want 180", cand.DurationMin)
	}
	if cand.Title != "Rapat Sinkronisasi STRANAS PPDT" {
		t.Fatalf("title = %q", cand.Title)
	}
	if cand.Location != "Zoom" || cand.Link != "https://zoom.us/j/5551112222" {
		t.Fatalf("location/link = %q / %q", cand.Location, cand.Link)
	}
	if len(cand.Sources) != 2 || cand.RowMessageID != "a1" {
		t.Fatalf("sources = %v", cand.Sources)
	}

	// Canonical has only a time; the other member has the date: the
	// date is filled in when the members agree on one value.
	g2 := messageGroup{
		members: []Message{
			mk("b1", groupA, "Ralat: rapat koordinasi tim data diundur jadi jam 13:00, tetap di ruang rapat utama", base.Add(75*time.Minute)),
			mk("b2", groupA, "Rapat koordinasi tim data, Kamis 12 November jam 10:00, ruang rapat utama", base.Add(60*time.Minute)),
		},
		canonical:  0,
		correction: true,
		notes:      map[string]string{},
	}
	row2 := EvidenceRow{MessageID: "b1", Sources: []RowSource{
		{MessageID: "b1", ChatJID: groupA, Sender: "sender:00000001", Timestamp: "2026-11-11T03:15:00Z"},
		{MessageID: "b2", ChatJID: groupA, Sender: "sender:00000002", Timestamp: "2026-11-11T03:00:00Z"},
	}}
	cand2 := extractAgenda(g2, row2, cfg, true)
	if cand2 == nil {
		t.Fatal("correction group produced no candidate")
	}
	if cand2.Date != "2026-11-12" || cand2.Start != "13:00" {
		t.Fatalf("candidate2 = %+v (want date 2026-11-12, start 13:00)", cand2)
	}
	if cand2.Location != "ruang rapat utama" {
		t.Fatalf("candidate2 location = %q", cand2.Location)
	}

	// "sampai selesai": the end is assumed, flagged, 120 minutes.
	g3 := messageGroup{
		members: []Message{
			mk("c1", groupA, "Rapat 12 November 2026 pukul 13:00 s.d. selesai, Ruang Rapat Utama", base.Add(30*time.Minute)),
		},
		canonical: 0,
		notes:     map[string]string{},
	}
	row3 := EvidenceRow{MessageID: "c1", Sources: []RowSource{
		{MessageID: "c1", ChatJID: groupA, Sender: "sender:00000001", Timestamp: "2026-11-11T02:30:00Z"},
	}}
	cand3 := extractAgenda(g3, row3, cfg, true)
	if cand3 == nil || cand3.End == nil || *cand3.End != "15:00" || !cand3.EndAssumed {
		t.Fatalf("candidate3 = %+v", cand3)
	}
	if cand3.DurationMin == nil || *cand3.DurationMin != 120 {
		t.Fatalf("candidate3 duration = %v, want 120", cand3.DurationMin)
	}
	if !strings.Contains(cand3.Note, "asumsi 2 jam") {
		t.Fatalf("candidate3 note = %q, want the 2h assumption noted", cand3.Note)
	}

	// Incomplete: date only, and time only.
	g4 := messageGroup{
		members: []Message{
			mk("d1", groupA, "Rapat koordinasi tim data tanggal 12 November", base.Add(60*time.Minute)),
		},
		canonical: 0,
		notes:     map[string]string{},
	}
	row4 := EvidenceRow{MessageID: "d1", Sources: []RowSource{
		{MessageID: "d1", ChatJID: groupA, Sender: "sender:00000001", Timestamp: "2026-11-11T03:00:00Z"},
	}}
	cand4 := extractAgenda(g4, row4, cfg, true)
	if cand4 == nil || cand4.Status != "belum lengkap" || len(cand4.Missing) != 1 || cand4.Missing[0] != "jam" {
		t.Fatalf("candidate4 = %+v", cand4)
	}
	g5 := messageGroup{
		members: []Message{
			mk("e1", groupB, "Rapat jam 15:00", base.Add(70*time.Minute)),
		},
		canonical: 0,
		notes:     map[string]string{},
	}
	row5 := EvidenceRow{MessageID: "e1", Sources: []RowSource{
		{MessageID: "e1", ChatJID: groupB, Sender: "sender:00000002", Timestamp: "2026-11-11T03:10:00Z"},
	}}
	cand5 := extractAgenda(g5, row5, cfg, true)
	if cand5 == nil || cand5.Status != "belum lengkap" || len(cand5.Missing) != 1 || cand5.Missing[0] != "tanggal" {
		t.Fatalf("candidate5 = %+v", cand5)
	}

	// No date and no time anywhere: no candidate at all.
	g6 := messageGroup{
		members: []Message{
			mk("f1", dmA, "Selamat pagi semuanya, semangat hari ini!", base.Add(80*time.Minute)),
		},
		canonical: 0,
		notes:     map[string]string{},
	}
	row6 := EvidenceRow{MessageID: "f1", Sources: []RowSource{
		{MessageID: "f1", ChatJID: dmA, Sender: "sender:00000003", Timestamp: "2026-11-11T03:20:00Z"},
	}}
	if cand6 := extractAgenda(g6, row6, cfg, true); cand6 != nil {
		t.Fatalf("greeting produced a candidate: %+v", cand6)
	}

	// Redaction: a phone-like number in the title/location is masked
	// when redact=true; the meeting link is NOT redacted (it is the
	// join link).
	g7 := messageGroup{
		members: []Message{
			mk("g1", groupA, "Rapat koordinasi hubungi 081234567890, 11 November jam 09:00", base.Add(90*time.Minute)),
		},
		canonical: 0,
		notes:     map[string]string{},
	}
	row7 := EvidenceRow{MessageID: "g1", Sources: []RowSource{
		{MessageID: "g1", ChatJID: groupA, Sender: "sender:00000001", Timestamp: "2026-11-11T03:30:00Z"},
	}}
	cand7 := extractAgenda(g7, row7, cfg, true)
	if cand7 == nil {
		t.Fatal("phone-in-title message produced no candidate")
	}
	if strings.Contains(cand7.Title, "081234567890") {
		t.Fatalf("title not redacted: %q", cand7.Title)
	}
}

func TestAgendaConfigValidation(t *testing.T) {
	cfg := defaultConfig()
	cfg.Collection.Allowlist = []string{groupA}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default agenda config rejected: %v", err)
	}
	bad := *cfg
	bad.Agenda.UntilFinishedMinutes = 0
	if err := bad.Validate(); err == nil {
		t.Fatal("until_finished_minutes=0 accepted")
	}
	bad = *cfg
	bad.Agenda.Timezone = "UTC"
	if err := bad.Validate(); err == nil {
		t.Fatal("timezone UTC accepted")
	}
	ok := *cfg
	ok.Agenda.Timezone = "Asia/Jakarta"
	if err := ok.Validate(); err != nil {
		t.Fatalf("timezone Asia/Jakarta rejected: %v", err)
	}
	off := *cfg
	off.Agenda.Enabled = false
	off.Agenda.UntilFinishedMinutes = 0
	off.Agenda.Timezone = "bogus"
	if err := off.Validate(); err != nil {
		t.Fatalf("disabled agenda section still validated: %v", err)
	}
}

func TestCandidateToAgendaRow(t *testing.T) {
	end := "12:00"
	dur := 180
	c := AgendaCandidate{
		Title: "Rapat Sinkronisasi STRANAS PPDT", Date: "2026-11-11",
		Start: "09:00", End: &end, DurationMin: &dur,
		Location: "Zoom", Link: "https://zoom.us/j/5551112222",
		Status: "lengkap", RowMessageID: "a1",
		Sources: []AgendaSource{
			{MessageID: "a1", ChatJID: groupA, Sender: "sender:00000001"},
			{MessageID: "a2", ChatJID: groupB, Sender: "sender:00000002"},
		},
	}
	row := CandidateToAgendaRow(c)
	want := []string{
		"Rapat Sinkronisasi STRANAS PPDT", "2026-11-11", "09:00", "12:00", "180",
		"Zoom", "https://zoom.us/j/5551112222", "lengkap", "",
		ChatShort(groupA) + " (a1), " + ChatShort(groupB) + " (a2)", "",
	}
	if len(row) != len(want) {
		t.Fatalf("row = %v (%d cols, want %d)", row, len(row), len(want))
	}
	for i := range want {
		if row[i] != want[i] {
			t.Fatalf("col %d = %q, want %q (row %v)", i, row[i], want[i], row)
		}
	}
	// An incomplete candidate with an unknown end: empty end/duration.
	c2 := AgendaCandidate{
		Title: "Rapat koordinasi", Date: "2026-11-12", Start: "",
		Status: "belum lengkap", Missing: []string{"jam"},
		Sources: []AgendaSource{{MessageID: "d1", ChatJID: groupA}},
	}
	row2 := CandidateToAgendaRow(c2)
	if row2[2] != "" || row2[3] != "" || row2[4] != "" || row2[8] != "jam" {
		t.Fatalf("incomplete row = %v", row2)
	}
}
