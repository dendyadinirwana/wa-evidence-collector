package collector

// Agenda extraction: parse date, time, location, and title from message
// text into agenda candidates.
//
// One candidate per message group (the semantic-dedup grouping is
// reused: the same event mentioned in several messages is ONE candidate
// with all sources listed, not one candidate per message). The
// canonical message's text is parsed first; a date or time missing
// there is filled from another member only when every member that has
// one agrees on a single value.
//
// Rules (the operator's standing agenda-scan rules):
//   - A candidate MUST have a date and a time. If either is missing
//     the candidate is "belum lengkap" and records what is missing;
//     a date or time is NEVER guessed or invented. A message with
//     neither a date nor a time produces no candidate at all.
//   - "Sampai selesai" / "s.d. selesai" with no end time means the
//     event runs UntilFinishedMinutes after the start (default 2
//     hours). The assumed end is flagged (EndAssumed + a note);
//     it is an assumption, not a fact from the text.
//   - A single start time with no end and no "sampai selesai" leaves
//     the end unknown (null). Nothing is invented.
//   - Relative dates ("hari ini", "besok", "lusa", "Senin depan",
//     "Jumat ini") resolve against the MESSAGE's own timestamp (in
//     WIB), never against today. A relative resolution is noted on
//     the candidate so the reader can see where the date came from.
//   - A date without a year resolves to the message's year; a day
//     number without a month ("tanggal 11") resolves to the message's
//     month. No rollover is applied: a date that lands in the past
//     is reported as written, not pushed to the next year/month.
//
// Parsing is pure Go, offline, no ML: regexes over Indonesian date
// and time forms (see the format list in README.md). The parser runs
// on the RAW message text (like semantic dedup): redaction would
// destroy the Zoom-ID/link signals. Title and location are redacted
// on export when redact=true; the link is exported raw (it is the
// meeting join link, and masking it would destroy its purpose).
//
// No calendar events are created here. Producing candidates is the
// scope; the operator selects candidates first, then creates events.
// Calendar integration is a documented next step.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// AgendaCandidate is one event parsed from a message group.
type AgendaCandidate struct {
	// Title is the first line/sentence of the canonical message,
	// cleaned of date/time/link fragments. "" when the message
	// carries no usable title (date/time/location only).
	Title string `json:"title"`
	// Date is the event date in WIB, "YYYY-MM-DD". "" when missing.
	Date string `json:"date"`
	// Start is the event start time (WIB), "HH:MM". "" when missing.
	Start string `json:"start"`
	// End is the event end time (WIB), "HH:MM". null when unknown.
	End *string `json:"end"`
	// EndAssumed is true when End was computed from the "sampai
	// selesai" rule (start + UntilFinishedMinutes), not read from
	// the text. The Note says so too.
	EndAssumed bool `json:"end_assumed,omitempty"`
	// DurationMin is the event length in minutes. null when the end
	// is unknown.
	DurationMin *int `json:"duration_minutes"`
	// Location is the venue / platform ("Ruang Rapat Utama", "Zoom").
	// "" when the text names none.
	Location string `json:"location,omitempty"`
	// Link is the meeting link when the text carries one (a Zoom /
	// Meet / Teams / Webex link first, else the first link). Raw:
	// it is the join link, so it is NOT redacted. "" when none.
	Link string `json:"link,omitempty"`
	// Status is "lengkap" (date + time) or "belum lengkap".
	Status string `json:"status"`
	// Missing lists what is missing when Status is "belum lengkap":
	// "tanggal" and/or "jam".
	Missing []string `json:"missing,omitempty"`
	// Note records assumptions and resolutions ("tanggal dari
	// 'besok' (pesan 11 November 2026)", "jam selesai asumsi 2 jam
	// (s.d. selesai)", "jam selesai lewat tengah malam").
	Note string `json:"note,omitempty"`
	// RowMessageID is the evidence row behind this candidate (the
	// canonical message ID), so the candidate links back to the
	// exported row.
	RowMessageID string `json:"row_message_id"`
	// Sources are the messages behind the candidate (one per group
	// member). Senders are redacted exactly like the row's sources
	// when redact=true.
	Sources []AgendaSource `json:"sources"`
}

// AgendaSource is one message behind a candidate.
type AgendaSource struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
	ChatName  string `json:"chat_name"`
	Sender    string `json:"sender"`
	Timestamp string `json:"timestamp"` // RFC3339 UTC
}

// AgendaFile is the per-run agenda export envelope.
type AgendaFile struct {
	Version     int               `json:"version"`
	Label       string            `json:"label"`
	WindowStart string            `json:"window_start"`
	WindowEnd   string            `json:"window_end"`
	GeneratedAt string            `json:"generated_at"`
	Candidates  []AgendaCandidate `json:"candidates"`
}

// AgendaFileName is Agenda-<YYYYMMDD>.json for a run date.
func AgendaFileName(t time.Time) string {
	return "Agenda-" + t.Format("20060102") + ".json"
}

