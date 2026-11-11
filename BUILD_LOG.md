# Build log — wa-evidence-collector v0.1.0 scaffold

Date: 2026-11-11. Go 1.27.3 (`~/workspace/tools/go/bin/go`).
Upstream: verygoodplugins/whatsapp-mcp, commit `a1af56c43a8e8ec4196f4c65e520a12d44bf79cc`
(local checkout at `~/workspace/custom-connectors/custom.whatsapp/whatsapp-mcp/`).

## What was copied (upstream, as-is except noted)

- `bridge/` — full Go bridge (module `whatsapp-client`, go 1.26.0),
  incl. all tests, `go.mod`, `go.sum`, `.golangci.yml`.
  **Local patches ported** (documented in README.md, "Local changes vs
  upstream"): (1) `downloadMedia` fallback `downloadViaOriginalURL` +
  `.enc` decrypt (`main.go` lines ~32, 2466, 2483-2490, 2518);
  (2) pairing QR written to `store/qr.png` + `store/qr.txt`
  (`main.go` lines ~3255, 3257); (3) `pairphone/` helper for code pairing.
  Non-test code normalizations: phone-like example digits replaced with
  placeholders (`pairphone/main.go:6` → `62XXXXXXXXXX`;
  `bridge/main.go:1223` vCard example → `62XXXXXXXXXX` /
  `+62 8XX-XXXX-XXXX`). Upstream test fixtures left as-is (fictional
  555-range numbers, sequential placeholders, Unix timestamps,
  test tokens) — they are upstream's own synthetic data.
- `whatsapp-mcp-server/` — upstream Python MCP server, unmodified
  (version 0.7.0, Python >= 3.11). The collector does not depend on it.
- `pairphone/` — code-pairing helper, own Go module (go 1.27.3).

Verified at copy time: **0 live-data files** — no `.db`, no `store/`,
no binaries, no tokens in the copy.

## What was written new

- `go.mod` (root module `github.com/wa-evidence-collector/wa-evidence-collector`,
  go 1.27; `mattn/go-sqlite3 v1.14.52`, `robfig/cron/v3 v3.0.1`,
  `gopkg.in/yaml.v3 v3.0.1`).
- `collector/config.go` — Config, LoadConfig, Validate, defaults
  (API `http://127.0.0.1:8080/api`, token `./bridge/store/.bridge-token`,
  DB `./bridge/store/messages.db`, binary `./bridge/whatsapp-bridge`,
  autostart true, health timeout 30s, lookback 24h, redact true,
  media max attempts 3, retry delay 30s).
- `collector/store.go` — Message, ContentHash (sha256 of chat|id|timestamp|content),
  StateKey (chat|id), OpenStore read-only (`mode=ro` + `query_only`),
  FetchMessages (allowlist + `[since, until)` window, deleted skipped,
  oldest first).
- `collector/redact.go` — RedactSender (`sender:<sha256[:8]>`),
  RedactContent (masks JIDs / wa.me links / phone-like digit runs),
  ChatShort (`g-<hash8>` / `dm-<hash8>` / `c-<hash8>`).
- `collector/media.go` — BridgeClient (token file read, Health,
  DownloadMedia via `POST /api/download`).
- `collector/export.go` — EvidenceRow, EvidenceFile envelope,
  WriteJSON, WriteCSV (11 columns), WriteManifest (sha256sum format),
  RunDir (`output/YYYYMMDD/` + `media/`).
- `collector/state.go` — runState, `output_dir/state.json` (content
  hashes only, never message text).
- `collector/collector.go` — Collector type, Collect orchestration
  (fetch → dedup vs state → redact → media download with retries →
  copy into run dir → JSON/CSV → manifest → update state → Summary).
- `collector/collector_test.go` — FetchMessages, redaction, config
  validation, end-to-end Collect (stub bridge), media-failure path,
  example-config parse.
- `scheduler/scheduler.go` — robfig/cron/v3 wrapper (5-field +
  descriptors, SkipIfStillRunning, Recover, Stop waits for running job).
- `scheduler/scheduler_test.go` — invalid spec, job fires, Stop waits.
- `cmd/wa-evidence-collector/main.go` — flags `--config` (default
  `config.yaml`), `--once`, `--version`; bridge autostart as subprocess
  + health wait (degraded mode on timeout, not fatal); scheduler start
  or single run; SIGINT/SIGTERM handling.
- `config.yaml.example` — placeholders only
  (`120363000000000000@g.us`, 555-range DM).
- `bridge/.env.example` — fresh minimal env example, placeholders only.
- `.gitignore` — `store/`, `*.db`, `*.db-journal`, `.env`, tokens,
  `qr.png`, `qr.txt`, `config.yaml`, `evidence/`, binaries.
- `README.md`, `LICENSE` (MIT, upstream attribution preserved),
  this file.

## What builds / verifies (all green 2026-11-11)

- `go build ./...` (root) — pass. `go vet ./...` (root) — pass.
  `go test ./...` (root) — pass (collector 0.018s, scheduler 1.9s).
- `go build ./...` + `go vet ./...` (bridge module) — pass.
  `go test ./...` (bridge, upstream suite) — pass (`ok whatsapp-client 0.688s`).
- `go build ./...` + `go vet ./...` (pairphone module) — pass.
- All three binaries produced with `-o`: `wa-evidence-collector`
  (14.6 MB), `bridge/whatsapp-bridge` (25.2 MB), `pairphone/pairphone`
  (23.7 MB). Binaries removed after verification (gitignored).
- Synthetic end-to-end test (`/tmp/e2e-wa-collector-test.py`, fake
  SQLite store + stub bridge HTTP server, never the live store):
  **12/12 checks passed** — allowlist/window/deleted filtering,
  redaction, media copy, manifest hash verification, second run
  exports 0 new / 3 skipped-seen.
- Privacy scan (`grep -rE '[0-9]{10,}'`): no real phone numbers,
  JIDs, tokens, or chat content anywhere. Remaining digit runs are
  (a) fictional 555-range / all-zero placeholders in new code, docs,
  and tests, (b) upstream test fixtures (555 numbers, sequential
  placeholders, Unix timestamps, test tokens), (c) module hashes and
  pseudo-version timestamps in `go.mod`/`go.sum`, (d) one upstream
  CDN-URL example comment in `bridge/main.go`. No `store/`, no `.db`,
  no token, no `qr.*`, no `config.yaml` in the repo.

## What is stubbed / not done (honest list)

- **Excel exporter** — JSON + CSV only.
- **Drive / S3 upload** — exports stay local under `output_dir`.
- **Chat-render screenshots** — rows without media have no image evidence.
- **Semantic dedup** — dedup is exact (message ID + content hash);
  reworded cross-posts export twice.
- **Agenda extraction** — no date/time parsing, no calendar integration.
- **MCP server** — included unmodified; no collector integration.

## Exact next steps

1. Pair the bridge once (QR via `store/qr.png` or `pairphone/` code).
2. `cp config.yaml.example config.yaml`, set the real allowlist.
3. `./wa-evidence-collector --config config.yaml --once` for a smoke run.
4. Pick the next feature from "What is not done yet" (Excel exporter
   is the natural first one — the upstream workflow's recap was an
   11-column Excel file).
5. When ready to publish: `git remote add origin <url>` + push.
   (Local-only so far by design; no remote configured.)

---

# Build log — v0.2.0: Excel exporter

Date: 2026-11-11. Go 1.27.3. Library: `github.com/xuri/excelize/v2
v2.11.1` (note: `github.com/qax-os/excelize/v2@latest` resolves to a
module that declares its path as `github.com/xuri/excelize/v2`, so the
xuri path is the one required; fetched fine from proxy.golang.org).

## What was added / changed

- `collector/excel.go` (new) — `RowToRecap` (EvidenceRow → 11 recap
  columns), `WriteRecapXLSX` (daily file), `ReadRecapXLSX`
  (reopen + header validation, rows padded to 11 columns),
  `MergeMonthlyRecap` (rebuild the monthly file from the month's daily
  files), `DailyFileName` / `MonthlyFileName`, `verifyRecapFile`
  (every written file is reopened and checked: header + row count).
  Formatting: dark-blue header fill `1F3864` + white bold text, frozen
  top row, autofilter, wrapped text, thin grey borders, landscape +
  fit-to-one-page-wide, sane column widths. All cells are plain strings
  via `SetCellStr` (a value starting with "=" can never become a
  formula; excelize truncates cells at 32767 chars). Dates/times are
  WIB via a fixed UTC+7 zone (no tzdata dependency).
- `collector/collector.go` — `Collect` now writes
  `Rekap-Evidence-<YYYYMMDD>.xlsx` next to the JSON/CSV, then rebuilds
  the monthly `Rekap-Evidence-<YYYYMM>-gabungan.xlsx` at the output
  root. A merge failure is logged and non-fatal (the daily export is
  kept); a daily-xlsx write failure is fatal like JSON/CSV.
- `collector/export.go` — `WriteManifest` takes variadic extra
  relative paths; the monthly file is listed in the run-dir manifest as
  `../Rekap-Evidence-<YYYYMM>-gabungan.xlsx`, so `sha256sum -c`
  works from inside the run dir.
- `collector/excel_test.go` (new) — header order, row mapping
  (incl. UTC→WIB date rollover, redacted values, media/link/doc
  kinds), formatting markers (header fill/font, wrap, freeze, landscape
  + fit, column width, autofilter + no-formula via sheet XML),
  monthly merge (date order, other-month exclusion, empty month),
  non-recap rejection.
- `collector/collector_test.go` — `TestCollectEndToEnd` now asserts the
  daily xlsx (reopens, header, 3 redacted rows, hashed Grup labels)
  and the monthly file (3 rows), and that the manifest lists both.
- `go.mod` / `go.sum` — excelize v2.11.1 + its deps.
- `/tmp/e2e-wa-collector-test.py` — extended (hermetic: cleans stale
  evidence/state first): daily xlsx presence/header/rows/redaction,
  no formulas, header fill, freeze, landscape fit, monthly file +
  rows, manifest lists both xlsx files. Now 25 checks (was 12).
- `README.md` — v0.2.0 status, Excel in the data-flow diagram and
  "What a run produces" (incl. the 11 columns, mapping rules, and the
  re-run caveat below), privacy note on the redacted recap, Excel
  removed from the stub list.

## Mapping decisions (documented in code + README)

- Grup = hashed chat label (`ChatShort`), Pengirim = redacted sender
  id, same as JSON/CSV. No new fields invented.
- Jenis Kegiatan / Jenis Evidence are coarse derivations from
  MediaType/MediaStatus/link presence, not content classification.
- Link Drive is empty (local-only); Konteks is the run label.
- Monthly merge concatenates the daily files of the month in date
  order. Known limitation: daily files hold only rows that were new in
  their run, so re-running the same window overwrites that day's file
  with the (possibly empty) re-run rows and the next monthly rebuild
  reflects the latest daily files.

## Verification (all green 2026-11-11)

- `go build ./...`, `go vet ./...`, `go test ./...` (root) — pass
  (collector incl. 5 new Excel tests, scheduler cached).
- `go build` + `go vet` + `go test` (bridge module, upstream suite)
  — pass (`ok whatsapp-client 0.595s`). `go build` + `go vet`
  (pairphone) — pass.
- Synthetic e2e (`/tmp/e2e-wa-collector-test.py`, fake store + stub
  bridge, openpyxl 3.1.2 reopening the files independently):
  **25/25 checks passed** (was 12/12).
- Privacy scan (`grep -rE '[0-9]{10,}'` over collector/, scheduler/,
  cmd/, *.md, config.yaml.example, excluding the known fictional
  555-range / all-zero placeholders): no real phone numbers, JIDs,
  tokens, or chat content. New code/docs/tests use fictional
  555-range / all-zero fixtures only.

## Updated stub list

- **Excel exporter** — DONE (this release).
- **Drive / S3 upload** — exports stay local under `output_dir`.
- **Chat-render screenshots** — rows without media have no image evidence.
- **Semantic dedup** — dedup is exact (message ID + content hash);
  reworded cross-posts export twice.
- **Agenda extraction** — no date/time parsing or calendar integration.
- **MCP server** — included unmodified; no collector integration.

## Exact next steps

1. Pair the bridge once, `cp config.yaml.example config.yaml`, smoke
   run `--once` (unchanged from v0.1.0).
2. Natural next feature: Drive/S3 upload (fills the Link Drive column)
   or chat-render screenshots (image evidence for text-only rows).
3. When ready to publish: `git remote add origin <url>` + push.

# Build log — v0.3.0: chat-render screenshots

## What changed

- **Go-native chat renderer** (`collector/render.go`, new, ~600 lines):
  draws WhatsApp-Web-style PNG renders of the verbatim message text
  for text-only rows, matching the reference Python renderer
  (template v3, verified 10 Nov 2026): white background with a faint
  doodle pattern, centered date pill, grey incoming-style bubbles with
  tail + read-check badge, green sender name, *bold* and _italic_
  WhatsApp markup honoured (markup not shown), verbatim text incl.
  typos, per-message timestamps, avatar circle with the sender's
  initial at the bottom left. `golang.org/x/image` (font/opentype +
  vector); Noto Sans from the system font dirs first, DejaVu second,
  embedded Go fonts last (`WA_RENDER_FONT_DIR` overrides). Own
  messages render in the same incoming-bubble style as the reference;
  no green right-aligned variant yet.
- **Wiring** (`collector/collector.go`): after dedup, a row with no
  media type, non-blank text, and no link gets a render into
  `media/` named `HHMM_<chat-label>_chat.png` (WIB time; a `-2`,
  `-3`, ... suffix when two text-only messages from one chat share a
  minute, so no render is ever overwritten). `row.ChatRender` is set
  and `Summary` gains `Renders` / `RenderFailed`; a failed render is
  logged, the row is kept without a render. Renders use the redacted
  sender/content. Run summary (`cmd/wa-evidence-collector/main.go`)
  now prints `renders=N render-failed=N`. JSON envelope version
  bumped 1 -> 2 (new `chat_render` field).
- **Exports** (`collector/export.go`, `collector/excel.go`):
  `EvidenceRow.ChatRender` added to JSON + CSV (`chat_render`
  column); Excel `RowToRecap` maps a rendered row to Jenis Evidence
  `Screenshot chat (render)`, Nama File the PNG name, Status Unduhan
  `dibuat dari teks pesan`.
- **Performance fix found by testing** (worth knowing): the vector
  rasterizer's buffers are width x height samples and `Draw` touches
  the whole buffer, so a full-image rasterizer per shape made the
  12000-rune render take 4+ minutes. Shapes are now rasterized with
  a rasterizer sized to the shape's bounding box (`fillPath` +
  `rectFromFloat`); the long-text render takes 0.7s.
