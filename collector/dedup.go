package collector

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Semantic dedup: a second pass after exact dedup (message ID + content
// hash). It groups messages that are the same event reworded or
// cross-posted, so one event exports as one row with all sources listed
// instead of two rows.
//
// Pure Go, offline, no ML model, no embedding API, no network: the text
// is normalized (lowercase, markup/punctuation stripped, generic words
// dropped), then messages are compared on token-set overlap (Jaccard)
// plus shared key entities (links incl. Zoom meeting IDs, explicit
// dates/times, media keys) plus time proximity. Merging is deliberately
// conservative: when in doubt, keep both rows and add a "mirip dengan
// ..." note. That is a feature, not a bug.
//
// Decision order (first match wins):
//  1. Message timestamps further apart than the time window: never merge.
//  2. Correction: the later message carries a correction marker
//     ("ralat", "koreksi", ...) and is similar enough to an earlier one
//     (token overlap >= correction threshold, or a shared link/media/
//     date): merge, flagged as a correction. A correction may change the
//     date or time, so it is checked before the conflict rules.
//  3. Date conflict: both carry explicit (different) dates: never merge;
//     note the similarity when the overlap is high enough.
//  4. Time conflict: both carry explicit (different) event times: never
//     merge; note it as "jam berbeda" when the pair is otherwise similar.
//  5. Shared Zoom meeting ID: merge (a Zoom ID identifies one meeting).
//  6. Shared media key or shared link, with supporting overlap
//     (token overlap >= note threshold, or a shared date/time): merge.
//  7. Token overlap at note level, corroborated by a shared explicit
//     date or time: merge.
//  8. Token overlap >= merge threshold: merge.
//  9. Token overlap >= note threshold: keep both, add a "mirip dengan"
//     note on both rows.
//
// Sender is a WEAK signal only and is never used here: cross-posts
// usually come from different senders, so requiring the same sender
// would defeat the feature. Grouping runs on the raw (unredacted)
// message text: redaction masks phone-like digit runs, which would
// destroy the Zoom-ID and link signals if dedup ran after it.
//
// Scope: grouping only covers the messages that are new in THIS run.
// A reworded copy that arrives in a later window cannot be matched
// against already-exported rows, because the state file holds content
// hashes only, never text. That copy exports as its own row.

// messageSig is the comparable fingerprint of one message.
type messageSig struct {
	tokens     map[string]struct{} // normalized content words
	links      map[string]struct{} // normalized URLs (query/fragment stripped)
	zoomIDs    map[string]struct{} // Zoom meeting IDs from zoom.us/j/<id>
	dates      map[string]struct{} // explicit dates as "DD-MM"
	times      map[int]struct{}    // explicit event times, minutes since midnight
	mediaKeys  map[string]struct{} // media identity (URL or filename|length)
	correction bool                // carries a correction marker
}

var (
	linkRe = regexp.MustCompile(`https?://[^\s<>"')\]]+`)
	// Numeric date: 11/11, 11-11-2026, 11/11/26. Day first (Indonesian order).
	dateNumRe = regexp.MustCompile(`\b(\d{1,2})[-/](\d{1,2})(?:[-/](\d{2,4}))?\b`)
	// Month-name date: 11 November 2026, 11 Nov 2026.
	dateMonthRe = regexp.MustCompile(`(?i)\b(\d{1,2})\s+(januari|jan|februari|feb|maret|mar|april|apr|mei|juni|jun|juli|jul|agustus|agu|september|sep|oktober|okt|november|nov|desember|des)(?:\s+\d{4})?\b`)
	// Time: 09:00, 09.00, 09:00-12:00 (each end found separately).
	timeRe = regexp.MustCompile(`\b(\d{1,2})[:.](\d{2})\b`)
	// Bare hour after jam/pukul: jam 9, pukul 9, jam 09.30.
	timeHourRe = regexp.MustCompile(`\b(?:jam|pukul)\s+(\d{1,2})(?:[:.](\d{2}))?\b`)
	zoomIDRe   = regexp.MustCompile(`zoom\.us/j/(\d+)`)
)

var monthNum = map[string]int{
	"januari": 1, "jan": 1, "februari": 2, "feb": 2, "maret": 3, "mar": 3,
	"april": 4, "apr": 4, "mei": 5, "juni": 6, "jun": 6, "juli": 7, "jul": 7,
	"agustus": 8, "agu": 8, "september": 9, "sep": 9, "oktober": 10, "okt": 10,
	"november": 11, "nov": 11, "desember": 12, "des": 12,
}

// dedupStopwords are generic words that carry no topic: function words,
// greetings, and the event-type words that appear in nearly every
// announcement ("rapat", "undangan", "zoom", ...). Dropping them keeps
// the token overlap about the topic, not about the message being an
// invitation at all. Correction markers are NOT stopwords: they are
// needed for the correction rule.
var dedupStopwords = map[string]struct{}{
	"yang": {}, "dan": {}, "di": {}, "ke": {}, "dari": {}, "untuk": {},
	"pada": {}, "ini": {}, "itu": {}, "dengan": {}, "ada": {}, "akan": {},
	"adalah": {}, "saya": {}, "kami": {}, "kita": {}, "mereka": {},
	"dia": {}, "bapak": {}, "ibu": {}, "saudara": {}, "teman": {},
	"temanteman": {}, "mohon": {}, "tolong": {}, "harap": {},
	"selamat": {}, "pagi": {}, "siang": {}, "sore": {}, "malam": {},
	"hari": {}, "tanggal": {}, "tgl": {}, "jam": {}, "pukul": {},
	"wib": {}, "via": {}, "berikut": {}, "terlampir": {}, "silakan": {},
	"silahkan": {}, "terima": {}, "kasih": {}, "assalamualaikum": {},
	"salam": {}, "halo": {}, "hai": {}, "hello": {}, "hi": {}, "yth": {},
	"kepada": {}, "dalam": {}, "oleh": {}, "atau": {}, "juga": {},
	"tidak": {}, "bukan": {}, "sudah": {}, "telah": {}, "sedang": {},
	"masih": {}, "bisa": {}, "dapat": {}, "agar": {}, "supaya": {},
	"jika": {}, "kalau": {}, "karena": {}, "sebagai": {}, "antara": {},
	"setelah": {}, "sebelum": {}, "saat": {}, "ketika": {}, "lalu": {},
	"kemudian": {}, "serta": {}, "maupun": {}, "bagi": {}, "atas": {},
	"tempat": {}, "perhatian": {}, "diberitahukan": {}, "disampaikan": {},
	"informasi": {}, "info": {},
	// Event-type words: they describe the message kind, not the topic.
	"undangan": {}, "pengumuman": {}, "diteruskan": {}, "forwarded": {},
	"zoom": {}, "meeting": {}, "rapat": {}, "acara": {}, "kegiatan": {},
	"agenda": {}, "diskusi": {}, "sosialisasi": {}, "webinar": {},
	"workshop": {}, "pelatihan": {}, "forum": {}, "link": {}, "dear": {},
	"bpk": {}, "sdr": {}, "wr": {}, "wb": {},
}

// normalizeLink lowercases a URL, trims trailing punctuation, and strips
// the query string and fragment, so the same link in different shapes
// still matches.
func normalizeLink(u string) string {
	u = strings.ToLower(u)
	u = strings.TrimRight(u, ".,;:!?)]")
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// signatureOf extracts the comparable fingerprint of a message. Links,
// dates, and times are blanked out of the text before tokenizing, so a
// time like 09:00 does not leak "09" and "00" into the token set.
func signatureOf(m Message, cfg DedupConfig) messageSig {
	sig := messageSig{
		tokens:    map[string]struct{}{},
		links:     map[string]struct{}{},
		zoomIDs:   map[string]struct{}{},
		dates:     map[string]struct{}{},
		times:     map[int]struct{}{},
		mediaKeys: map[string]struct{}{},
	}
	text := m.Content

	// Links (blanked from the token text).
	for _, loc := range linkRe.FindAllStringIndex(text, -1) {
		raw := text[loc[0]:loc[1]]
		norm := normalizeLink(raw)
		if norm != "" {
			sig.links[norm] = struct{}{}
			if mm := zoomIDRe.FindStringSubmatch(norm); mm != nil {
				sig.zoomIDs[mm[1]] = struct{}{}
			}
		}
	}
	blanked := blankSpans(text, linkRe.FindAllStringIndex(text, -1))

	// Times first, then dates: a time range like 09:00-12:00 contains
	// "00-12", which the numeric date pattern would otherwise read as
	// a date. Blanking the times first removes that.
	for _, loc := range timeRe.FindAllStringIndex(blanked, -1) {
		// Guard: "11.11" in a dotted date (11.11.2026) is not a time.
		if loc[1] < len(blanked) && blanked[loc[1]] == '.' &&
			loc[1]+1 < len(blanked) && blanked[loc[1]+1] >= '0' && blanked[loc[1]+1] <= '9' {
			continue
		}
		parts := timeRe.FindStringSubmatch(blanked[loc[0]:loc[1]])
		if len(parts) == 3 {
			if mins, ok := parseMinutes(parts[1], parts[2]); ok {
				sig.times[mins] = struct{}{}
			}
		}
	}
	for _, loc := range timeHourRe.FindAllStringIndex(blanked, -1) {
		parts := timeHourRe.FindStringSubmatch(blanked[loc[0]:loc[1]])
		if len(parts) == 3 {
			mins := "0"
			if parts[2] != "" {
				mins = parts[2]
			}
			if m, ok := parseMinutes(parts[1], mins); ok {
				sig.times[m] = struct{}{}
			}
		}
	}
	blanked = blankSpans(blanked, timeRe.FindAllStringIndex(blanked, -1))
	blanked = blankSpans(blanked, timeHourRe.FindAllStringIndex(blanked, -1))

	// Dates (blanked too).
	for _, loc := range dateNumRe.FindAllStringIndex(blanked, -1) {
		parts := dateNumRe.FindStringSubmatch(blanked[loc[0]:loc[1]])
		if len(parts) == 4 {
			sig.dates[fmt.Sprintf("%02s-%02s", parts[1], parts[2])] = struct{}{}
		}
	}
	for _, loc := range dateMonthRe.FindAllStringIndex(blanked, -1) {
		parts := dateMonthRe.FindStringSubmatch(blanked[loc[0]:loc[1]])
		if len(parts) == 3 {
			sig.dates[fmt.Sprintf("%02s-%02d", parts[1], monthNum[strings.ToLower(parts[2])])] = struct{}{}
		}
	}
	blanked = blankSpans(blanked, dateNumRe.FindAllStringIndex(blanked, -1))
	blanked = blankSpans(blanked, dateMonthRe.FindAllStringIndex(blanked, -1))

	// Media identity: the stored URL, or filename + length. Two messages
	// carrying the same file are the same evidence.
	if m.MediaType != "" {
		if u := normalizeLink(m.URL); u != "" {
			sig.mediaKeys[u] = struct{}{}
		} else if m.Filename != "" {
			sig.mediaKeys[fmt.Sprintf("%s|%d", m.Filename, m.FileLen)] = struct{}{}
		}
	}

	// Correction markers are whole words in the lowercased text.
	lower := strings.ToLower(m.Content)
	for _, marker := range cfg.CorrectionMarkers {
		if marker == "" {
			continue
		}
		if hasWord(lower, strings.ToLower(marker)) {
			sig.correction = true
			break
		}
	}

	// Tokens: lowercase, markup/punctuation to spaces, drop stopwords
	// and one-letter tokens.
	for _, tok := range tokenize(blanked) {
		sig.tokens[tok] = struct{}{}
	}
	return sig
}

// blankSpans replaces each [start, end) span of s with spaces (same
// length, so later matches keep their positions).
func blankSpans(s string, spans [][]int) string {
	b := []byte(s)
	for _, sp := range spans {
		for i := sp[0]; i < sp[1] && i < len(b); i++ {
			b[i] = ' '
		}
	}
	return string(b)
}

// parseMinutes validates an hour:minute pair and returns minutes since
// midnight. Out-of-range values (a version number, a year) are rejected.
func parseMinutes(hour, min string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(hour, "%d", &h); err != nil {
		return 0, false
	}
	if _, err := fmt.Sscanf(min, "%d", &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// hasWord reports whether word appears in s as a whole word.
func hasWord(s, word string) bool {
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i:i+len(word)] != word {
			continue
		}
		beforeOK := i == 0 || !isWordChar(rune(s[i-1]))
		afterOK := i+len(word) == len(s) || !isWordChar(rune(s[i+len(word)]))
		if beforeOK && afterOK {
			return true
		}
	}
	return false
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// tokenize splits s into normalized content words.
func tokenize(s string) []string {
	lower := strings.ToLower(s)
	var b strings.Builder
	for _, r := range lower {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	var out []string
	for _, tok := range strings.Fields(b.String()) {
		if len(tok) < 2 {
			continue
		}
		if _, stop := dedupStopwords[tok]; stop {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// jaccard is the token-set overlap: |A∩B| / |A∪B|. Two empty sets score
// 0 (no evidence), never 1.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for tok := range a {
		if _, ok := b[tok]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// sharedKeys reports whether two string sets share at least one key.
func sharedKeys(a, b map[string]struct{}) bool {
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

// minTimeGap is the smallest absolute gap (minutes) between any time in
// a and any time in b. ok is false when either set is empty.
func minTimeGap(a, b map[int]struct{}) (gap int, ok bool) {
	if len(a) == 0 || len(b) == 0 {
		return 0, false
	}
	first := true
	for x := range a {
		for y := range b {
			d := x - y
			if d < 0 {
				d = -d
			}
			if first || d < gap {
				gap, first = d, false
			}
		}
	}
	return gap, true
}

// formatTimes renders a time set as "09:00" or "09:00-12:00" (sorted).
func formatTimes(times map[int]struct{}) string {
	if len(times) == 0 {
		return ""
	}
	list := make([]int, 0, len(times))
	for t := range times {
		list = append(list, t)
	}
	sort.Ints(list)
	parts := make([]string, len(list))
	for i, t := range list {
		parts[i] = fmt.Sprintf("%02d:%02d", t/60, t%60)
	}
	return strings.Join(parts, "-")
}

// formatDates renders a date set as "11-11" or "11-11, 12-11" (sorted).
func formatDates(dates map[string]struct{}) string {
	if len(dates) == 0 {
		return ""
	}
	list := make([]string, 0, len(dates))
	for d := range dates {
		list = append(list, d)
	}
	sort.Strings(list)
	return strings.Join(list, ", ")
}

// pairVerdict is the outcome of comparing one candidate message against
// one already-grouped message.
type pairVerdict int

const (
	verdictNoMatch pairVerdict = iota
	verdictMerge
	verdictMergeCorrection
	verdictSimilar      // keep both, add a "mirip dengan" note
	verdictDateConflict // keep both, add a "tanggal berbeda" note; blocks a merge
	verdictTimeConflict // keep both, add a "jam berbeda" note; blocks a merge
)

// decidePair is the real comparison. The window check (on message
// timestamps) stays explicit at the call site.
func decidePair(member, candidate messageSig, cfg DedupConfig) (pairVerdict, [2]string, float64) {
	j := jaccard(member.tokens, candidate.tokens)
	sharedLink := sharedKeys(member.links, candidate.links)
	sharedZoom := sharedKeys(member.zoomIDs, candidate.zoomIDs)
	sharedMedia := sharedKeys(member.mediaKeys, candidate.mediaKeys)
	sharedDate := sharedKeys(member.dates, candidate.dates)
	sharedTime := false
	for t := range member.times {
		if _, ok := candidate.times[t]; ok {
			sharedTime = true
			break
		}
	}

	// 2. Correction first: a correction may change the date or time, so
	// it bypasses the conflict rules. It still needs real similarity.
	if candidate.correction &&
		(j >= cfg.CorrectionThreshold || sharedZoom || sharedLink || sharedMedia || sharedDate) {
		return verdictMergeCorrection, [2]string{}, j
	}

	// 3. Date conflict: both carry explicit (different) dates.
	if len(member.dates) > 0 && len(candidate.dates) > 0 && !sharedDate {
		if j >= cfg.NoteThreshold {
			return verdictDateConflict,
				[2]string{formatDates(member.dates), formatDates(candidate.dates)}, j
		}
		return verdictNoMatch, [2]string{}, j
	}

	// 4. Time conflict: both carry explicit event times that differ by
	// more than 15 minutes.
	if gap, ok := minTimeGap(member.times, candidate.times); ok && gap > 15 && !sharedTime {
		if j >= cfg.NoteThreshold || sharedDate || sharedLink {
			return verdictTimeConflict,
				[2]string{formatTimes(member.times), formatTimes(candidate.times)}, j
		}
		return verdictNoMatch, [2]string{}, j
	}

	// 5. Shared Zoom meeting ID: one meeting.
	if sharedZoom {
		return verdictMerge, [2]string{}, j
	}
	// 6. Shared media or shared link, with supporting overlap.
	if sharedMedia && (j >= cfg.NoteThreshold || sharedDate || sharedTime) {
		return verdictMerge, [2]string{}, j
	}
	if sharedLink && j >= cfg.NoteThreshold {
		return verdictMerge, [2]string{}, j
	}
	// 7. Token overlap at note level, corroborated by a shared explicit
	// date or time: merge (the entity agreement backs the topic
	// overlap). A reworded copy often drops words, so its overlap
	// alone can sit below the merge threshold.
	if j >= cfg.NoteThreshold && (sharedDate || sharedTime) {
		return verdictMerge, [2]string{}, j
	}
	// 8. Token overlap at or above the merge threshold.
	if j >= cfg.Threshold {
		return verdictMerge, [2]string{}, j
	}
	// 9. Similar enough to note, not to merge.
	if j >= cfg.NoteThreshold {
		return verdictSimilar, [2]string{}, j
	}
	return verdictNoMatch, [2]string{}, j
}

// messageGroup is a set of messages that export as one row.
type messageGroup struct {
	members    []Message    // oldest first
	sigs       []messageSig // parallel to members
	canonical  int          // index of the member whose text becomes the row
	correction bool         // a correction message joined this group
	// notes maps another group's first message ID to the note text
	// ("mirip dengan <id>" / "mirip dengan <id> (jam berbeda: ...)").
	notes map[string]string
}

// firstID is the group's stable reference: the oldest member's ID.
func (g *messageGroup) firstID() string {
	if len(g.members) == 0 {
		return ""
	}
	return g.members[0].ID
}

// pickCanonical chooses the member whose text becomes the row: the
// latest correction message when the group holds a correction (it
// supersedes), otherwise the member with the most content words
// (longest/most complete); ties go to the earliest.
func (g *messageGroup) pickCanonical() {
	if g.correction {
		for i := len(g.members) - 1; i >= 0; i-- {
			if g.sigs[i].correction {
				g.canonical = i
				return
			}
		}
	}
	best, bestN := 0, -1
	for i := range g.members {
		if n := len(g.sigs[i].tokens); n > bestN {
			best, bestN = i, n
		}
	}
	g.canonical = best
}

// groupMessages clusters new messages (oldest first) into groups. With
// dedup disabled it returns one singleton group per message.
func groupMessages(msgs []Message, cfg DedupConfig) []messageGroup {
	if len(msgs) == 0 {
		return nil
	}
	if !cfg.Enabled {
		out := make([]messageGroup, 0, len(msgs))
		for _, m := range msgs {
			g := messageGroup{members: []Message{m}, sigs: []messageSig{signatureOf(m, cfg)}, notes: map[string]string{}}
			g.pickCanonical()
			out = append(out, g)
		}
		return out
	}

	var groups []messageGroup
	for _, m := range msgs {
		sig := signatureOf(m, cfg)
		// Compare against every existing group, member by member.
		bestGroup, bestJ, bestCorrection := -1, -1.0, false
		type noteCand struct {
			gi      int
			verdict pairVerdict
			detail  [2]string
			j       float64
		}
		var notes []noteCand
		for gi := range groups {
			g := &groups[gi]
			join, joinCorr, groupJ := false, false, -1.0
			blocked := false
			var groupNote *noteCand
			for mi := range g.members {
				// 1. Message timestamps too far apart: never merge.
				gap := m.Timestamp.Sub(g.members[mi].Timestamp)
				if gap < 0 {
					gap = -gap
				}
				if gap > time.Duration(cfg.TimeWindowHours)*time.Hour {
					continue
				}
				v, detail, j := decidePair(g.sigs[mi], sig, cfg)
				switch v {
				case verdictMerge:
					join = true
					if j > groupJ {
						groupJ = j
					}
				case verdictMergeCorrection:
					join, joinCorr = true, true
					if j > groupJ {
						groupJ = j
					}
				case verdictDateConflict, verdictTimeConflict:
					// A conflict from any member blocks joining the
					// whole group (conservative); the note still
					// stands. A conflict note outranks a plain
					// similarity note from another member.
					blocked = true
					if groupNote == nil || v > groupNote.verdict ||
						(v == groupNote.verdict && j > groupNote.j) {
						groupNote = &noteCand{gi: gi, verdict: v, detail: detail, j: j}
					}
				case verdictSimilar:
					if groupNote == nil ||
						(groupNote.verdict == verdictSimilar && j > groupNote.j) {
						groupNote = &noteCand{gi: gi, verdict: v, detail: detail, j: j}
					}
				}
			}
			if blocked {
				join = false
			}
			if join && groupJ > bestJ {
				bestGroup, bestJ, bestCorrection = gi, groupJ, joinCorr
			}
			if groupNote != nil {
				notes = append(notes, *groupNote)
			}
		}

		if bestGroup >= 0 {
			g := &groups[bestGroup]
			g.members = append(g.members, m)
			g.sigs = append(g.sigs, sig)
			if bestCorrection {
				g.correction = true
			}
			g.pickCanonical()
			// Notes recorded against other groups still apply to the
			// merged group; they reference the other groups' first
			// IDs, which are stable. (A note against the joined group
			// itself is skipped: the message joined it.)
			for _, nc := range notes {
				if nc.gi == bestGroup {
					continue
				}
				addMutualNote(&groups[bestGroup], &groups[nc.gi], nc.verdict, nc.detail)
			}
			continue
		}

		// No group to join: start a new one, and record mutual notes
		// with every group it was similar to (but did not merge with).
		ng := messageGroup{members: []Message{m}, sigs: []messageSig{sig}, notes: map[string]string{}}
		ng.pickCanonical()
		groups = append(groups, ng)
		ni := len(groups) - 1
		for _, nc := range notes {
			addMutualNote(&groups[ni], &groups[nc.gi], nc.verdict, nc.detail)
		}
	}
	return groups
}

// addMutualNote records a similarity note on both groups, each
// referencing the other's first message ID. detail is always
// [member/other-group values, candidate/this-group values]; a is the
// group holding the candidate message, b the group holding the member
// it was compared against. Each note shows its own group's values
// first ("jam berbeda: 09:00-12:00 vs 13:00" on the 09:00 row).
func addMutualNote(a, b *messageGroup, v pairVerdict, detail [2]string) {
	if a.notes == nil {
		a.notes = map[string]string{}
	}
	if b.notes == nil {
		b.notes = map[string]string{}
	}
	switch v {
	case verdictTimeConflict:
		a.notes[b.firstID()] = fmt.Sprintf("mirip dengan %s (jam berbeda: %s vs %s)", b.firstID(), detail[1], detail[0])
		b.notes[a.firstID()] = fmt.Sprintf("mirip dengan %s (jam berbeda: %s vs %s)", a.firstID(), detail[0], detail[1])
	case verdictDateConflict:
		a.notes[b.firstID()] = fmt.Sprintf("mirip dengan %s (tanggal berbeda: %s vs %s)", b.firstID(), detail[1], detail[0])
		b.notes[a.firstID()] = fmt.Sprintf("mirip dengan %s (tanggal berbeda: %s vs %s)", a.firstID(), detail[0], detail[1])
	case verdictSimilar:
		a.notes[b.firstID()] = fmt.Sprintf("mirip dengan %s", b.firstID())
		b.notes[a.firstID()] = fmt.Sprintf("mirip dengan %s", a.firstID())
	}
}

// sortedNotes returns a group's notes in stable (ID-sorted) order.
func (g *messageGroup) sortedNotes() []string {
	if len(g.notes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(g.notes))
	for id := range g.notes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, g.notes[id])
	}
	return out
}
