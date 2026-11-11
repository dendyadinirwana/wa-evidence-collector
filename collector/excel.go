package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// Excel recap export (Rekap-Evidence-<YYYYMMDD>.xlsx + monthly combined
// file). This mirrors the upstream workflow's 11-column Excel recap.
//
// Mapping rules (no fields are invented; every column derives from the
// EvidenceRow the JSON/CSV exporters already produce):
//   - Tanggal / Jam Pesan (WIB): the row timestamp converted to WIB
//     (UTC+7, fixed zone, no DST).
//   - Grup: the hashed chat label (ChatShort), never the real group name
//     or JID.
//   - Pengirim: the row's sender, already redacted when redact=true.
//   - Jenis Kegiatan / Jenis Evidence: derived from MediaType,
//     MediaStatus, and whether the text contains a link (see
//     activityKind / evidenceKind). This is a coarse media-based label,
//     not a content classification.
//   - Link Drive: the row's LinkDrive (remote URL of its evidence file
//     after a successful upload); empty while exports stay local.
//   - Konteks: the run label (schedule label / --once label), plus a
//     merged row's note ("digabung dari 2 sumber, 2 grup",
//     "mirip dengan <id> (jam berbeda: ...)").
//
// All cells are plain strings (SetCellStr): a value starting with "="
// can never become a formula. excelize truncates cells to 32767
// characters; very long messages are cut there.

// recapSheetName is the single sheet in every recap file.
const recapSheetName = "Rekap Evidence"

// recapHeader is the exact 11-column order, shared by daily and monthly files.
var recapHeader = []string{
	"Tanggal", "Jam Pesan (WIB)", "Grup", "Pengirim", "Jenis Kegiatan",
	"Isi Pesan", "Jenis Evidence", "Nama File", "Link Drive",
	"Status Unduhan", "Konteks",
}

// recapColWidths is the width (characters) per column, same order as recapHeader.
var recapColWidths = []float64{12, 11, 12, 16, 16, 64, 16, 32, 32, 15, 16}

const (
	recapHeaderFill      = "1F3864" // dark blue
	recapHeaderFontColor = "FFFFFF" // white
	recapBorderColor     = "808080" // thin grey borders
)

// wib is WIB as a fixed zone (UTC+7, no DST), so date/time conversion
// never depends on the host's tzdata.
var wib = time.FixedZone("WIB", 7*3600)

// DailyFileName is Rekap-Evidence-<YYYYMMDD>.xlsx for a run date.
func DailyFileName(t time.Time) string {
	return "Rekap-Evidence-" + t.Format("20060102") + ".xlsx"
}

// MonthlyFileName is Rekap-Evidence-<YYYYMM>-gabungan.xlsx for a month.
func MonthlyFileName(t time.Time) string {
	return "Rekap-Evidence-" + t.Format("200601") + "-gabungan.xlsx"
}

// RowToRecap maps one EvidenceRow to the 11 recap columns.
func RowToRecap(r EvidenceRow, label string) []string {
	date, clock := "", ""
	if ts, err := time.Parse(time.RFC3339, r.Timestamp); err == nil {
		local := ts.In(wib)
		date = local.Format("2006-01-02")
		clock = local.Format("15:04")
	}
	// A chat render is the row's evidence: its PNG name goes in Nama
	// File and its status in Status Unduhan.
	namaFile, status := r.MediaFile, r.MediaStatus
	if r.ChatRender != "" {
		namaFile, status = r.ChatRender, "dibuat dari teks pesan"
	}
	// A merged row lists every source: all distinct chat labels in
	// Grup, all distinct senders in Pengirim. A plain row keeps the
	// single label/sender as before.
	grup, pengirim := ChatShort(r.ChatJID), r.Sender
	if len(r.Sources) > 0 {
		grup = joinDistinctChatShort(r.Sources)
		pengirim = joinDistinctSenders(r.Sources)
	}
	// Konteks is the run label, plus the row's merge/similarity note
	// ("digabung dari 2 sumber, 2 grup", "mirip dengan <id> ...").
	konteks := label
	if r.Note != "" {
		konteks = label + "; " + r.Note
	}
	return []string{
		date,
		clock,
		grup,
		pengirim,
		activityKind(r),
		r.Content,
		evidenceKind(r),
		namaFile,
		r.LinkDrive,
		status,
		konteks,
	}
}

// joinDistinctChatShort joins the distinct hashed chat labels of the
// sources, in source order.
func joinDistinctChatShort(sources []RowSource) string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range sources {
		l := ChatShort(s.ChatJID)
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return strings.Join(out, ", ")
}

