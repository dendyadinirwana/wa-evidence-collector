package collector

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Summary is the result of one collection run.
type Summary struct {
	Label          string
	WindowStart    time.Time
	WindowEnd      time.Time
	RunDir         string
	Scanned        int // messages found in the window
	New            int // messages exported (not seen before)
	SkippedSeen    int // messages skipped because already collected
	MediaOK        int
	MediaFailed    int
	MediaSkipped   int // media disabled in config, or no bridge
	Renders        int // chat renders generated for text-only rows
	RenderFailed   int // chat renders that failed (row kept, render missing)
	Merged         int // rows formed by merging 2+ messages (semantic dedup)
	SimilarNotes   int // rows carrying a "mirip dengan" note (kept separate)
	Agenda         int // agenda candidates extracted (one per event row)
	AgendaComplete int // candidates with date + time ("lengkap")
	UploadOK       int // files uploaded to the remote store
	UploadFailed   int // files whose upload failed (link left empty)
	UploadSkipped  int // rows with no evidence file to upload
	// UploadedFiles maps a local path to its remote URL (evidence
	// files and export files). Empty when upload is disabled.
	UploadedFiles map[string]string
	Files         []string
}

// Collector ties config, store access, and the bridge client together.
type Collector struct {
	cfg      *Config
	bridge   *BridgeClient
	uploader Uploader // nil when upload is disabled (local-only)
	log      *log.Logger
}

// New builds a Collector from a validated config. The bridge client is
// optional: when the token file is missing and the bridge is unreachable,
// collection still works for text; media downloads are recorded as failed.
// When upload is enabled, the remote uploader is built here; a bad
// upload configuration fails New (the config was already validated).
func New(cfg *Config, logger *log.Logger) (*Collector, error) {
	bridge, err := NewBridgeClient(cfg.Bridge.APIURL, cfg.Bridge.TokenFile)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	uploader, err := BuildUploader(context.Background(), cfg.Upload)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	return &Collector{cfg: cfg, bridge: bridge, uploader: uploader, log: logger}, nil
}

// Bridge exposes the client (used by cmd for the health wait).
func (c *Collector) Bridge() *BridgeClient { return c.bridge }