// WriteAgendaFile writes the agenda envelope as indented JSON. A nil
// candidate list is written as [] (the file proves extraction ran).
func WriteAgendaFile(path string, env AgendaFile) error {
	if env.Candidates == nil {
		env.Candidates = []AgendaCandidate{}
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

var (
	// "jam/pukul" + hour, with an optional qualifier ("jam 7 malam").
	agendaHourRe = regexp.MustCompile(`(?i)\b(?:jam|pukul)\s+(\d{1,2})(?:[:.](\d{2}))?\s*(pagi|siang|sore|malam)?\b`)
	// Month-name date with a captured year: 11 November 2026, 11 Nov.
	// (dedup's dateMonthRe does not capture the year; agenda needs it.)
	agendaMonthDateRe = regexp.MustCompile(`(?i)\b(\d{1,2})\s+(januari|jan|februari|feb|maret|mar|april|apr|mei|juni|jun|juli|jul|agustus|agu|september|sep|oktober|okt|november|nov|desember|des)(?:\s+(\d{4}))?\b`)
	// Dotted date: 11.11.2026, 11.11 (day.month[.year], Indonesian
	// order). A match followed by ".<digit>" is a version number
	// ("Versi 2.7.1"), not a date; it is skipped at match time.
	agendaDotDateRe = regexp.MustCompile(`\b(\d{1,2})\.(\d{1,2})(?:\.(\d{2,4}))?\b`)
	// "sampai selesai" / "s.d. selesai" / "- selesai": the event
	// runs until finished (see UntilFinishedMinutes).
	agendaUntilFinishedRe = regexp.MustCompile(`(?i)(?:s\.?\s*d\.?|s/d|sampai|hingga|-)\s*selesai\b`)
	// A range connector between two times: "-", "s.d.", "sampai", ...
	agendaRangeConnectorRe = regexp.MustCompile(`(?i)(?:^|[\s,])(?:-|–|—|s\.?\s*d\.?|s/d|sd|sampai|hingga)(?:[\s,]|$)`)
	// Relative dates (resolved against the message timestamp).
	agendaTodayRe      = regexp.MustCompile(`(?i)\bhari\s+ini\b`)
	agendaTomorrowRe   = regexp.MustCompile(`(?i)\bbesok\b`)
	agendaDayAfterRe   = regexp.MustCompile(`(?i)\blusa\b`)
	agendaYesterdayRe  = regexp.MustCompile(`(?i)\bkemarin\b`)
	agendaWeekdayNextW = regexp.MustCompile(`(?i)\b(senin|selasa|rabu|kamis|jumat|jum'at|sabtu|minggu|ahad)\s+(?:minggu|pekan)\s+depan\b`)
	// "Senin depan", "Jumat depan": that weekday next week. "Minggu
	// depan" is NOT here: bare "minggu depan" means "next week", not
	// "next Sunday" (see agendaNextWeekRe below).
	agendaWeekdayDepan = regexp.MustCompile(`(?i)\b(senin|selasa|rabu|kamis|jumat|jum'at|sabtu)\s+depan\b`)
	agendaWeekdayIni   = regexp.MustCompile(`(?i)\b(senin|selasa|rabu|kamis|jumat|jum'at|sabtu|minggu|ahad)\s+ini\b`)
	agendaWeekdayBare  = regexp.MustCompile(`(?i)\b(senin|selasa|rabu|kamis|jumat|jum'at|sabtu|minggu|ahad)\b`)
	// Bare "minggu depan" / "pekan depan" ("next week"): Monday of
	// next week. The day within the week is unknown, so the week's
	// start is used and the note says where the date came from.
	agendaNextWeekRe = regexp.MustCompile(`(?i)\b(?:minggu|pekan)\s+depan\b`)
	agendaDayNumRe   = regexp.MustCompile(`(?i)\bt(?:an)?ggal\s+(\d{1,2})\b`)
	// Location markers.
	agendaPlaceRe = regexp.MustCompile(`(?i)(?:tempat|lokasi)\s*:\s*([^\n,;]+)`)
	agendaViaRe   = regexp.MustCompile(`(?i)\bvia\s+(zoom|google\s*meet|meet|microsoft\s*teams|teams|webex)\b`)
	agendaDiRe    = regexp.MustCompile(`(?i)\bdi\s+([^\n,;]{2,80})`)
	agendaVenueRe = regexp.MustCompile(`(?i)\b(ruang|aula|gedung|hotel|kantor|lapangan|masjid|balai|kampus|sekolah|universitas)\s+[^\n,;]{1,60}`)
	// A single time preceded by "s.d." / "sampai" is an END time,
	// not a start ("Rapat s.d. jam 12:00"): the start is unknown.
	agendaEndOnlyRe = regexp.MustCompile(`(?i)(?:s\.?\s*d\.?|s/d|sampai|hingga)\s+(?:jam|pukul)?\s*$`)
)

// weekdayNum maps Indonesian weekday names to ISO weekday numbers
// (Monday 1 .. Sunday 7). "jum'at" is the apostrophized "jumat";
// "ahad" is Sunday.
var weekdayNum = map[string]int{
	"senin": 1, "selasa": 2, "rabu": 3, "kamis": 4, "jumat": 5,
	"jum'at": 5, "sabtu": 6, "minggu": 7, "ahad": 7,
}

// agendaVenueWords are lowercase words that may start a place after
// "di" ("di ruang rapat", "di aula"). Anything else after "di" must
// start with a capital letter to count as a place.
var agendaVenueWords = map[string]struct{}{
	"ruang": {}, "aula": {}, "gedung": {}, "hotel": {}, "kantor": {},
	"lapangan": {}, "masjid": {}, "balai": {}, "kampus": {}, "sekolah": {},
	"universitas": {}, "taman": {}, "mall": {}, "pusat": {}, "studio": {},
	"hall": {}, "zoom": {}, "meet": {}, "cafe": {}, "kafe": {},
	"restoran": {}, "rumah": {},
}

// agendaDiExclusions are words after "di" that are never a place
// ("di sini", "di tanggal 11", "di grup ini").
var agendaDiExclusions = map[string]struct{}{
	"sini": {}, "situ": {}, "sana": {}, "atas": {}, "bawah": {},
	"depan": {}, "belakang": {}, "luar": {}, "dalam": {},
	"tanggal": {}, "tgl": {}, "hari": {}, "jam": {}, "pukul": {},
	"grup": {}, "channel": {}, "link": {}, "tempat": {}, "lokasi": {},
	"mana": {}, "samping": {},
}

// agendaEventWords are whole words that mark a message as being
// about an event. A relative date ("hari ini", "besok", ...) only
// counts as an event date when the message also carries a time or
// one of these words: without either, "semangat hari ini!" is a
// greeting, not an agenda item.
var agendaEventWords = map[string]struct{}{
	"rapat": {}, "undangan": {}, "acara": {}, "kegiatan": {},
	"webinar": {}, "workshop": {}, "pelatihan": {}, "diskusi": {},
	"sosialisasi": {}, "forum": {}, "pertemuan": {}, "upacara": {},
	"apel": {}, "seminar": {}, "sidang": {}, "musyawarah": {},
	"koordinasi": {}, "kunjungan": {}, "pelantikan": {}, "evaluasi": {},
	"monitoring": {}, "briefing": {}, "peringatan": {}, "ujian": {},
	"seleksi": {}, "wawancara": {}, "peresmian": {}, "peluncuran": {},
	"pameran": {}, "festival": {}, "lomba": {}, "ibadah": {},
	"pengajian": {}, "syukuran": {}, "perayaan": {}, "serah": {},
}

// hasAgendaEventWord reports whether the lowercased text carries a
// whole-word event word.
func hasAgendaEventWord(lower string) bool {
	for _, tok := range strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if _, ok := agendaEventWords[tok]; ok {
			return true
		}
	}
	return false
}

// agendaTitleStrip are leading words dropped from a title line
// (greetings, salutations, message-kind labels, correction markers).
var agendaTitleStrip = []string{
	"selamat pagi", "selamat siang", "selamat sore", "selamat malam",
	"assalamualaikum", "wr", "wb", "salam", "halo", "hai", "hello", "hi",
	"yth", "dear", "kepada", "bapak/ibu", "bapak", "ibu", "saudara",
	"teman-teman", "temanteman", "teman", "mohon",
	"undangan", "pengumuman", "pemberitahuan", "info", "informasi",
	"diteruskan", "forwarded", "ralat", "koreksi", "revisi",
}

// isoWeekday is the ISO weekday number (Monday 1 .. Sunday 7).
func isoWeekday(t time.Time) int {
	return (int(t.Weekday())+6)%7 + 1
}

// validDate reports whether y/m/d is a real calendar date.
func validDate(y, m, d int) bool {
	if m < 1 || m > 12 || d < 1 || d > 31 {
		return false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return t.Year() == y && int(t.Month()) == m && t.Day() == d
}

// resolveYear turns a 2-digit year into 2000+yy; a 4-digit year must be
// in 2000-2099. Anything else is rejected (a version number, a room
// number) rather than guessed.
func resolveYear(y string) (int, bool) {
	n, err := strconv.Atoi(y)
	if err != nil {
		return 0, false
	}
	switch len(y) {
	case 2:
		return 2000 + n, true
	case 4:
		if n >= 2000 && n <= 2099 {
			return n, true
		}
	}
	return 0, false
}

// meetingPlatform names the meeting platform of a link, or "".
func meetingPlatform(link string) string {
	switch {
	case strings.Contains(link, "zoom.us/"):
		return "Zoom"
	case strings.Contains(link, "meet.google.com"):
		return "Google Meet"
	case strings.Contains(link, "teams.microsoft.com"), strings.Contains(link, "teams.live.com"):
		return "Microsoft Teams"
	case strings.Contains(link, "webex.com"):
		return "Webex"
	default:
		return ""
	}
}

// agendaTime is one extracted event time: minutes since midnight plus
// its span in the text (for range detection and title cleaning).
type agendaTime struct {
	mins  int
	start int // byte offset of the match start
	end   int // byte offset of the match end
}

// agendaDate is one extracted event date plus its span.
type agendaDate struct {
	date  time.Time // midnight in WIB
	start int
	end   int
}

// agendaParts is the parsed date/time/location of ONE message.
type agendaParts struct {
	date          time.Time
	hasDate       bool
	start, end    int
	hasStart      bool
	hasEnd        bool
	endOnly       bool // the single time is an end ("s.d. jam 12:00")
	untilFinished bool
	overnight     bool
	location      string
	link          string
	relativeNote  string
	spans         [][]int // consumed spans (dates, times, links, relative)
}

// parseMinutesOK is parseMinutes (dedup.go) plus the hour qualifier
// ("jam 7 malam" = 19:00, "jam 2 siang" = 14:00, "jam 12 malam" =
// 00:00). A 24-hour time ("09:00") ignores the qualifier.
func adjustHour(h int, qual string) int {
	switch strings.ToLower(qual) {
	case "pagi":
		return h
	case "siang", "sore":
		if h < 12 {
			return h + 12
		}
		return h
	case "malam":
		if h == 12 {
			return 0
		}
		if h < 12 {
			return h + 12
		}
		return h
	}
	return h
}

// collectHourTimes finds "jam/pukul" times in w ("jam 9", "pukul
// 07.00 WIB", "jam 7 malam"). Matches are returned in text order.
func collectHourTimes(w string) []agendaTime {
	var out []agendaTime
	for _, loc := range agendaHourRe.FindAllStringIndex(w, -1) {
		parts := agendaHourRe.FindStringSubmatch(w[loc[0]:loc[1]])
		if len(parts) != 4 {
			continue
		}
		mins := "0"
		if parts[2] != "" {
			mins = parts[2]
		}
		h, m := 0, 0
		if _, err := fmt.Sscanf(parts[1], "%d", &h); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(mins, "%d", &m); err != nil {
			continue
		}
		h = adjustHour(h, parts[3])
		if h < 0 || h > 23 || m < 0 || m > 59 {
			continue
		}
		out = append(out, agendaTime{mins: h*60 + m, start: loc[0], end: loc[1]})
	}
	return out
}

// collectBareTimes finds bare "09:00" / "09.00" times in w. A match
// followed by ".<digit>" is a dotted date ("11.11"), not a time.
func collectBareTimes(w string) []agendaTime {
	var out []agendaTime
	for _, loc := range timeRe.FindAllStringIndex(w, -1) {
		if loc[1] < len(w) && w[loc[1]] == '.' &&
			loc[1]+1 < len(w) && w[loc[1]+1] >= '0' && w[loc[1]+1] <= '9' {
			continue
		}
		parts := timeRe.FindStringSubmatch(w[loc[0]:loc[1]])
		if len(parts) != 3 {
			continue
		}
		if mins, ok := parseMinutes(parts[1], parts[2]); ok {
			// A qualifier right after a bare time ("09:00 malam").
			if qm := regexp.MustCompile(`^\s*(pagi|siang|sore|malam)\b`).FindStringSubmatch(w[loc[1]:]); qm != nil {
				mins = adjustHour(mins/60, qm[1])*60 + mins%60
			}
			out = append(out, agendaTime{mins: mins, start: loc[0], end: loc[1]})
		}
	}
	return out
}

// parseDottedDate validates one dotted-date match ("11.11.2026" or
// "11.11"). The year defaults to the reference year when absent.
func parseDottedDate(parts []string, ref time.Time) (time.Time, bool) {
	day, err1 := strconv.Atoi(parts[1])
	month, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return time.Time{}, false
	}
	year := ref.Year()
	if parts[3] != "" {
		y, ok := resolveYear(parts[3])
		if !ok {
			return time.Time{}, false
		}
		year = y
	}
	if !validDate(year, month, day) {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, wib), true
}

// parseNumericDate validates one slash/dash date match ("11/11/2026",
// "11/11", "11-11-26"). Day first (Indonesian order).
func parseNumericDate(parts []string, ref time.Time) (time.Time, bool) {
	day, err1 := strconv.Atoi(parts[1])
	month, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return time.Time{}, false
	}
	year := ref.Year()
	if parts[3] != "" {
		y, ok := resolveYear(parts[3])
		if !ok {
			return time.Time{}, false
		}
		year = y
	}
	if !validDate(year, month, day) {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, wib), true
}