// joinDistinctSenders joins the distinct (redacted) senders of the
// sources, in source order.
func joinDistinctSenders(sources []RowSource) string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range sources {
		if _, ok := seen[s.Sender]; ok {
			continue
		}
		seen[s.Sender] = struct{}{}
		out = append(out, s.Sender)
	}
	return strings.Join(out, ", ")
}

// activityKind is a coarse media-based label for the Jenis Kegiatan column.
func activityKind(r EvidenceRow) string {
	switch r.MediaType {
	case "image":
		return "Media gambar"
	case "video":
		return "Media video"
	case "document":
		return "Dokumen"
	case "audio":
		return "Media audio"
	case "":
		return "Pesan teks"
	default:
		return "Media"
	}
}

// evidenceKind is the Jenis Evidence label: "Screenshot chat (render)"
// when a chat render was generated, the media kind when a file was
// downloaded, "Link" when the text carries a URL, otherwise "Pesan teks".
func evidenceKind(r EvidenceRow) string {
	// A chat render is a RENDER drawn from the message text (never a
	// screenshot); it wins over a plain text row.
	if r.ChatRender != "" {
		return "Screenshot chat (render)"
	}
	if r.MediaStatus == "ok" && r.MediaType != "" {
		switch r.MediaType {
		case "image":
			return "Gambar"
		case "video":
			return "Video"
		case "document":
			return "Dokumen"
		case "audio":
			return "Audio"
		default:
			return "Media"
		}
	}
	if strings.Contains(r.Content, "http://") || strings.Contains(r.Content, "https://") {
		return "Link"
	}
	return "Pesan teks"
}

func thinBorder() []excelize.Border {
	sides := []string{"left", "right", "top", "bottom"}
	out := make([]excelize.Border, 0, len(sides))
	for _, s := range sides {
		out = append(out, excelize.Border{Type: s, Color: recapBorderColor, Style: 1})
	}
	return out
}

// recapStyles returns the (header, data) cell styles shared by the
// recap sheet and the Agenda sheet: dark-blue header + white bold
// text, wrapped data cells, thin grey borders.
func recapStyles(f *excelize.File) (int, int, error) {
	headerStyle, err := f.NewStyle(&excelize.Style{
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{recapHeaderFill}},
		Font:      &excelize.Font{Bold: true, Color: recapHeaderFontColor, Family: "Calibri", Size: 11},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border:    thinBorder(),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("header style: %w", err)
	}
	dataStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Family: "Calibri", Size: 11},
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: true},
		Border:    thinBorder(),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("data style: %w", err)
	}
	return headerStyle, dataStyle, nil
}

// styleSheet writes header + data rows (plain strings) to an existing
// sheet with the recap formatting: frozen header, autofilter,
// landscape fit-to-width. It is shared by the recap sheet (via
// writeRecapFile) and the Agenda sheet.
func styleSheet(f *excelize.File, sheet string, header []string, widths []float64, dataRows [][]string) error {
	headerStyle, dataStyle, err := recapStyles(f)
	if err != nil {
		return err
	}
	lastCol := string(rune('A' + len(header) - 1))
	lastRow := len(dataRows) + 1

	for i, h := range header {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err := f.SetCellStr(sheet, cell, h); err != nil {
			return fmt.Errorf("write header %q: %w", h, err)
		}
	}
	if err := f.SetCellStyle(sheet, "A1", lastCol+"1", headerStyle); err != nil {
		return fmt.Errorf("style header: %w", err)
	}
	for ri, row := range dataRows {
		for ci, v := range row {
			if ci >= len(header) {
				break
			}
			cell, _ := excelize.CoordinatesToCellName(ci+1, ri+2)
			if err := f.SetCellStr(sheet, cell, v); err != nil {
				return fmt.Errorf("write cell %s: %w", cell, err)
			}
		}
	}
	if len(dataRows) > 0 {
		if err := f.SetCellStyle(sheet, "A2", lastCol+strconv.Itoa(lastRow), dataStyle); err != nil {
			return fmt.Errorf("style data: %w", err)
		}
	}
	for i, w := range widths {
		col := string(rune('A' + i))
		if err := f.SetColWidth(sheet, col, col, w); err != nil {
			return fmt.Errorf("column width %s: %w", col, err)
		}
	}
	if err := f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
		Selection: []excelize.Selection{{SQRef: "A2", ActiveCell: "A2", Pane: "bottomLeft"}},
	}); err != nil {
		return fmt.Errorf("freeze panes: %w", err)
	}
	if err := f.AutoFilter(sheet, "A1:"+lastCol+strconv.Itoa(lastRow), nil); err != nil {
		return fmt.Errorf("autofilter: %w", err)
	}
	orientation := "landscape"
	fitW, fitH := 1, 0
	if err := f.SetPageLayout(sheet, &excelize.PageLayoutOptions{
		Orientation: &orientation, FitToWidth: &fitW, FitToHeight: &fitH,
	}); err != nil {
		return fmt.Errorf("page layout: %w", err)
	}
	fit := true
	if err := f.SetSheetProps(sheet, &excelize.SheetPropsOptions{FitToPage: &fit}); err != nil {
		return fmt.Errorf("fit to page: %w", err)
	}
	return nil
}