- Defensive limit: a message over 12000 runes is truncated with a
  `[...]` marker before layout (full text stays in JSON/CSV/Excel).

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` all pass
  (collector 1.3s; full suite 3.2s).
- New tests (`collector/render_test.go`, 8 tests): markup parsing
  (bold/italic/unclosed/other-marker-literal/blank lines), wrapping
  (line widths within the bubble), PNG output (1320 wide, height
  >= 400, bubble pixels >= 50000, non-blank), multi-message height,
  zero-message error, long-text cap (30000 runes -> height < 20000),
  filename + date pill, and `TestCollectRenderRules` (text-only
  renders, image-fail / link / blank do not). Existing e2e tests
  extended: renders=2, media files=3, JSON `chat_render` checks,
  Excel render semantics; `TestCollectMediaFailure` asserts renders
  still happen when media fails.
- Synthetic e2e (`/tmp/e2e-wa-collector-test.py`, fake store + stub
  bridge, openpyxl + PIL reopening the outputs independently):
  **33/33 checks passed** (was 25/25). Renders visually verified:
  date pill, green sender, bold/italic, wrapped lines, timestamps,
  check badge, avatar initial, doodle background.
- Privacy scan (`grep -rE '[0-9]{10,}'` over collector/, scheduler/,
  cmd/, *.md, config.yaml.example, excluding the known fictional
  555-range / all-zero placeholders): no real phone numbers, JIDs,
  tokens, or chat content. The only 10+ digit match is the kappa
  circle constant `0.5522847498307936` in render.go. New code/docs/
  tests use fictional 555-range / all-zero fixtures only.

## Updated stub list

- **Chat-render screenshots** — DONE (this release).
- **Drive / S3 upload** — exports stay local under `output_dir`.
- **Semantic dedup** — dedup is exact (message ID + content hash);
  reworded cross-posts export twice.
- **Agenda extraction** — no date/time parsing or calendar integration.
- **MCP server** — included unmodified; no collector integration.

## Exact next steps

1. Pair the bridge once, `cp config.yaml.example config.yaml`, smoke
   run `--once` (unchanged from v0.1.0).
2. Natural next feature: Drive/S3 upload (fills the Link Drive column)
   or semantic dedup (one row per event across reworded cross-posts).
3. When ready to publish: `git remote add origin <url>` + push.

---

# Build log — v0.4.0: remote upload (Google Drive + S3-compatible)

Date: 2026-11-11. Go 1.27.3. Libraries (both fetched fine from
proxy.golang.org): `google.golang.org/api v0.312.0` (drive/v3) and
`github.com/aws/aws-sdk-go-v2 v1.48.0` + `config v1.33.18` +
`service/s3 v1.114.10` (+ credentials, sso/sts/imds deps pulled in by
config).

## What was added / changed

- `collector/upload.go` (new) — `Uploader` interface (`Upload`,
  `Exists`, `ExistingURL`, `Name`), `BuildUploader` factory
  ((nil, nil) when disabled), `uploadOne` (Exists check, then up to
  MaxAttempts with RetryDelaySeconds between attempts), remote-path
  helpers (`remoteMediaPath` / `remoteDocPath` / `splitRemotePath`).
- `collector/upload_drive.go` (new) — `DriveUploader` (official
  `drive/v3` client, `option.WithCredentialsFile` for service-account
  or OAuth authorized-user JSON). Root folder by `folder_id` (verified
  to exist and be a folder) or by `folder_path` (resolved/created
  segment by segment from the Drive root). Date/media/dokumen folders
  are found-or-created (cached per run). Upload returns `webViewLink`
  (fallback `https://drive.google.com/file/d/<id>/view`). No sharing
  changes are made. Idempotency: list by name in the target folder,
  compare size.