// parseMonthDate validates one month-name date match ("11 November
// 2026", "11 Nov"). The year defaults to the reference year.
func parseMonthDate(parts []string, ref time.Time) (time.Time, bool) {
	day, err := strconv.Atoi(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	month := monthNum[strings.ToLower(parts[2])]
	year := ref.Year()
	if len(parts) > 3 && parts[3] != "" {
		y, err := strconv.Atoi(parts[3])
		if err != nil || y < 2000 || y > 2099 {
			return time.Time{}, false
		}
		year = y
	}
	if !validDate(year, month, day) {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, wib), true
}

// extendDateSpanLeft extends a date match leftwards to include a
// weekday name directly before it ("Rabu, 11 November 2026" -> the
// whole span) or a "tanggal"/"tgl" marker ("tanggal 12 November" ->
// the whole span), so neither leaks into the title.
func extendDateSpanLeft(text string, start int) int {
	before := text[:start]
	best := start
	if m := agendaWeekdayBeforeRe.FindStringIndex(before); m != nil {
		best = m[0]
	}
	if m := agendaTanggalBeforeRe.FindStringIndex(before); m != nil && m[0] < best {
		best = m[0]
	}
	return best
}

var (
	agendaWeekdayBeforeRe = regexp.MustCompile(`(?i)(senin|selasa|rabu|kamis|jumat|jum'at|sabtu|minggu|ahad),?\s*$`)
	agendaTanggalBeforeRe = regexp.MustCompile(`(?i)(?:pada|di)?\s*(?:tanggal|tgl)\s*$`)
)

// resolveWeekday returns the date of the given ISO weekday: "ini"
// (this week), "depan" (next week), or the next occurrence on/after
// the reference date (bare weekday).
func resolveWeekday(ref time.Time, wd int, mode string) time.Time {
	// Monday of the reference week (WIB dates).
	refDay := time.Date(ref.Year(), ref.Month(), ref.Day(), 0, 0, 0, 0, wib)
	monday := refDay.AddDate(0, 0, -(isoWeekday(refDay) - 1))
	switch mode {
	case "ini":
		return monday.AddDate(0, 0, wd-1)
	case "depan":
		return monday.AddDate(0, 0, 7+wd-1)
	default: // bare weekday: next occurrence on or after ref
		diff := (wd - isoWeekday(refDay) + 7) % 7
		return refDay.AddDate(0, 0, diff)
	}
}

// normWeekday lowercases a weekday word and strips apostrophes
// ("jum'at" -> "jumat") for the weekdayNum lookup.
func normWeekday(w string) string {
	w = strings.ToLower(w)
	w = strings.ReplaceAll(w, "'", "")
	w = strings.ReplaceAll(w, "’", "")
	return w
}

// relDate is one relative-date match with its resolved date.
type relDate struct {
	start, end int
	date       time.Time
	word       string // the matched phrase, e.g. "besok"
}

// collectRelativeDates finds every relative-date expression in w
// (already explicit-date-blanked) and resolves it against ref.
// Matches are returned in text order, overlaps dropped (the longest
// match wins, so "Senin minggu depan" beats bare "Senin").
func collectRelativeDates(w string, ref time.Time) []relDate {
	type cand struct {
		start, end int
		date       time.Time
		word       string
	}
	var found []cand
	add := func(re *regexp.Regexp, resolve func(m []string) (time.Time, string)) {
		for _, loc := range re.FindAllStringIndex(w, -1) {
			parts := re.FindStringSubmatch(w[loc[0]:loc[1]])
			if d, word := resolve(parts); !d.IsZero() {
				found = append(found, cand{loc[0], loc[1], d, word})
			}
		}
	}
	refDay := time.Date(ref.Year(), ref.Month(), ref.Day(), 0, 0, 0, 0, wib)
	add(agendaTodayRe, func(m []string) (time.Time, string) { return refDay, "hari ini" })
	add(agendaTomorrowRe, func(m []string) (time.Time, string) { return refDay.AddDate(0, 0, 1), "besok" })
	add(agendaDayAfterRe, func(m []string) (time.Time, string) { return refDay.AddDate(0, 0, 2), "lusa" })
	add(agendaYesterdayRe, func(m []string) (time.Time, string) { return refDay.AddDate(0, 0, -1), "kemarin" })
	wd := func(m []string) (int, bool) {
		if len(m) < 2 {
			return 0, false
		}
		n, ok := weekdayNum[normWeekday(m[1])]
		return n, ok
	}
	add(agendaWeekdayNextW, func(m []string) (time.Time, string) {
		if n, ok := wd(m); ok {
			return resolveWeekday(refDay, n, "depan"), strings.ToLower(m[0])
		}
		return time.Time{}, ""
	})
	add(agendaWeekdayDepan, func(m []string) (time.Time, string) {
		if n, ok := wd(m); ok {
			return resolveWeekday(refDay, n, "depan"), strings.ToLower(m[0])
		}
		return time.Time{}, ""
	})
	// Bare "minggu depan" / "pekan depan": Monday of next week.
	add(agendaNextWeekRe, func(m []string) (time.Time, string) {
		return resolveWeekday(refDay, 1, "depan"), strings.ToLower(m[0])
	})
	add(agendaWeekdayIni, func(m []string) (time.Time, string) {
		if n, ok := wd(m); ok {
			return resolveWeekday(refDay, n, "ini"), strings.ToLower(m[0])
		}
		return time.Time{}, ""
	})
	add(agendaWeekdayBare, func(m []string) (time.Time, string) {
		if n, ok := wd(m); ok {
			return resolveWeekday(refDay, n, ""), strings.ToLower(m[0])
		}
		return time.Time{}, ""
	})
	// Bare day number ("tanggal 11", "tgl 11"): the reference month.
	add(agendaDayNumRe, func(m []string) (time.Time, string) {
		if len(m) < 2 {
			return time.Time{}, ""
		}
		d, err := strconv.Atoi(m[1])
		if err != nil || !validDate(refDay.Year(), int(refDay.Month()), d) {
			return time.Time{}, ""
		}
		return time.Date(refDay.Year(), refDay.Month(), d, 0, 0, 0, 0, wib), strings.ToLower(m[0])
	})
	sort.Slice(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		return found[i].end > found[j].end
	})
	var out []relDate
	lastEnd := -1
	for _, f := range found {
		if f.start < lastEnd {
			continue
		}
		out = append(out, relDate{f.start, f.end, f.date, f.word})
		lastEnd = f.end
	}
	return out
}