// Collect runs one collection for [since, until) and writes the run folder.
// It never fails the whole run because of one bad message or one failed
// media download: those are recorded in the export and counted.
func (c *Collector) Collect(ctx context.Context, label string, since, until time.Time) (*Summary, error) {
	sum := &Summary{Label: label, WindowStart: since.UTC(), WindowEnd: until.UTC()}

	db, err := OpenStore(c.cfg.Bridge.DBPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	msgs, err := FetchMessages(ctx, db, c.cfg.Collection.Allowlist, since, until)
	if err != nil {
		return nil, err
	}
	sum.Scanned = len(msgs)

	if err := os.MkdirAll(c.cfg.Collection.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}
	state, err := loadState(stateFilePath(c.cfg.Collection.OutputDir))
	if err != nil {
		return nil, err
	}

	runDir, err := RunDir(c.cfg.Collection.OutputDir, until)
	if err != nil {
		return nil, err
	}
	sum.RunDir = runDir

	rows := make([]EvidenceRow, 0, len(msgs))
	stateChanged := false
	// First pass: exact dedup against the state file (message ID +
	// content hash). Messages already collected are skipped.
	var newMsgs []Message
	for _, m := range msgs {
		key := m.StateKey()
		hash := m.ContentHash()
		if prev, ok := state.Seen[key]; ok && prev == hash {
			sum.SkippedSeen++
			continue
		}
		newMsgs = append(newMsgs, m)
		state.Seen[key] = hash
		stateChanged = true
	}

	// Second pass: semantic dedup. Messages that are the same event
	// reworded or cross-posted are grouped, so one event exports as
	// one row with all sources listed. With dedup disabled this
	// returns one singleton group per message.
	groups := groupMessages(newMsgs, c.cfg.Dedup)
	for _, g := range groups {
		row := c.buildRow(ctx, runDir, g, sum)
		if row.Merged {
			sum.Merged++
		}
		if len(g.notes) > 0 {
			sum.SimilarNotes++
		}
		rows = append(rows, row)
	}
	sum.New = len(rows)

	// Agenda extraction: one candidate per message group (the same
	// grouping as the rows above, so one event is one candidate with
	// all sources). A candidate is only produced when the group
	// carries a date or a time; anything else is not an agenda
	// message. This runs on the raw group text (like dedup), before
	// the exports are written.
	var candidates []AgendaCandidate
	if c.cfg.Agenda.Enabled {
		candidates = []AgendaCandidate{}
		for i, g := range groups {
			if cand := extractAgenda(g, rows[i], c.cfg.Agenda, c.cfg.Collection.Redact); cand != nil {
				candidates = append(candidates, *cand)
				if cand.Status == "lengkap" {
					sum.AgendaComplete++
				}
			}
		}
		sum.Agenda = len(candidates)
	}

	// Remote upload of the rows' evidence files (media + chat renders).
	// This runs BEFORE the exports are written, so the remote URLs land
	// in the final JSON/CSV/Excel (Link Drive column). A failed upload
	// leaves the row's link empty; the local file is kept.
	if c.uploader != nil {
		if err := c.uploadEvidence(ctx, runDir, until, rows, sum); err != nil {
			return nil, err
		}
	}

	envelope := EvidenceFile{
		Version:     4, // v4 adds sources / merged / is_correction / note (v0.5.0)
		Label:       label,
		WindowStart: sum.WindowStart.Format(time.RFC3339),
		WindowEnd:   sum.WindowEnd.Format(time.RFC3339),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Rows:        rows,
	}
	stamp := until.Format("150405")
	jsonPath := filepath.Join(runDir, "evidence-"+stamp+".json")
	if err := WriteJSON(jsonPath, envelope); err != nil {
		return nil, err
	}
	csvPath := filepath.Join(runDir, "evidence-"+stamp+".csv")
	if err := WriteCSV(csvPath, rows); err != nil {
		return nil, err
	}
	sum.Files = append(sum.Files, jsonPath, csvPath)

	// Daily Excel recap: same rows as JSON/CSV, same run folder.
	xlsxPath := filepath.Join(runDir, DailyFileName(until))
	if err := WriteRecapXLSX(xlsxPath, rows, label); err != nil {
		return nil, err
	}
	sum.Files = append(sum.Files, xlsxPath)

	// Agenda candidates: their own JSON file in the run folder, plus
	// an "Agenda" sheet in the daily Excel when there is at least
	// one candidate. The JSON is written even with zero candidates
	// (an empty list proves extraction ran). Both go before the
	// manifest, so the manifest covers the final files.
	if c.cfg.Agenda.Enabled {
		agendaPath := filepath.Join(runDir, AgendaFileName(until))
		agendaEnv := AgendaFile{
			Version:     1,
			Label:       label,
			WindowStart: sum.WindowStart.Format(time.RFC3339),
			WindowEnd:   sum.WindowEnd.Format(time.RFC3339),
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Candidates:  candidates,
		}
		if err := WriteAgendaFile(agendaPath, agendaEnv); err != nil {
			return nil, err
		}
		sum.Files = append(sum.Files, agendaPath)
		if len(candidates) > 0 {
			if err := AddAgendaSheet(xlsxPath, candidates); err != nil {
				return nil, err
			}
		}
	}

	// Monthly combined recap: rebuilt from this month's daily files.
	// A merge failure must not lose the daily export, so it is logged
	// and the run continues without the monthly file.
	var manifestExtras []string
	monthlyPath, err := MergeMonthlyRecap(c.cfg.Collection.OutputDir, until)
	if err != nil {
		c.log.Printf("monthly recap merge failed (daily export kept): %v", err)
	} else {
		sum.Files = append(sum.Files, monthlyPath)
		if rel, err := filepath.Rel(runDir, monthlyPath); err == nil {
			manifestExtras = append(manifestExtras, rel)
		}
	}

	if _, err := WriteManifest(runDir, manifestExtras...); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	sum.Files = append(sum.Files, filepath.Join(runDir, "manifest.sha256"))

	// Remote upload of the export files themselves (JSON, CSV, XLSX,
	// manifest, monthly recap). Their URLs are recorded in the Summary
	// and logged; they cannot be in the exports (written above).
	if c.uploader != nil {
		if err := c.uploadExports(ctx, runDir, until, monthlyPath, sum); err != nil {
			return nil, err
		}
	}

	if stateChanged {
		if err := state.save(stateFilePath(c.cfg.Collection.OutputDir)); err != nil {
			return nil, fmt.Errorf("save state: %w", err)
		}
	}
	return sum, nil
}

// downloadMedia fetches one message's media through the bridge with retries
// and copies it into runDir/media/. It returns (status, fileName).
// "skipped" means media is disabled in config; "failed" means every
// attempt failed (bridge down, media expired, ...). Either way the run
// continues and the failure is visible in the export.
func (c *Collector) downloadMedia(ctx context.Context, runDir string, m Message, sum *Summary) (string, string) {
	if !c.cfg.Media.Download {
		sum.MediaSkipped++
		return "skipped", ""
	}
	var lastErr error
	for attempt := 1; attempt <= c.cfg.Media.MaxAttempts; attempt++ {
		src, err := c.bridge.DownloadMedia(ctx, m.ChatJID, m.ID)
		if err == nil {
			// The bridge names its download <type>_<timestamp>_<messageID>.<ext>.
			// Keep that name, prefixed with the (hashed) chat label so files
			// from different chats can never collide in one media/ folder.
			mediaFile := ChatShort(m.ChatJID) + "_" + filepath.Base(src)
			dst := filepath.Join(runDir, "media", mediaFile)
			if err := copyFile(src, dst); err == nil {
				sum.MediaOK++
				return "ok", mediaFile
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		if attempt < c.cfg.Media.MaxAttempts {
			select {
			case <-ctx.Done():
				sum.MediaFailed++
				return "failed", ""
			case <-time.After(time.Duration(c.cfg.Media.RetryDelaySeconds) * time.Second):
			}
		}
	}
	c.log.Printf("media download failed after %d attempts (chat=%s id=%s): %v",
		c.cfg.Media.MaxAttempts, ChatShort(m.ChatJID), m.ID, lastErr)
	sum.MediaFailed++
	return "failed", ""
}

// buildRow turns one message group into a single EvidenceRow. The row's
// top-level fields come from the group's canonical message (latest
// correction when the group holds one, otherwise the longest/most
// complete); every member is listed in Sources with its own chat,
// sender, timestamp, text, and evidence file. Media is downloaded for
// every member that has any; the first successful download becomes the
// row's evidence file. A text-only group with no link in any member gets
// one chat render holding ALL its messages.
func (c *Collector) buildRow(ctx context.Context, runDir string, g messageGroup, sum *Summary) EvidenceRow {
	canon := g.members[g.canonical]
	row := EvidenceRow{
		MessageID:    canon.ID,
		ChatJID:      canon.ChatJID,
		ChatName:     canon.ChatName,
		Sender:       canon.Sender,
		Timestamp:    canon.Timestamp.UTC().Format(time.RFC3339),
		IsFromMe:     canon.IsFromMe,
		Content:      canon.Content,
		MediaType:    canon.MediaType,
		MediaStatus:  "not-applicable",
		ContentHash:  canon.ContentHash(),
		Merged:       len(g.members) > 1,
		IsCorrection: g.correction,
	}
	// Sources (redacted like the row itself).
	for _, m := range g.members {
		src := RowSource{
			MessageID: m.ID,
			ChatJID:   m.ChatJID,
			ChatName:  m.ChatName,
			Sender:    m.Sender,
			Timestamp: m.Timestamp.UTC().Format(time.RFC3339),
			Content:   m.Content,
			MediaType: m.MediaType,
		}
		if c.cfg.Collection.Redact {
			src.Sender = RedactSender(m.Sender)
			src.Content = RedactContent(m.Content)
		}
		row.Sources = append(row.Sources, src)
	}
	if c.cfg.Collection.Redact {
		row.Sender = RedactSender(canon.Sender)
		row.Content = RedactContent(canon.Content)
	}
	// Note: merge note first, then the similarity notes (stable order).
	var notes []string
	if len(g.members) > 1 {
		chats := map[string]struct{}{}
		for _, m := range g.members {
			chats[m.ChatJID] = struct{}{}
		}
		n := fmt.Sprintf("digabung dari %d sumber, %d grup", len(g.members), len(chats))
		if g.correction {
			n += " (ralat)"
		}
		notes = append(notes, n)
	}
	notes = append(notes, g.sortedNotes()...)
	row.Note = strings.Join(notes, "; ")

	// Media: download for every member that has any. The first
	// successful download becomes the row's evidence file; the rest
	// are recorded on their source entries.
	firstMedia := -1
	for i, m := range g.members {
		if m.MediaType == "" {
			continue
		}
		if firstMedia < 0 {
			firstMedia = i
		}
		status, file := c.downloadMedia(ctx, runDir, m, sum)
		row.Sources[i].MediaFile = file
		row.Sources[i].MediaStatus = status
		if status == "ok" && row.MediaFile == "" {
			row.MediaFile = file
			row.MediaStatus = status
			row.MediaType = m.MediaType
		}
	}
	if row.MediaFile == "" && firstMedia >= 0 {
		// No download succeeded: the row keeps the first member's
		// status (failed/skipped), like a single-message media row.
		row.MediaStatus = row.Sources[firstMedia].MediaStatus
		row.MediaType = g.members[firstMedia].MediaType
	}

	// Chat render: only when NO member has media and NO member has a
	// link (a link is evidence by itself), and some member has text.
	// The render holds every member's message, so a merged row's
	// render shows all its sources.
	anyMedia, anyLink, anyText := false, false, false
	for _, m := range g.members {
		if m.MediaType != "" {
			anyMedia = true
		}
		if hasLink(m.Content) {
			anyLink = true
		}
		if strings.TrimSpace(m.Content) != "" {
			anyText = true
		}
	}
	if !anyMedia && !anyLink && anyText {
		if pngName, err := c.renderChatGroup(runDir, g, row); err != nil {
			c.log.Printf("chat render failed (chat=%s id=%s): %v", ChatShort(canon.ChatJID), canon.ID, err)
			sum.RenderFailed++
		} else {
			row.ChatRender = pngName
			sum.Renders++
		}
	}
	return row
}

// renderChatGroup renders all messages of a group as one WhatsApp-Web-
// style chat render (a RENDER drawn from text, never a screenshot) into
// runDir/media/. The file name is HHMM_<chat-label>_chat.png for the
// canonical message's time (WIB); a -2, -3, ... suffix avoids
// overwriting when two groups from one chat share a minute. The render
// uses the redacted senders and content from the row's sources.
func (c *Collector) renderChatGroup(runDir string, g messageGroup, row EvidenceRow) (string, error) {
	canon := g.members[g.canonical]
	name := RenderFileName(canon.Timestamp, canon.ChatJID)
	mediaDir := filepath.Join(runDir, "media")
	pngName := uniqueRenderName(mediaDir, name)
	msgs := make([]RenderMessage, 0, len(g.members))
	for i, m := range g.members {
		msgs = append(msgs, RenderMessage{Sender: row.Sources[i].Sender, Timestamp: m.Timestamp, Text: row.Sources[i].Content})
	}
	if err := RenderChat(msgs, RenderDatePill(canon.Timestamp), filepath.Join(mediaDir, pngName)); err != nil {
		return "", err
	}
	return pngName, nil
}

// uniqueRenderName returns name when it does not exist in dir, otherwise
// name with a -2, -3, ... suffix before the extension.
func uniqueRenderName(dir, name string) string {
	if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
}

// uploadEvidence uploads each row's evidence file (downloaded media
// or chat render PNG) to <YYYYMMDD>/media/ and sets the row's
// LinkDrive to the returned URL. Rows with no evidence file are
// counted as skipped. A failed upload is logged and counted; the row
// keeps its local file and an empty link. With Strict, the first
// failure fails the run instead.
func (c *Collector) uploadEvidence(ctx context.Context, runDir string, runDate time.Time, rows []EvidenceRow, sum *Summary) error {
	for i := range rows {
		var fileName string
		switch {
		case rows[i].MediaStatus == "ok" && rows[i].MediaFile != "":
			fileName = rows[i].MediaFile
		case rows[i].ChatRender != "":
			fileName = rows[i].ChatRender
		default:
			sum.UploadSkipped++
			continue
		}
		localPath := filepath.Join(runDir, "media", fileName)
		remotePath := remoteMediaPath(runDate, fileName)
		url, err := uploadOne(ctx, c.uploader, localPath, remotePath, c.cfg.Upload.MaxAttempts, c.cfg.Upload.RetryDelaySeconds)
		if err != nil {
			c.log.Printf("upload failed (%s %s): %v", c.uploader.Name(), remotePath, err)
			sum.UploadFailed++
			if c.cfg.Upload.Strict {
				return fmt.Errorf("upload %s: %w", remotePath, err)
			}
			continue
		}
		rows[i].LinkDrive = url
		sum.UploadOK++
		if sum.UploadedFiles == nil {
			sum.UploadedFiles = map[string]string{}
		}
		sum.UploadedFiles[localPath] = url
		// A merged row can hold more media than its primary evidence
		// file (one per source). Upload those too, so every source's
		// file gets a link; the primary file above is skipped here.
		for si := range rows[i].Sources {
			f := rows[i].Sources[si].MediaFile
			if f == "" || f == fileName {
				continue
			}
			srcPath := filepath.Join(runDir, "media", f)
			srcRemote := remoteMediaPath(runDate, f)
			srcURL, err := uploadOne(ctx, c.uploader, srcPath, srcRemote, c.cfg.Upload.MaxAttempts, c.cfg.Upload.RetryDelaySeconds)
			if err != nil {
				c.log.Printf("upload failed (%s %s): %v", c.uploader.Name(), srcRemote, err)
				sum.UploadFailed++
				if c.cfg.Upload.Strict {
					return fmt.Errorf("upload %s: %w", srcRemote, err)
				}
				continue
			}
			rows[i].Sources[si].LinkDrive = srcURL
			sum.UploadOK++
			if sum.UploadedFiles == nil {
				sum.UploadedFiles = map[string]string{}
			}
			sum.UploadedFiles[srcPath] = srcURL
		}
	}
	return nil
}

// uploadExports uploads the run's export files (JSON, CSV, daily XLSX,
// manifest) to <YYYYMMDD>/dokumen/ and the monthly recap (when it was
// merged) to the remote root. Their URLs are recorded in the Summary
// and logged. Failures follow the same rule as uploadEvidence: logged
// and counted, fatal only with Strict.
func (c *Collector) uploadExports(ctx context.Context, runDir string, runDate time.Time, monthlyPath string, sum *Summary) error {
	docs := []string{}
	for _, name := range sum.Files {
		// sum.Files holds the export paths written this run (JSON,
		// CSV, XLSX, monthly, manifest). The monthly file lives at
		// the output root; the rest live in the run dir.
		if name == monthlyPath {
			continue
		}
		docs = append(docs, name)
	}
	for _, localPath := range docs {
		remotePath := remoteDocPath(runDate, filepath.Base(localPath))
		url, err := uploadOne(ctx, c.uploader, localPath, remotePath, c.cfg.Upload.MaxAttempts, c.cfg.Upload.RetryDelaySeconds)
		if err != nil {
			c.log.Printf("upload failed (%s %s): %v", c.uploader.Name(), remotePath, err)
			sum.UploadFailed++
			if c.cfg.Upload.Strict {
				return fmt.Errorf("upload %s: %w", remotePath, err)
			}
			continue
		}
		sum.UploadOK++
		if sum.UploadedFiles == nil {
			sum.UploadedFiles = map[string]string{}
		}
		sum.UploadedFiles[localPath] = url
	}
	if monthlyPath != "" {
		url, err := uploadOne(ctx, c.uploader, monthlyPath, filepath.Base(monthlyPath), c.cfg.Upload.MaxAttempts, c.cfg.Upload.RetryDelaySeconds)
		if err != nil {
			c.log.Printf("upload failed (%s %s): %v", c.uploader.Name(), filepath.Base(monthlyPath), err)
			sum.UploadFailed++
			if c.cfg.Upload.Strict {
				return fmt.Errorf("upload %s: %w", filepath.Base(monthlyPath), err)
			}
		} else {
			sum.UploadOK++
			if sum.UploadedFiles == nil {
				sum.UploadedFiles = map[string]string{}
			}
			sum.UploadedFiles[monthlyPath] = url
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