- `collector/upload_s3.go` (new) — `S3Uploader` (aws-sdk-go-v2).
  Custom endpoint resolver for S3-compatible endpoints; auth from the
  default chain (env vars) or static config credentials when both are
  set. Keys are `<prefix>/<YYYYMMDD>/media/...` and
  `<prefix>/<YYYYMMDD>/dokumen/...`. URL is `public_url_base/key` when
  set, else `endpoint/bucket/key` (path style) or virtual-host style
  when `use_path_style` is false. Idempotency: HeadObject +
  ContentLength comparison (404 = missing). No bucket-policy or ACL
  changes.
- `collector/config.go` — `UploadConfig` (+ `DriveUploadConfig`,
  `S3UploadConfig`) with `Validate` (disabled = no validation;
  enabled requires backend drive/s3, drive credentials file that
  exists + folder_id or folder_path, s3 bucket + endpoint + region,
  max_attempts >= 1). Defaults: disabled, backend drive, 3 attempts,
  10s delay, strict false, s3 region "auto", path style true.
- `collector/export.go` — `EvidenceRow.LinkDrive` (`json` + `csv`);
  CSV header gains `link_drive` as the last column.
- `collector/excel.go` — `RowToRecap` fills the Link Drive column
  from `r.LinkDrive` (was hardcoded empty).