// extractLocation finds the venue in the (date/time/link-blanked)
// text, most specific first: "tempat:/lokasi:", "via <platform>",
// "di <Place>", a bare venue phrase, then the meeting-link platform.
// It returns the location and the span to blank from the title:
// for "tempat:" only the marker is blanked (the venue name stays in
// the title), for "via <platform>" the whole phrase is blanked, and
// for "di <Place>" / bare venues / link platforms nothing is blanked
// (the venue reads fine as part of a title).
func extractLocation(w string, linkPlatform string) (string, []int) {
	if m := agendaPlaceRe.FindStringSubmatchIndex(w); m != nil {
		loc := strings.TrimSpace(w[m[2]:m[3]])
		loc = strings.Join(strings.Fields(loc), " ")
		if loc != "" {
			return loc, []int{m[0], m[2]}
		}
	}
	if m := agendaViaRe.FindStringSubmatchIndex(w); m != nil {
		return platformName(w[m[2]:m[3]]), []int{m[0], m[1]}
	}
	if m := agendaDiRe.FindStringSubmatchIndex(w); m != nil {
		phrase := strings.TrimSpace(w[m[2]:m[3]])
		phrase = strings.Join(strings.Fields(phrase), " ")
		if loc, ok := cleanDiPhrase(phrase); ok {
			return loc, nil
		}
	}
	if m := agendaVenueRe.FindStringSubmatchIndex(w); m != nil {
		loc := strings.TrimSpace(w[m[0]:m[1]])
		loc = strings.Join(strings.Fields(loc), " ")
		return loc, nil
	}
	if linkPlatform != "" {
		return linkPlatform, nil
	}
	return "", nil
}

