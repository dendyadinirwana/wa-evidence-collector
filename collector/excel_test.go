package collector

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

var wantRecapHeader = []string{
	"Tanggal", "Jam Pesan (WIB)", "Grup", "Pengirim", "Jenis Kegiatan",
	"Isi Pesan", "Jenis Evidence", "Nama File", "Link Drive",
	"Status Unduhan", "Konteks",
}

// Fictional fixture rows (555-range senders, all-zero group IDs). Not real.
func recapFixtureRows() []EvidenceRow {
	return []EvidenceRow{
		{
			MessageID: "m1", ChatJID: groupA, ChatName: "Group A",
			Sender: "sender:aaaaaaaa", Timestamp: "2026-11-10T20:30:00Z",
			Content: "rapat besok jam 09.00", MediaStatus: "not-applicable",
		},
		{
			MessageID: "m2", ChatJID: groupA, ChatName: "Group A",
			Sender: "sender:bbbbbbbb", Timestamp: "2026-11-11T02:00:00Z",
			Content: "foto undangan", MediaType: "image",
			MediaFile:   "g-aaaaaaaa_image_20261111_090000_m2.jpg",
			MediaStatus: "ok",
		},
		{
			MessageID: "m3", ChatJID: dmA, ChatName: "DM A",
			Sender: "sender:cccccccc", Timestamp: "2026-11-11T05:15:00Z",
			Content:     "lihat https://example.invalid/info",
			MediaStatus: "not-applicable",
		},
		{
			MessageID: "m4", ChatJID: groupA, ChatName: "Group A",
			Sender: "sender:dddddddd", Timestamp: "2026-11-11T06:00:00Z",
			Content: "laporan pdf", MediaType: "document",
			MediaFile: "", MediaStatus: "failed",
		},
	}
}

func TestRecapHeaderOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.xlsx")
	if err := WriteRecapXLSX(path, nil, "test"); err != nil {
		t.Fatal(err)
	}
	header, rows, err := ReadRecapXLSX(path)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(header) != fmt.Sprint(wantRecapHeader) {
		t.Fatalf("header = %v, want %v", header, wantRecapHeader)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

func TestRowToRecapMapping(t *testing.T) {
	rows := recapFixtureRows()
	got := make([][]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, RowToRecap(r, "morning"))
	}
	if len(got) != 4 {
		t.Fatalf("mapped %d rows", len(got))
	}
	// m1: 2026-11-10T20:30:00Z is 2026-11-11 03:30 WIB (date rolls over).
	if got[0][0] != "2026-11-11" || got[0][1] != "03:30" {
		t.Fatalf("m1 date/time = %q %q, want 2026-11-11 03:30", got[0][0], got[0][1])
	}
	if got[0][2] != ChatShort(groupA) || got[0][3] != "sender:aaaaaaaa" {
		t.Fatalf("m1 grup/pengirim = %q %q", got[0][2], got[0][3])
	}
	if got[0][4] != "Pesan teks" || got[0][6] != "Pesan teks" {
		t.Fatalf("m1 jenis = %q %q", got[0][4], got[0][6])
	}
	if got[0][8] != "" || got[0][9] != "not-applicable" || got[0][10] != "morning" {
		t.Fatalf("m1 link/status/konteks = %q %q %q", got[0][8], got[0][9], got[0][10])
	}
	// m2: image downloaded ok.
	if got[1][0] != "2026-11-11" || got[1][1] != "09:00" {
		t.Fatalf("m2 date/time = %q %q, want 2026-11-11 09:00", got[1][0], got[1][1])
	}
	if got[1][4] != "Media gambar" || got[1][6] != "Gambar" {
		t.Fatalf("m2 jenis = %q %q", got[1][4], got[1][6])
	}
	if got[1][7] != "g-aaaaaaaa_image_20261111_090000_m2.jpg" || got[1][9] != "ok" {
		t.Fatalf("m2 file/status = %q %q", got[1][7], got[1][9])
	}
	// m3: link in text, no media.
	if got[2][2] != ChatShort(dmA) {
		t.Fatalf("m3 grup = %q, want %q", got[2][2], ChatShort(dmA))
	}
	if got[2][4] != "Pesan teks" || got[2][6] != "Link" {
		t.Fatalf("m3 jenis = %q %q", got[2][4], got[2][6])
	}
	// m4: document download failed.
	if got[3][4] != "Dokumen" || got[3][6] != "Pesan teks" || got[3][9] != "failed" {
		t.Fatalf("m4 jenis/status = %q %q %q", got[3][4], got[3][6], got[3][9])
	}
	// Every row has exactly 11 columns.
	for i, r := range got {
		if len(r) != 11 {
			t.Fatalf("row %d has %d columns", i, len(r))
		}
	}
}