- `collector/collector.go` — `Collector.uploader` built in `New`
  when enabled; `Summary` gains `UploadOK` / `UploadFailed` /
  `UploadSkipped` / `UploadedFiles` (local path -> remote URL).
  `Collect` uploads each row's evidence file (media or chat render)
  to `<YYYYMMDD>/media/` BEFORE writing exports, so the URL lands in
  JSON/CSV/Excel; then uploads the export files (JSON, CSV, XLSX,
  manifest) to `<YYYYMMDD>/dokumen/` and the monthly recap to the
  remote root. Failures are logged/counted and non-fatal unless
  `strict`. JSON envelope version bumped 2 -> 3 (new `link_drive`).
- `cmd/wa-evidence-collector/main.go` — run summary prints
  `upload ok=N failed=N skipped=N` and the uploaded local -> remote
  URLs.
- `config.yaml.example` — `upload:` section, placeholders only
  (all-zero folder ID, fictional R2 endpoint, example bucket name).
- `.gitignore` — `credentials/`, `*-service-account.json`,
  `*-credentials.json`.
- `README.md` — v0.4.0 status, upload in the data-flow diagram and
  config table, new "Remote upload" section (both backends, auth,
  layout, idempotency, retries), privacy note, upload removed from
  the stub list.
- `/tmp/e2e-wa-collector-test.py` — 4 new checks (JSON `link_drive`
  present + empty, CSV header ends with `link_drive` + empty values,
  run summary `upload ok=0`, XLSX Link Drive empty). Now 37 checks
  (was 33).

## Design decisions (documented in code + README)

- Upload runs after rows are built but BEFORE exports are written:
  the per-row URLs must be in the final exports. The export files'
  own URLs cannot be in the exports; they go to the Summary/log.
- Idempotency is name + size only, NOT a content hash (Drive does
  store md5Checksum, S3 stores ETag, but the interface contract is
  name + size; the limitation is documented). File names embed
  message IDs and timestamps, so same-name-different-content at the
  same size is unlikely.
- Drive files are NOT shared on upload; `webViewLink` opens for the
  credentials' owner. S3 URLs for a private bucket are a pattern
  (needs signing or public-read to open); the backend never changes
  bucket policy or ACLs.
- `config.WithCredentials` does not exist in aws-sdk-go-v2/config
  v1.33.18; the correct option is `config.WithCredentialsProvider`
  (found by reading the module source).

## Verification (all green 2026-11-11)