// writeRecapFile writes header + data rows (already mapped to the 11
// columns) to path with the recap formatting, then reopens the file to
// prove it opens error-free with the expected header and row count.
func writeRecapFile(path string, dataRows [][]string) error {
	f := excelize.NewFile()
	if err := f.SetSheetName("Sheet1", recapSheetName); err != nil {
		return fmt.Errorf("rename sheet: %w", err)
	}
	if err := styleSheet(f, recapSheetName, recapHeader, recapColWidths, dataRows); err != nil {
		return err
	}
	if err := f.SaveAs(path); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}

	// Verify: the file must reopen with the expected header and row count.
	return verifyRecapFile(path, len(dataRows))
}

// verifyRecapFile reopens path and checks its header and data-row count.
func verifyRecapFile(path string, wantDataRows int) error {
	header, rows, err := ReadRecapXLSX(path)
	if err != nil {
		return fmt.Errorf("verify %s: %w", path, err)
	}
	if !slices.Equal(header, recapHeader) {
		return fmt.Errorf("verify %s: header mismatch: %v", path, header)
	}
	if len(rows) != wantDataRows {
		return fmt.Errorf("verify %s: %d data rows, want %d", path, len(rows), wantDataRows)
	}
	return nil
}

// WriteRecapXLSX writes one daily recap file for rows (same rows as the
// JSON/CSV export of the run). label becomes the Konteks column.
func WriteRecapXLSX(path string, rows []EvidenceRow, label string) error {
	data := make([][]string, 0, len(rows))
	for _, r := range rows {
		data = append(data, RowToRecap(r, label))
	}
	return writeRecapFile(path, data)
}

// ReadRecapXLSX reads a recap file back: header + data rows (each padded
// to 11 columns). It rejects files whose header is not the recap header.
func ReadRecapXLSX(path string) ([]string, [][]string, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	got, err := f.GetRows(recapSheetName)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(got) == 0 {
		return nil, nil, fmt.Errorf("read %s: empty sheet", path)
	}
	header := got[0]
	if !slices.Equal(header, recapHeader) {
		return nil, nil, fmt.Errorf("read %s: not a recap file (header %v)", path, header)
	}
	rows := make([][]string, 0, len(got)-1)
	for _, r := range got[1:] {
		padded := make([]string, len(recapHeader))
		copy(padded, r)
		rows = append(rows, padded)
	}
	return header, rows, nil
}

// MergeMonthlyRecap rebuilds the monthly combined file for month from all
// daily recap files found under outputDir/<YYYYMMDD>/ for that month
// (header once, data rows in date order). The result is written to
// outputDir/Rekap-Evidence-<YYYYMM>-gabungan.xlsx. Rebuilding is
// idempotent: the file is regenerated from the daily files on every run.
//
// Known limitation: daily files hold only the rows that were new in their
// run, so re-running the same window overwrites that day's file with the
// (possibly empty) re-run rows, and the next monthly rebuild reflects
// the latest daily files. Daily re-runs are the exception, not the norm
// (each window is scheduled once).
func MergeMonthlyRecap(outputDir string, month time.Time) (string, error) {
	prefix := month.Format("200601")
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return "", fmt.Errorf("read output dir %s: %w", outputDir, err)
	}
	var dailyPaths []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) != 8 || !strings.HasPrefix(name, prefix) || !isAllDigits(name) {
			continue
		}
		p := filepath.Join(outputDir, name, "Rekap-Evidence-"+name+".xlsx")
		if _, err := os.Stat(p); err == nil {
			dailyPaths = append(dailyPaths, p)
		}
	}
	sort.Strings(dailyPaths)

	var all [][]string
	for _, p := range dailyPaths {
		_, rows, err := ReadRecapXLSX(p)
		if err != nil {
			return "", fmt.Errorf("read daily %s: %w", p, err)
		}
		all = append(all, rows...)
	}
	out := filepath.Join(outputDir, MonthlyFileName(month))
	if err := writeRecapFile(out, all); err != nil {
		return "", err
	}
	return out, nil
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// Agenda sheet (second sheet of the daily recap file). One row per
// agenda candidate, same formatting as the recap sheet. No calendar
// events are created: these are candidates for the operator to
// select from. The monthly combined file does NOT carry an Agenda
// sheet (it rebuilds from the daily recap sheets only).