// platformName canonicalizes a "via <platform>" match.
func platformName(raw string) string {
	switch strings.ToLower(strings.Join(strings.Fields(raw), " ")) {
	case "zoom":
		return "Zoom"
	case "google meet", "meet":
		return "Google Meet"
	case "microsoft teams", "teams":
		return "Microsoft Teams"
	case "webex":
		return "Webex"
	default:
		return raw
	}
}

// cleanDiPhrase validates a phrase after "di": it must start with a
// capital letter or a venue word, and must not be an exclusion
// ("di sini", "di tanggal 11"). The phrase is cut at " dan " / " serta "
// and at a trailing " di " clause.
func cleanDiPhrase(phrase string) (string, bool) {
	if phrase == "" {
		return "", false
	}
	// Cut at " dan " / " serta " / " atau " (the rest is not the place).
	for _, sep := range []string{" dan ", " serta ", " atau "} {
		if i := strings.Index(strings.ToLower(phrase), sep); i >= 0 {
			phrase = strings.TrimSpace(phrase[:i])
		}
	}
	if phrase == "" {
		return "", false
	}
	words := strings.Fields(phrase)
	first := strings.ToLower(words[0])
	if _, bad := agendaDiExclusions[first]; bad {
		return "", false
	}
	r0 := []rune(words[0])[0]
	if !unicode.IsUpper(r0) {
		if _, ok := agendaVenueWords[first]; !ok {
			return "", false
		}
	}
	return phrase, true
}