- `go build ./...`, `go vet ./...`, `go test ./...` (root) — pass
  (collector incl. 15 new upload tests, scheduler cached); `gofmt`
  clean.
- `go build` + `go vet` (bridge module, upstream suite) — pass.
  `go build` + `go vet` (pairphone) — pass.
- New tests (`collector/upload_test.go`, 9 tests): `uploadOne`
  idempotency (same size skips, different size uploads), retry-then-
  succeed, failure after max attempts, remote-path helpers, upload
  config validation (all backends), end-to-end Collect with a fake
  in-memory uploader (8 uploads: 3 evidence + 5 exports; remote
  layout media/dokumen/monthly-at-root; `link_drive` in JSON + CSV +
  daily + monthly Excel), upload failure non-fatal (links empty,
  local exports kept), strict mode fails the run, upload-disabled run
  identical to v0.3.0 (empty links).
- `collector/upload_drive_test.go` (4 tests): fake in-memory Drive
  HTTP server (List/Get/Create + multipart + resumable upload
  endpoints). Upload creates the date/media folders, returns a
  webViewLink; Exists compares sizes; ExistingURL returns the same
  URL; folder-path root resolution creates the chain once (cached);
  folder-ID root verification rejects non-folders and missing IDs;
  quoted file names do not break the list query. The client used
  multipart upload for the small test files (verified from the stub's
  request log).
- `collector/upload_s3_test.go` (2 tests): fake in-memory S3 HTTP
  server (HEAD + PUT). Upload stores the object under
  `<prefix>/<date>/...` and returns the endpoint/bucket/key URL;
  Exists compares ContentLength (404 = missing); ExistingURL needs
  no request; public-base + no-prefix variant verified.
- Synthetic e2e (`/tmp/e2e-wa-collector-test.py`, fake store + stub
  bridge, openpyxl + PIL reopening the outputs independently):
  **37/37 checks passed** (was 33/33).
- Privacy scan (`grep -rE '[0-9]{10,}'` over collector/, scheduler/,
  cmd/, *.md, config.yaml.example, excluding the known fictional
  555-range / all-zero placeholders): no real phone numbers, JIDs,
  tokens, or chat content. The only 10+ digit match is the kappa
  circle constant `0.5522847498307936` in render.go. New code/docs/
  tests use fictional 555-range / all-zero fixtures only.

## What is UNTESTED against the real service (honest)

- **Drive auth against real Google**: the stub tests use
  `option.WithoutAuthentication()` + a stub endpoint. The production
  path (`option.WithCredentialsFile` with a real service-account or
  OAuth JSON) has never run here — no real credentials exist in this
  repo and none were used. First real use needs a service-account
  JSON + a `--once` smoke run; a wrong credential file fails fast in
  `New` (client construction) or at root-folder verification.
- **S3 against a real endpoint**: the stub tests cover the SDK's
  HeadObject/PutObject flow and URL building, but no real bucket
  (AWS/MinIO/R2) was contacted. First real use needs a `--once`
  smoke run with the real endpoint + credentials.

## Updated stub list

- **Drive / S3 upload** — DONE (this release).
- **Semantic dedup** — dedup is exact (message ID + content hash);
  reworded cross-posts export twice.
- **Agenda extraction** — no date/time parsing or calendar integration.
- **MCP server** — included unmodified; no collector integration.

## Exact next steps

1. Pair the bridge once, `cp config.yaml.example config.yaml`, smoke
   run `--once` (unchanged from v0.1.0). For a first upload smoke
   test: put a Drive service-account JSON in `credentials/` (never
   commit it) or S3 credentials in the environment, enable upload,
   run `--once`, and check the Link Drive column.
2. Natural next feature: semantic dedup (one row per event across
   reworded cross-posts).
3. When ready to publish: `git remote add origin <url>` + push.

---

# Build log — v0.5.0: semantic dedup

Date: 2026-11-11. Go 1.27.3. No new dependencies (pure Go, stdlib only).

## What was added / changed