// agendaSheetName is the sheet holding the run's agenda candidates.
const agendaSheetName = "Agenda"

// agendaHeader is the exact 11-column order of the Agenda sheet.
var agendaHeader = []string{
	"Judul", "Tanggal", "Jam Mulai", "Jam Selesai", "Durasi (menit)",
	"Lokasi", "Link", "Status", "Kurang", "Sumber", "Catatan",
}

// agendaColWidths is the width (characters) per column, same order as agendaHeader.
var agendaColWidths = []float64{48, 12, 10, 11, 13, 28, 40, 13, 14, 32, 40}

// CandidateToAgendaRow maps one AgendaCandidate to the 11 Agenda columns.
func CandidateToAgendaRow(c AgendaCandidate) []string {
	end, dur := "", ""
	if c.End != nil {
		end = *c.End
	}
	if c.DurationMin != nil {
		dur = strconv.Itoa(*c.DurationMin)
	}
	return []string{
		c.Title,
		c.Date,
		c.Start,
		end,
		dur,
		c.Location,
		c.Link,
		c.Status,
		strings.Join(c.Missing, ", "),
		agendaSources(c.Sources),
		c.Note,
	}
}

// agendaSources renders the candidate's sources as
// "g-<hash> (m10, m11), dm-<hash> (m12)": hashed chat labels with
// their message IDs, in source order.
func agendaSources(sources []AgendaSource) string {
	byChat := map[string][]string{}
	var order []string
	for _, s := range sources {
		l := ChatShort(s.ChatJID)
		if _, ok := byChat[l]; !ok {
			order = append(order, l)
		}
		byChat[l] = append(byChat[l], s.MessageID)
	}
	parts := make([]string, 0, len(order))
	for _, l := range order {
		parts = append(parts, l+" ("+strings.Join(byChat[l], ", ")+")")
	}
	return strings.Join(parts, ", ")
}

// AddAgendaSheet adds an "Agenda" sheet with the candidates to the
// daily recap file at path (replacing a sheet of that name when the
// file already has one, so a re-run overwrites cleanly). The recap
// sheet is left untouched and stays the active sheet. The file is
// reopened afterwards to prove the sheet is there with the expected
// header and row count.
func AddAgendaSheet(path string, candidates []AgendaCandidate) error {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if idx, err := f.GetSheetIndex(agendaSheetName); err == nil && idx >= 0 {
		if err := f.DeleteSheet(agendaSheetName); err != nil {
			return fmt.Errorf("replace agenda sheet: %w", err)
		}
	}
	if _, err := f.NewSheet(agendaSheetName); err != nil {
		return fmt.Errorf("new agenda sheet: %w", err)
	}
	rows := make([][]string, 0, len(candidates))
	for _, c := range candidates {
		rows = append(rows, CandidateToAgendaRow(c))
	}
	if err := styleSheet(f, agendaSheetName, agendaHeader, agendaColWidths, rows); err != nil {
		return err
	}
	// Keep the recap sheet active (it is the first sheet).
	if idx, err := f.GetSheetIndex(recapSheetName); err == nil {
		f.SetActiveSheet(idx)
	}
	if err := f.Save(); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	return verifyAgendaSheet(path, len(candidates))
}

// ReadAgendaSheet reads an Agenda sheet back: header + data rows
// (each padded to 11 columns). It rejects files without an Agenda
// sheet or with a different header.
func ReadAgendaSheet(path string) ([]string, [][]string, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	got, err := f.GetRows(agendaSheetName)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(got) == 0 {
		return nil, nil, fmt.Errorf("read %s: empty agenda sheet", path)
	}
	header := got[0]
	if !slices.Equal(header, agendaHeader) {
		return nil, nil, fmt.Errorf("read %s: not an agenda sheet (header %v)", path, header)
	}
	rows := make([][]string, 0, len(got)-1)
	for _, r := range got[1:] {
		padded := make([]string, len(agendaHeader))
		copy(padded, r)
		rows = append(rows, padded)
	}
	return header, rows, nil
}

// verifyAgendaSheet reopens path and checks the Agenda sheet's header
// and data-row count.
func verifyAgendaSheet(path string, wantDataRows int) error {
	header, rows, err := ReadAgendaSheet(path)
	if err != nil {
		return fmt.Errorf("verify agenda %s: %w", path, err)
	}
	if !slices.Equal(header, agendaHeader) {
		return fmt.Errorf("verify agenda %s: header mismatch: %v", path, header)
	}
	if len(rows) != wantDataRows {
		return fmt.Errorf("verify agenda %s: %d data rows, want %d", path, len(rows), wantDataRows)
	}
	return nil
}