// parseAgendaText parses one message's text into its date/time/
// location parts. ref is the message timestamp (WIB); relative dates
// resolve against it.
func parseAgendaText(text string, ref time.Time) agendaParts {
	var p agendaParts
	refWIB := ref.In(wib)
	work := text

	// 1. Links (blanked). A meeting link also sets the location
	// fallback and is the candidate's link; otherwise the first link.
	var firstLink, meetingLink, linkPlatform string
	for _, loc := range linkRe.FindAllStringIndex(work, -1) {
		raw := work[loc[0]:loc[1]]
		norm := normalizeLink(raw)
		if norm == "" {
			continue
		}
		p.spans = append(p.spans, []int{loc[0], loc[1]})
		if firstLink == "" {
			firstLink = norm
		}
		if plat := meetingPlatform(norm); plat != "" && meetingLink == "" {
			meetingLink, linkPlatform = norm, plat
		}
	}
	if meetingLink != "" {
		p.link = meetingLink
	} else {
		p.link = firstLink
	}
	work = blankSpans(work, linkRe.FindAllStringIndex(work, -1))

	// 2. "jam/pukul" times first: the "jam" prefix disambiguates
	// "jam 11.11" (a time) from a bare "11.11" (a date).
	times := collectHourTimes(work)
	for _, t := range times {
		p.spans = append(p.spans, []int{t.start, t.end})
	}
	work = blankSpans(work, spansOfTimes(times))

	// 3. Dotted dates ("11.11.2026", "11.11") before bare times, so
	// "11.11" is read as a date and "09.00" (invalid month) is left
	// for the time pass. 4. Bare times next, so "09:00-12:00" is read
	// as times before "00-12" can be read as a date.
	var dates []agendaDate
	for _, loc := range agendaDotDateRe.FindAllStringIndex(work, -1) {
		// Guard: a match followed by ".<digit>" is a version number.
		if loc[1] < len(work) && work[loc[1]] == '.' &&
			loc[1]+1 < len(work) && work[loc[1]+1] >= '0' && work[loc[1]+1] <= '9' {
			continue
		}
		parts := agendaDotDateRe.FindStringSubmatch(work[loc[0]:loc[1]])
		if len(parts) != 4 {
			continue
		}
		if d, ok := parseDottedDate(parts, refWIB); ok {
			start := extendDateSpanLeft(work, loc[0])
			dates = append(dates, agendaDate{date: d, start: start, end: loc[1]})
			p.spans = append(p.spans, []int{start, loc[1]})
		}
	}
	work = blankSpans(work, spansOfDates(dates))
	for _, t := range collectBareTimes(work) {
		// A bare time inside a link span is not an event time.
		overlap := false
		for _, sp := range p.spans {
			if t.start < sp[1] && sp[0] < t.end {
				overlap = true
				break
			}
		}
		if !overlap {
			times = append(times, t)
			p.spans = append(p.spans, []int{t.start, t.end})
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].start < times[j].start })
	work = blankSpans(work, spansOfTimes(times))

	// 5. Numeric dates (slash/dash), then 6. month-name dates.
	for _, loc := range dateNumRe.FindAllStringIndex(work, -1) {
		parts := dateNumRe.FindStringSubmatch(work[loc[0]:loc[1]])
		if len(parts) != 4 {
			continue
		}
		if d, ok := parseNumericDate(parts, refWIB); ok {
			start := extendDateSpanLeft(work, loc[0])
			dates = append(dates, agendaDate{date: d, start: start, end: loc[1]})
			p.spans = append(p.spans, []int{start, loc[1]})
		}
	}
	work = blankSpans(work, spansOfDates(dates))
	for _, loc := range agendaMonthDateRe.FindAllStringIndex(work, -1) {
		parts := agendaMonthDateRe.FindStringSubmatch(work[loc[0]:loc[1]])
		if len(parts) != 4 {
			continue
		}
		if d, ok := parseMonthDate(parts, refWIB); ok {
			start := extendDateSpanLeft(work, loc[0])
			dates = append(dates, agendaDate{date: d, start: start, end: loc[1]})
			p.spans = append(p.spans, []int{start, loc[1]})
		}
	}
	work = blankSpans(work, spansOfDates(dates))

	// The event date is the first explicit date in text order.
	sort.Slice(dates, func(i, j int) bool { return dates[i].start < dates[j].start })
	if len(dates) > 0 {
		p.date, p.hasDate = dates[0].date, true
	}

	// 7. Relative dates, only when no explicit date was found, and
	// only when the message also carries a time (already collected
	// above) or an event word: without either, "semangat hari ini!"
	// is a greeting, not an agenda item (see agendaEventWords).
	if !p.hasDate && (len(times) > 0 || hasAgendaEventWord(strings.ToLower(text))) {
		rels := collectRelativeDates(work, refWIB)
		if len(rels) > 0 {
			r := rels[0]
			p.date, p.hasDate = r.date, true
			p.relativeNote = fmt.Sprintf("tanggal dari %q (pesan %s)", r.word, RenderDatePill(refWIB))
			p.spans = append(p.spans, []int{r.start, r.end})
		}
	}

	// 8. Times: range, until-finished, or a single start.
	sort.Slice(times, func(i, j int) bool { return times[i].start < times[j].start })
	if len(times) == 1 {
		t := times[0]
		// A single time preceded by "s.d."/"sampai" is an END time.
		if m := agendaEndOnlyRe.FindStringIndex(text[:t.start]); m != nil {
			p.end, p.hasEnd, p.endOnly = t.mins, true, true
			p.spans = append(p.spans, []int{m[0], m[1]})
		} else if m := agendaUntilFinishedRe.FindStringIndex(text); m != nil {
			p.start, p.hasStart = t.mins, true
			p.untilFinished = true
			p.spans = append(p.spans, []int{m[0], m[1]})
		} else {
			p.start, p.hasStart = t.mins, true
		}
	} else if len(times) >= 2 {
		t0, t1 := times[0], times[1]
		gap := text[t0.end:t1.start]
		if agendaRangeConnectorRe.MatchString(gap) {
			p.start, p.hasStart = t0.mins, true
			p.end, p.hasEnd = t1.mins, true
			// Blank the connector ("-", "s.d.") from the title too.
			p.spans = append(p.spans, []int{t0.end, t1.start})
		} else {
			p.start, p.hasStart = t0.mins, true
		}
	}
	// End earlier than (or equal to) the start: overnight, or a
	// repeated time (typo) when equal.
	if p.hasStart && p.hasEnd {
		if p.end == p.start {
			p.hasEnd = false
		} else if p.end < p.start {
			p.end += 24 * 60
			p.overnight = true
		}
	}

	// 9. Location, on the blanked text (dates/times/links already
	// blanked, so a "di ... jam 09:00" phrase ends at the blank).
	if loc, span := extractLocation(work, linkPlatform); loc != "" {
		p.location = loc
		if span != nil {
			p.spans = append(p.spans, span)
		}
	}
	return p
}