- `collector/dedup.go` (new) — `messageSig` (normalized token set +
  links incl. Zoom IDs, explicit dates/times, media keys, correction
  flag), `signatureOf` (links/times/dates extracted then blanked from
  the token text, Indonesian stopwords incl. event-type words dropped),
  `decidePair` (the 9-step decision order: window, correction,
  date conflict, time conflict, shared Zoom, shared media/link with
  support, corroborated overlap, plain overlap, note), `groupMessages`
  (greedy clustering, oldest first; a conflict from any member blocks
  joining the whole group; mutual "mirip dengan" notes reference the
  other group's first message ID, times/dates shown own-first),
  `pickCanonical` (latest correction wins, else most content words).
- `collector/collector.go` — `Collect` now runs exact dedup first
  (unchanged), then `groupMessages`, then `buildRow` per group
  (replaces the inline per-message loop). `Summary` gains `Merged`
  (rows from 2+ messages) and `SimilarNotes` (rows with a
  "mirip dengan" note); JSON envelope version bumped 3 -> 4 (new
  `sources` / `merged` / `is_correction` / `note` fields). Media is
  downloaded for every member with media; the first success becomes
  the row's file, the rest are recorded on their sources. A text-only
  group with no link in any member gets ONE render holding all its
  messages (`renderChatGroup`, replacing the single-message
  `renderChat`). `uploadEvidence` also uploads non-primary source
  media files and records their URLs on the sources.
- `collector/export.go` — `RowSource` (one message behind a row) and
  `EvidenceRow.Sources` / `Merged` / `IsCorrection` / `Note` (JSON
  only). The CSV stays flat at 13 columns on purpose (the upload
  tests assert `link_drive` is last); full merge detail lives in
  JSON + Excel.
- `collector/excel.go` — `RowToRecap`: a merged row lists every
  distinct chat label in Grup and every distinct sender in Pengirim,
  and the Konteks column appends the row note to the run label.
- `collector/config.go` — `DedupConfig` + `Validate` (thresholds in
  (0,1], note < threshold, correction <= threshold, window > 0;
  skipped when disabled) and defaults (enabled, 0.55 / 0.35 / 0.30,
  48h, markers ralat/koreksi/revisi/perubahan/update/perbaikan/
  diperbaiki).
- `cmd/wa-evidence-collector/main.go` — run summary prints
  `merged=N similar=N`.
- `config.yaml.example` — `dedup:` section, annotated.
- `README.md` — v0.5.0 status, dedup config table, new "Semantic
  dedup" section (rules, signals, honest limits), stub list updated.

## Design decisions (documented in code + README)

- Semantic dedup is a SECOND pass after exact dedup; it never
  replaces it. Grouping runs on raw (unredacted) message text:
  redaction masks phone-like digit runs, which would destroy the
  Zoom-ID/link signals if dedup ran after it. Signatures live in
  memory only; the state file still holds hashes only.
- Sender is never a signal: cross-posts come from different senders.
- Correction is checked before the date/time conflict rules, because
  a correction may change the date or time ("Ralat: ... diundur jadi
  jam 13:00"). It still needs real similarity (overlap >= correction
  threshold, or a shared link/media/date).
- Overlap at note level + a shared explicit date/time merges (rule 7):
  a reworded copy often drops words, so its overlap alone can sit
  below the merge threshold while the entity agreement corroborates
  it. A shared Zoom ID merges alone (one meeting); a shared generic
  link or media key needs supporting overlap.
- Time conflict = both carry explicit event times differing by more
  than 15 minutes (a range like 09:00-12:00 counts by its closest
  end). A date conflict is the same idea for explicit dates.
- Notes reference the other group's FIRST message ID (stable: the
  oldest member never changes), and show each row's own times/dates
  first ("jam berbeda: 09:00-12:00 vs 13:00" on the 09:00 row).
- Canonical text: latest correction when the group holds one,
  otherwise the member with the most content words (longest/most
  complete); ties go to the earliest.

## Bugs found by testing (fixed in this release)

- The month-name date regex was case-sensitive, so "11 November"
  never matched (only lowercase "november" did). Fixed with `(?i)`.
- The numeric date regex matched "00-12" inside a time range
  ("09:00-12:00"), inventing a date and breaking time extraction
  (the blanked span destroyed "09:00"). Fixed by extracting times
  BEFORE dates, plus a guard so a dotted date ("11.11.2026") is not
  read as a time.