func TestRecapFormatting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fmt.xlsx")
	if err := WriteRecapXLSX(path, recapFixtureRows(), "test"); err != nil {
		t.Fatal(err)
	}
	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Header: dark-blue fill, white bold text.
	styleID, err := f.GetCellStyle(recapSheetName, "A1")
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.GetStyle(styleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Fill.Color) == 0 || st.Fill.Color[0] != recapHeaderFill {
		t.Fatalf("header fill = %v, want %s", st.Fill.Color, recapHeaderFill)
	}
	if st.Font == nil || !st.Font.Bold || st.Font.Color != recapHeaderFontColor {
		t.Fatalf("header font = %+v", st.Font)
	}
	if st.Alignment == nil || !st.Alignment.WrapText {
		t.Fatalf("header alignment = %+v", st.Alignment)
	}
	// Data cells: wrapped text.
	dataStyleID, err := f.GetCellStyle(recapSheetName, "F2")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := f.GetStyle(dataStyleID)
	if err != nil {
		t.Fatal(err)
	}
	if dst.Alignment == nil || !dst.Alignment.WrapText {
		t.Fatalf("data alignment = %+v", dst.Alignment)
	}
	// Frozen header row.
	panes, err := f.GetPanes(recapSheetName)
	if err != nil {
		t.Fatal(err)
	}
	if !panes.Freeze || panes.YSplit != 1 {
		t.Fatalf("panes = %+v, want frozen row 1", panes)
	}
	// Landscape, fit to one page wide.
	layout, err := f.GetPageLayout(recapSheetName)
	if err != nil {
		t.Fatal(err)
	}
	if layout.Orientation == nil || *layout.Orientation != "landscape" {
		t.Fatalf("orientation = %+v", layout.Orientation)
	}
	if layout.FitToWidth == nil || *layout.FitToWidth != 1 {
		t.Fatalf("fitToWidth = %+v", layout.FitToWidth)
	}
	props, err := f.GetSheetProps(recapSheetName)
	if err != nil {
		t.Fatal(err)
	}
	if props.FitToPage == nil || !*props.FitToPage {
		t.Fatalf("fitToPage = %+v", props.FitToPage)
	}
	// Column widths are set (spot-check the wide content column).
	w, err := f.GetColWidth(recapSheetName, "F")
	if err != nil {
		t.Fatal(err)
	}
	if w != 64 {
		t.Fatalf("col F width = %v, want 64", w)
	}

	// Autofilter + no formulas: inspect the sheet XML directly.
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var sheetXML string
	for _, zf := range r.File {
		if zf.Name == "xl/worksheets/sheet1.xml" {
			rc, err := zf.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
			sheetXML = string(data)
		}
	}
	if !strings.Contains(sheetXML, `autoFilter ref="$A$1:$K$5"`) {
		t.Fatalf("sheet XML has no $A$1:$K$5 autofilter:\n%s", sheetXML)
	}
	if strings.Contains(sheetXML, "<f>") {
		t.Fatal("sheet XML contains a formula cell")
	}
}

func TestMonthlyMerge(t *testing.T) {
	outDir := t.TempDir()
	nov10 := filepath.Join(outDir, "20261110")
	nov11 := filepath.Join(outDir, "20261111")
	oct31 := filepath.Join(outDir, "20261031")
	for _, d := range []string{nov10, nov11, oct31} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	rows10 := []EvidenceRow{recapFixtureRows()[0]}
	rows11 := recapFixtureRows()[1:]
	rowsOct := []EvidenceRow{recapFixtureRows()[0]}
	if err := WriteRecapXLSX(filepath.Join(nov10, "Rekap-Evidence-20261110.xlsx"), rows10, "day10"); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecapXLSX(filepath.Join(nov11, "Rekap-Evidence-20261111.xlsx"), rows11, "day11"); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecapXLSX(filepath.Join(oct31, "Rekap-Evidence-20261031.xlsx"), rowsOct, "dayOct"); err != nil {
		t.Fatal(err)
	}

	merged, err := MergeMonthlyRecap(outDir, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if merged != filepath.Join(outDir, "Rekap-Evidence-202611-gabungan.xlsx") {
		t.Fatalf("merged path = %q", merged)
	}
	header, rows, err := ReadRecapXLSX(merged)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(header) != fmt.Sprint(wantRecapHeader) {
		t.Fatalf("merged header = %v", header)
	}
	// 1 row from the 10th + 3 rows from the 11th, in date order; the
	// October file must not leak in.
	if len(rows) != 4 {
		t.Fatalf("merged rows = %d, want 4", len(rows))
	}
	if rows[0][10] != "day10" || rows[1][10] != "day11" || rows[3][10] != "day11" {
		t.Fatalf("merged konteks order wrong: %v", rows)
	}
	if rows[0][5] != "rapat besok jam 09.00" || rows[1][5] != "foto undangan" {
		t.Fatalf("merged content wrong: %v", rows)
	}
}

func TestMonthlyMergeEmpty(t *testing.T) {
	outDir := t.TempDir()
	merged, err := MergeMonthlyRecap(outDir, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := ReadRecapXLSX(merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("empty monthly has %d rows", len(rows))
	}
}

func TestReadRecapRejectsNonRecap(t *testing.T) {
	// A plain workbook without the recap header must be rejected.
	f := excelize.NewFile()
	path := filepath.Join(t.TempDir(), "plain.xlsx")
	if err := f.SetCellStr("Sheet1", "A1", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := f.SaveAs(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRecapXLSX(path); err == nil {
		t.Fatal("non-recap file accepted")
	}
}