func spansOfTimes(times []agendaTime) [][]int {
	out := make([][]int, 0, len(times))
	for _, t := range times {
		out = append(out, []int{t.start, t.end})
	}
	return out
}

func spansOfDates(dates []agendaDate) [][]int {
	out := make([][]int, 0, len(dates))
	for _, d := range dates {
		out = append(out, []int{d.start, d.end})
	}
	return out
}

// titleFromText derives the event title: the first non-empty line
// after blanking the consumed spans (dates, times, links) and
// stripping leading greeting / message-kind words, then its first
// sentence. "" when no line carries a title.
func titleFromText(text string, spans [][]int) string {
	lines := strings.Split(text, "\n")
	offset := 0
	for _, line := range lines {
		lineStart, lineEnd := offset, offset+len(line)
		cleaned := blankLineSpans(line, lineStart, lineEnd, spans)
		cleaned = stripLeadingWords(cleaned)
		cleaned = cleanTitleText(cleaned)
		if cleaned != "" {
			return firstSentence(cleaned)
		}
		offset = lineEnd + 1 // the "\n"
	}
	return ""
}

// blankLineSpans blanks the parts of line covered by absolute spans.
func blankLineSpans(line string, lineStart, lineEnd int, spans [][]int) string {
	b := []byte(line)
	for _, sp := range spans {
		lo := max(sp[0], lineStart) - lineStart
		hi := min(sp[1], lineEnd) - lineStart
		for i := lo; i < hi && i < len(b); i++ {
			if i >= 0 {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// stripLeadingWords repeatedly drops a leading greeting / message-kind
// word ("Undangan", "Selamat pagi", "Ralat:") from line.
func stripLeadingWords(line string) string {
	for {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return ""
		}
		lower := strings.ToLower(trimmed)
		dropped := false
		for _, w := range agendaTitleStrip {
			if lower == w {
				return ""
			}
			for _, sep := range []string{" ", ",", ".", ":", ";", "-"} {
				if strings.HasPrefix(lower, w+sep) {
					line = strings.TrimSpace(trimmed[len(w)+len(sep):])
					dropped = true
					break
				}
			}
			if dropped {
				break
			}
		}
		if !dropped {
			return trimmed
		}
	}
}

// cleanTitleText collapses whitespace, fixes comma pile-ups left by
// blanked fragments ("Rapat X, , Ruang Y" -> "Rapat X, Ruang Y"),
// and trims trailing punctuation.
func cleanTitleText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for strings.Contains(s, ", ,") {
		s = strings.ReplaceAll(s, ", ,", ",")
	}
	s = strings.ReplaceAll(s, " ,", ",")
	s = strings.ReplaceAll(s, " ;", ";")
	s = strings.Trim(s, " ,;:.-")
	return s
}

// firstSentence cuts s at the first sentence end: "!" / "?" anywhere,
// or ". " followed by an uppercase letter.
func firstSentence(s string) string {
	r := []rune(s)
	for i, ch := range r {
		if ch == '!' || ch == '?' {
			return strings.TrimSpace(string(r[:i]))
		}
		if ch == '.' && i+2 < len(r) && r[i+1] == ' ' && unicode.IsUpper(r[i+2]) {
			return strings.TrimSpace(string(r[:i]))
		}
	}
	return strings.TrimSpace(s)
}

// extractAgenda builds one candidate from a message group (the
// semantic-dedup grouping: one candidate per event, all sources
// listed). The canonical message is parsed first; a date or time
// missing there is filled from another member only when every member
// that has one agrees on a single value. It returns nil when no
// member carries a date or a time (not an agenda message at all).
// row supplies the row link (message ID + redacted sources); redact
// controls whether title/location are redacted on export.
func extractAgenda(g messageGroup, row EvidenceRow, cfg AgendaConfig, redact bool) *AgendaCandidate {
	canon := g.members[g.canonical]
	parts := make([]agendaParts, len(g.members))
	for i, m := range g.members {
		parts[i] = parseAgendaText(m.Content, m.Timestamp)
	}
	cp := parts[g.canonical]

	// Fill a missing date/time from the other members, but only when
	// they agree on exactly one value; a disagreement leaves the
	// field missing and is noted.
	var notes []string
	if cp.relativeNote != "" {
		notes = append(notes, cp.relativeNote)
	}
	if !cp.hasDate {
		dates := map[time.Time]struct{}{}
		for i, p := range parts {
			if i != g.canonical && p.hasDate {
				dates[p.date] = struct{}{}
			}
		}
		switch len(dates) {
		case 1:
			for d := range dates {
				cp.date, cp.hasDate = d, true
			}
		case 0:
			// nothing to fill
		default:
			notes = append(notes, "tanggal antar sumber berbeda")
		}
	}
	if !cp.hasStart && !cp.hasEnd {
		starts := map[int]struct{}{}
		for i, p := range parts {
			if i != g.canonical && p.hasStart {
				starts[p.start] = struct{}{}
			}
		}
		switch len(starts) {
		case 1:
			for s := range starts {
				cp.start, cp.hasStart = s, true
			}
		case 0:
		default:
			notes = append(notes, "jam antar sumber berbeda")
		}
	}
	if cp.location == "" {
		for i, p := range parts {
			if i != g.canonical && p.location != "" {
				cp.location = p.location
				break
			}
		}
	}
	if cp.link == "" {
		for i, p := range parts {
			if i != g.canonical && p.link != "" {
				cp.link = p.link
				break
			}
		}
	}

	// No date and no time in any member: not an agenda message.
	if !cp.hasDate && !cp.hasStart && !cp.hasEnd {
		return nil
	}

	cand := &AgendaCandidate{RowMessageID: row.MessageID}
	// Title from the canonical text (date/time/link fragments
	// blanked). When the canonical line carries no title, try the
	// other members' texts in order.
	cand.Title = titleFromText(canon.Content, cp.spans)
	if cand.Title == "" {
		for i, m := range g.members {
			if i == g.canonical {
				continue
			}
			if t := titleFromText(m.Content, parts[i].spans); t != "" {
				cand.Title = t
				break
			}
		}
	}
	if cp.hasDate {
		cand.Date = cp.date.Format("2006-01-02")
	}
	if cp.hasStart {
		cand.Start = fmt.Sprintf("%02d:%02d", cp.start/60, cp.start%60)
	}
	if cp.hasEnd {
		endMin := cp.end
		if endMin >= 24*60 {
			endMin -= 24 * 60
		}
		s := fmt.Sprintf("%02d:%02d", endMin/60, endMin%60)
		cand.End = &s
	}
	if cp.untilFinished {
		endMin := cp.start + cfg.UntilFinishedMinutes
		s := fmt.Sprintf("%02d:%02d", (endMin/60)%24, endMin%60)
		cand.End = &s
		cand.EndAssumed = true
		d := cfg.UntilFinishedMinutes
		notes = append(notes, fmt.Sprintf("jam selesai asumsi %s (s.d. selesai)", formatMinutesID(d)))
	}
	if cp.overnight {
		notes = append(notes, "jam selesai lewat tengah malam")
	}
	if cp.endOnly {
		// The single time was an end time: the start is unknown.
		// (hasStart is false; the end stays as parsed.)
		notes = append(notes, "hanya jam selesai yang disebut")
	}
	if cand.End != nil && cand.Start != "" {
		startMin := cp.start
		endMin := cp.end
		if cp.untilFinished {
			endMin = cp.start + cfg.UntilFinishedMinutes
		}
		d := endMin - startMin
		cand.DurationMin = &d
	}
	cand.Location = cp.location
	cand.Link = cp.link

	// Status: a candidate needs both a date and a start time.
	switch {
	case cp.hasDate && cp.hasStart:
		cand.Status = "lengkap"
	default:
		cand.Status = "belum lengkap"
		if !cp.hasDate {
			cand.Missing = append(cand.Missing, "tanggal")
		}
		if !cp.hasStart {
			cand.Missing = append(cand.Missing, "jam")
		}
	}
	cand.Note = strings.Join(notes, "; ")
	for _, s := range row.Sources {
		cand.Sources = append(cand.Sources, AgendaSource{
			MessageID: s.MessageID, ChatJID: s.ChatJID, ChatName: s.ChatName,
			Sender: s.Sender, Timestamp: s.Timestamp,
		})
	}
	if redact {
		cand.Title = RedactContent(cand.Title)
		cand.Location = RedactContent(cand.Location)
	}
	return cand
}

// formatMinutesID renders a duration in Indonesian ("2 jam", "90 menit").
func formatMinutesID(mins int) string {
	if mins%60 == 0 {
		return fmt.Sprintf("%d jam", mins/60)
	}
	return fmt.Sprintf("%d menit", mins)
}