- `addMutualNote` showed the conflict times in the wrong order
  (other group's times first). Fixed: each note shows its own
  group's times/dates first.

## Verification (all green 2026-11-11)

- `go build ./...`, `go vet ./...`, `go test ./...` (root) — pass
  (collector incl. 11 new dedup tests, scheduler cached); `gofmt`
  clean (root module + cmd; `pairphone/main.go` was already
  unformatted upstream and is left untouched).
- `go build` + `go vet` (bridge module, upstream suite) — pass.
  `go build` + `go vet` (pairphone) — pass.
- New tests (`collector/dedup_test.go`, 6 test functions): signature
  extraction (links/Zoom/date/time/token rules), date/time forms
  (incl. "Versi 2.7.1" not being a time), correction markers +
  media keys, Jaccard, the six required fictional pairs (reworded
  merge, cross-post merge, same-topic-different-time kept with a
  "jam berbeda" note, correction merge with canonical = the ralat,
  different events sharing words kept apart with no note, forwarded
  copies merge) + date-conflict pair, shared-Zoom merge despite
  short text, 48h window cutoff, disabled dedup = singletons,
  config validation.
- End-to-end (`collector/collector_test.go`, `TestCollectSemanticDedup`,
  fake store + stub bridge): 7 messages -> 5 rows ([m10+m11],
  [m12], [m13], [m14+m15], [m16]); merged=2, similar notes=4,
  renders=3 (m13, m14+m15 group render, m16; the two Zoom-link rows
  get none); envelope v4; JSON sources/notes verified; Excel Grup
  lists both labels, Pengirim both senders, Konteks carries the
  merge/time notes; second run 0 new / 7 skipped-seen.
- Real-binary smoke run (`--once` against a synthetic store in
  /tmp, bridge unreachable/degraded): scanned=7 new=5 merged=2
  similar=4 renders=3; JSON + Excel re-checked independently with
  python (openpyxl): 5 rows, merged flags, sources, notes, Konteks
  all as expected; 3 render PNGs in media/.
- Privacy scan (`grep -rE '[0-9]{10,}'` over the new/changed files,
  excluding the known fictional 555-range / all-zero placeholders):
  clean. All fixtures fictional.

## Note: the /tmp Python e2e script is gone

`/tmp/e2e-wa-collector-test.py` (37 checks at v0.4.0) no longer
exists: /tmp was wiped by a VM replacement. It was ephemeral by
design, so it is not recreated here; the Go end-to-end tests
(`TestCollectEndToEnd`, `TestCollectSemanticDedup`, upload/drive/s3
tests) are the durable coverage and were extended instead. If the
Python script is wanted back, it should live under
`~/workspace/wa-evidence-collector/` (e.g. `scripts/`), not /tmp.

## Updated stub list

- **Semantic dedup** — DONE (this release).
- **Agenda extraction** — no date/time parsing or calendar integration.
- **MCP server** — included unmodified; no collector integration.

## Honest limits (also in README)

- Grouping only covers messages new in ONE run: a reworded copy
  arriving in a later window cannot match already-exported rows
  (state holds hashes, never text), so it exports as its own row.
- Two genuinely different events with near-identical wording at the
  same time and date can merge; thresholds are tunable in `dedup:`.
- Dotted dates ("11.11.2026") are not recognized as dates; times are
  read as written (WIB assumed). No stemming or abbreviation
  expansion ("DT" vs "daerah tertinggal" are different tokens).

## Exact next steps

1. Pair the bridge once, `cp config.yaml.example config.yaml`, smoke
   run `--once` (unchanged from v0.1.0). The upload smoke run still
   needs real Drive/S3 credentials to close the v0.4.0 "untested
   against the real service" gap.
2. Natural next feature: agenda extraction (date/time parsing from
   message text into agenda candidates) — the last item in "What is
   not done yet".
3. When ready to publish: `git remote add origin <url>` + push.

# Build log — v0.6.0: agenda extraction

Date: 2026-11-11. Go 1.27.3. No new dependencies (pure Go, stdlib only).

## What was added / changed

- `collector/agenda.go` (new, ~700 lines) — `AgendaCandidate`
  (title, date `YYYY-MM-DD`, start `HH:MM`, end `*string` null when
  unknown, `EndAssumed`, `DurationMin *int`, location, link, status,
  missing fields, note, `RowMessageID`, sources), `AgendaSource`,
  `AgendaFile` envelope (version 1), `AgendaFileName`,
  `WriteAgendaFile`. Parser `parseAgendaText(text, ref)` in 9 steps:
  links first (meeting-platform link preferred: Zoom/Meet/Teams/
  Webex, else first link), then "jam/pukul" times (incl. pagi/siang/
  sore/malam qualifiers: "jam 7 malam" = 19:00, "jam 2 siang" =
  14:00, "jam 12 malam" = 00:00), then dotted dates (validated
  day/month/year; a match followed by ".<digit>" is a version number,
  e.g. "Versi 2.7.1"), then bare times (dotted-date guard), then
  numeric dates (day-first, 2-digit year -> 2000+yy, 4-digit year
  must be 2000-2099), then month-name dates (own regex WITH captured
  year — dedup's `dateMonthRe` does not capture the year), then
  relative dates (only when no explicit date: "hari ini", "besok",
  "lusa", "kemarin", weekday + "ini" = this week Mon-Sun, weekday +
  "depan" = next week, bare weekday = next occurrence on/after the
  message date, "tanggal N" = ref month; bare "minggu depan" /
  "pekan depan" = Monday of next week; no year/month rollover — a
  date landing in the past is reported as written). Range detection: first two times
  joined by a range connector ("-", "–", "—", "s.d.", "s/d", "sd",
  "sampai", "hingga") = start/end; two unconnected times -> first is
  start, end unknown. "s.d. selesai" with one time -> end = start +
  `UntilFinishedMinutes`, flagged `EndAssumed` + note. Single time
  preceded by "s.d."/"sampai" is an END time ("Rapat s.d. jam
  12:00") -> start unknown. End == start -> dropped (typo); end <
  start -> +24h (overnight, noted). Location priority: "tempat:" /
  "lokasi:" > "via <platform>" > "di <Place>" (must start uppercase
  or a venue word; exclusions: sini, atas, bawah, tanggal, hari, jam,
  grup, channel, link, tempat, lokasi...) > bare venue phrase >
  meeting-link platform. Title: first non-empty line after blanking
  consumed spans (dates, times, links, "s.d. selesai", range
  connector, "tempat:" marker, "via <platform>") and stripping
  leading greeting/kind words (incl. "selamat pagi", "undangan",
  "ralat"), then first sentence (cut at "!" / "?" or ". " +
  uppercase). Title/location are redacted on export when redact=true;
  the link is exported RAW (it is the join link).
  `extractAgenda(group, row, cfg, redact)`: canonical message parsed
  first; missing date/time filled from other members only when they
  agree on a single value (disagreement noted, field left missing);
  returns nil when no member carries a date or a time.
- `collector/config.go` — `AgendaConfig` (enabled default true,
  `UntilFinishedMinutes` default 120, `Timezone` "WIB" /
  "Asia/Jakarta" only) + `Validate` + defaults + wired into
  `Config.Validate`.
- `collector/collector.go` — `Summary` gains `Agenda` (candidates)
  and `AgendaComplete` (status "lengkap"); `Collect` extracts
  candidates per group after rows are built, writes
  `Agenda-<YYYYMMDD>.json` (even with zero candidates — empty list
  proves extraction ran) + adds the "Agenda" sheet when >= 1
  candidate, both before the manifest so the manifest covers them;
  agenda file added to `sum.Files` (so upload to dokumen/ includes
  it).
- `collector/excel.go` — style creation refactored into shared
  `recapStyles` + `styleSheet` (used by both sheets); new Agenda
  sheet ("Agenda", 11 columns: Judul | Tanggal | Jam Mulai | Jam
  Selesai | Durasi (menit) | Lokasi | Link | Status | Kurang |
  Sumber | Catatan; `CandidateToAgendaRow`, `agendaSources` renders
  "g-<hash> (m10, m11), dm-<hash> (m12)"), `AddAgendaSheet` (opens
  daily xlsx, replaces existing Agenda sheet, keeps recap sheet
  active, verifies by reopening), `ReadAgendaSheet`,
  `verifyAgendaSheet`. Monthly combined file does NOT carry an
  Agenda sheet.
- `cmd/wa-evidence-collector/main.go` — version const bumped
  `v0.1.0` -> `v0.6.0`; `printSummary` now prints
  `agenda=N complete=N`.
- `config.yaml.example` — `agenda:` section added (annotated,
  enabled true, 120 min, WIB).
- `collector/agenda_test.go` (new) — `TestParseAgendaFormats` (~40
  table cases: month-name + range + Zoom, abbrev month, slash date,
  dotted dates with/without year, 2-digit year, besok/lusa/hari ini/
  kemarin, Senin depan/Jumat ini/bare weekday/weekday minggu depan,
  tanggal N, s.d. selesai, s.d. range, dotted range, jam 7 malam/2
  siang/12 malam, unconnected times, end-only, overnight range, di/
  tempat:/lokasi: venues, Meet link, generic link, date-only,
  time-only, no date/time, "Versi 2.7.1", invalid date 31 Februari,
  "minggu depan" alone); `TestAgendaTitle` (6 cases incl.
  greeting-line skip, "Rabu, 11 November 2026 jam 09:00 via Zoom" ->
  ""); `TestExtractAgendaGroup` (merged cross-post pair -> 1
  candidate/2 sources, 09:00-12:00, 180 min; correction group fills
  date from other member; s.d. selesai -> end 15:00 assumed, 120
  min, note "asumsi 2 jam"; date-only -> missing ["jam"]; time-only
  -> missing ["tanggal"]; greeting -> nil; phone number in title
  redacted, link NOT redacted); `TestAgendaConfigValidation`;
  `TestCandidateToAgendaRow`.
- `collector/collector_test.go` — `TestCollectAgenda` (new synthetic
  e2e, `seedAgendaStore`): 9 messages -> 8 rows (a1+a2 merged), 7
  candidates, 5 complete; checks every candidate field (title, date,
  start, end, duration, location, link, status, missing), the merged
  candidate's 2 sources, the "besok" note, the 2h-assumption note,
  no candidate from the greeting, no phone-number leak, the Agenda
  sheet (header + 7 rows, incl. both sources in row 0), the manifest
  listing the Agenda JSON, and a second run over the same window
  (0 new, 9 skipped, 0 agenda).
- `collector/upload_test.go` — `TestCollectWithFakeUpload` upload
  count 8 -> 9 (the Agenda JSON is a 6th export file).

## Design decisions (documented in code + README)

- Extraction runs on dedup GROUPS, not raw messages: the same event
  in several messages is one candidate with all sources. A
  correction member can fill a date/time the canonical message
  lacks, but only when the other members agree on a single value.
- "Never guess" is literal: a missing date or time is exported as
  "belum lengkap" with the missing field named. The only assumed
  value is the 2-hour "s.d. selesai" duration (the user's standing
  rule), and it is flagged `EndAssumed` + noted on the candidate.
- Relative dates resolve against the message timestamp, never today,
  and only when the message also carries a time or an event word:
  the unit tests caught "semangat hari ini!" producing a junk date,
  which is what forced the event-word gate. Bare "minggu depan" is
  "next week" (Monday), not "next Sunday": the first test run
  showed the weekday regex reading it as Sunday + "depan".
- The link is exported raw while title/location are redacted: the
  link is the join link, the title/location are user text.
- The Agenda sheet lives only in the daily file (one candidate list
  per run day); the monthly combined file stays recap-only.
- No calendar events are created. Candidates are the output;
  calendar integration is a documented next step.

## Bugs found by the tests (fixed in this release)

- `TestParseAgendaFormats` caught two real bugs on the first run:
  "hari ini" in a greeting matched as a date, and "minggu depan"
  resolved to next Sunday. Both fixed (event-word gate for relative
  dates; bare "minggu depan" -> Monday of next week) and covered by
  new table cases.
- `TestCollectWithFakeUpload` caught the Agenda JSON as a 6th export
  file (upload count 8 -> 9).
- Pre-existing nit, NOT fixed: `pairphone/main.go` (untouched
  upstream code from the v0.1.0 scaffold) is not gofmt-clean.

## Verification

- `go build ./...` — OK. `go vet ./...` — OK. `gofmt -l` clean
  except pre-existing `pairphone/main.go`.
- `go test ./...` — all packages pass (collector 50 tests incl. the
  new agenda unit + e2e tests; scheduler cached). No new
  dependencies.

## Updated stub list

- **Agenda extraction** — DONE (this release).
- **MCP server** — included unmodified; no collector integration.

## Honest limits (also in README)

- One candidate per row: a message listing two separate sessions
  loses the second (first date/time pair wins).
- Grouping covers only messages new in one run: a reworded copy in
  a later window cannot match an already-exported row.
- No announcement detection: a casual time mention with a date
  produces an incomplete candidate.
- Year = message year, no rollover; a date landing in the past is
  reported as written.
- Dotted "11.11" is read as a date (DD.MM), not a time, unless
  prefixed by "jam"/"pukul".
- End <= start rolls over midnight (overnight, noted).
- "Minggu ini" is read as this Sunday, "minggu depan" as Monday of
  next week — both are ambiguous in Indonesian and the choice is
  documented, not hidden.
- No stemming/abbreviation expansion ("DT" vs "daerah tertinggal"
  are different tokens).

## Exact next steps

1. Pair the bridge once, `cp config.yaml.example config.yaml`, smoke
   run `--once` (unchanged from v0.1.0). The upload smoke run still
   needs real Drive/S3 credentials to close the v0.4.0 "untested
   against the real service" gap.
2. Natural next step: calendar integration (create events from
   agenda candidates after the user selects them) — or publish the
   repo first (`git remote add origin <url>` + push).
3. When ready to publish: `git remote add origin <url>` + push.
