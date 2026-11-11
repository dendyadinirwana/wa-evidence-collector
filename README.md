# wa-evidence-collector

Collect WhatsApp group messages as evidence, on a built-in schedule.
One repo, one binary: a WhatsApp bridge (forked from
[verygoodplugins/whatsapp-mcp](https://github.com/verygoodplugins/whatsapp-mcp))
plus a collector that reads the bridge store and exports allowlisted chats
to JSON + CSV + Excel, with media downloads and a SHA256 manifest per run.

> **Status: v0.6.0.** The bridge, pairing, collector, scheduler,
> exporters (JSON, CSV, Excel), chat renders, remote upload
> (Google Drive + S3-compatible), semantic dedup, and agenda
> extraction work (see [Agenda extraction](#agenda-extraction)).

> **Unofficial API.** This connects as a WhatsApp *Linked Device* via the
> WhatsApp Web multidevice protocol (whatsmeow). It is not a Meta API.
> Use it on an account you own, keep the collection read-only, and expect
> that WhatsApp may change the protocol, rate-limit, or disconnect the
> session at any time.

## Architecture

```
wa-evidence-collector
├── bridge/                 # Go bridge (whatsmeow) — forked, patches ported
│   └── store/              # created at runtime, gitignored: *.db, media, token
├── whatsapp-mcp-server/    # Python MCP server — upstream, unmodified
├── pairphone/              # code-pairing helper (8-char code, no QR scan)
├── collector/              # NEW: store reader, dedup, redaction, exporters
├── scheduler/              # NEW: built-in cron (robfig/cron/v3)
├── cmd/wa-evidence-collector/  # main binary: bridge + scheduler
└── config.yaml             # your config (copy from config.yaml.example)
```

Data flow:

```
phone ──WhatsApp Web──> bridge ──> store/messages.db
                                        │
                                        ▼ (read-only)
                              collector ──> evidence/YYYYMMDD/
                                        │     evidence-HHMMSS.json
                                        │     evidence-HHMMSS.csv
                                        │     Rekap-Evidence-YYYYMMDD.xlsx
                                        │     Agenda-YYYYMMDD.json
                                        │     media/...
                                        │     manifest.sha256
                                        │
                                        └─> evidence/Rekap-Evidence-YYYYMM-gabungan.xlsx
                                            (monthly, rebuilt from the daily files)
                                        │
                                        └─ media via bridge POST /api/download
                                        │
                                        └─> remote upload (optional, off by
                                            default): Drive or S3, same
                                            per-date layout, links back into
                                            the Link Drive column
```

The collector never writes to the bridge store and never sends messages.
The MCP server is included for compatibility (AI clients that speak MCP);
the collector does not depend on it.

## Quick start

Requirements: Go 1.27+, Python 3.11+ (only for the MCP server), `uv`
(optional, for the MCP server).

```bash
# 1. Build everything.
go build ./...                       # collector + scheduler + cmd
go build -o bridge/whatsapp-bridge ./bridge
go build -o pairphone/pairphone ./pairphone

# 2. Configure (placeholders only in the example).
cp config.yaml.example config.yaml
$EDITOR config.yaml                 # set your allowlist JIDs

# 3. Pair the bridge (first run only). Pick one:
#    a. QR code: run the bridge, scan store/qr.png with your phone
#       (WhatsApp > Settings > Linked Devices > Link a Device).
./bridge/whatsapp-bridge
#    b. Pairing code (no QR scan): stop the bridge first, then
./pairphone/pairphone <phone-international-digits> ./bridge/store/whatsapp.db
#    Enter the printed 8-character code on your phone
#    (Linked Devices > Link a Device > Link with phone number instead),
#    then restart the bridge.

# 4. Run.
./wa-evidence-collector --config config.yaml          # scheduled mode
./wa-evidence-collector --config config.yaml --once   # one run, then exit
```

The bridge stays alive while unpaired; it writes a fresh QR to
`bridge/store/qr.png` (+ the raw code to `bridge/store/qr.txt`, mode 0600)
on every pairing attempt. QR codes rotate fast (first 60s, then 20s each),
so prefer the pairing code when QR delivery is slow.

## Configuration

See `config.yaml.example` (annotated). Summary:

| Section | Key | Meaning |
|---|---|---|
| `bridge` | `api_url` | Bridge REST base, default `http://127.0.0.1:8080/api` |
| `bridge` | `token_file` | Bearer token file (`store/.bridge-token`), created by the bridge |
| `bridge` | `db_path` | SQLite store the collector reads (read-only) |
| `bridge` | `binary` | Bridge binary path (autostart only) |
| `bridge` | `autostart` | Start the bridge as a child process (default true) |
| `bridge` | `health_timeout_seconds` | Wait for `/api/health` 200; timeout = degraded mode, not fatal |
| `collection` | `output_dir` | Evidence root; runs go to `output_dir/YYYYMMDD/` |
| `collection` | `allowlist` | Chat JIDs to collect (`...@g.us`, `...@s.whatsapp.net`, `...@lid`) |
| `collection` | `lookback_hours` | Default window for `--once` |
| `collection` | `redact` | Mask senders + phone-like numbers (default true) |
| `media` | `download` | Download media via the bridge (default true) |
| `media` | `max_attempts` | Attempts per message (default 3) |
| `media` | `retry_delay_seconds` | Wait between attempts (default 30) |
| `dedup` | `enabled` | Semantic dedup pass (default true) |
| `dedup` | `threshold` | Token overlap (Jaccard) to merge (default 0.55) |
| `dedup` | `note_threshold` | Overlap to add a "mirip dengan" note (default 0.35) |
| `dedup` | `correction_threshold` | Overlap for a correction to merge (default 0.30) |
| `dedup` | `time_window_hours` | Max message-timestamp gap to consider (default 48) |
| `dedup` | `correction_markers` | Words marking a correction ("ralat", "koreksi", ...) |
| `agenda` | `enabled` | Parse agenda candidates from messages (default true) |
| `agenda` | `until_finished_minutes` | Duration assumed for "s.d. selesai" with no end (default 120) |
| `agenda` | `timezone` | `"WIB"` or `"Asia/Jakarta"` only (default WIB) |
| `upload` | `enabled` | Upload after a run (default false, local-only) |
| `upload` | `backend` | `"drive"` or `"s3"` |
| `upload` | `max_attempts` / `retry_delay_seconds` | Attempts per file (default 3) / wait (default 10s) |
| `upload` | `strict` | Any upload failure fails the run (default false) |
| `upload.drive` | `credentials_file` / `folder_id` / `folder_path` | Service-account or OAuth JSON; remote root by folder ID or path |
| `upload.s3` | `endpoint` / `region` / `bucket` / `prefix` | S3-compatible endpoint, bucket, key prefix |
| `upload.s3` | `access_key` / `secret_key` | Fallback credentials (prefer env vars) |
| `upload.s3` | `public_url_base` / `use_path_style` | Base for returned URLs / path-style addressing (default true) |
| `schedules` | `spec` / `label` / `window_hours` | Cron spec + window per run |

Cron specs are 5-field (`minute hour day-of-month month day-of-week`) or
descriptors (`@daily`, `@weekly`, `@every 1h`). A slow run is skipped,
never overlapped.

## What a run produces

`evidence/20261111/` for a run on 11 Nov 2026:

```
evidence/20261111/
├── evidence-063000.json     # rows + run metadata
├── evidence-063000.csv      # same rows, CSV
├── Rekap-Evidence-20261111.xlsx  # same rows, 11-column recap + Agenda sheet
├── Agenda-20261111.json     # agenda candidates parsed from the rows
├── media/
│   └── g-a1b2c3d4_image_20261111_060500_3AXXXX.jpg
└── manifest.sha256          # sha256 of every file above (plus the monthly file)
state.json                    # dedup state (hashes only, at evidence/)
```

`evidence/Rekap-Evidence-202611-gabungan.xlsx` is the monthly combined
file: header once, data rows from every daily file of that month, rebuilt
from the daily files on every run. The daily files hold only the rows
that were new in their run, so re-running the same window overwrites
that day's file with the (possibly empty) re-run rows, and the next
monthly rebuild reflects the latest daily files. Daily re-runs are the
exception, not the norm (each window is scheduled once).

The Excel recap has 11 columns: Tanggal | Jam Pesan (WIB) | Grup |
Pengirim | Jenis Kegiatan | Isi Pesan | Jenis Evidence | Nama File |
Link Drive | Status Unduhan | Konteks. Grup is the hashed chat label
(`g-<hash>` / `dm-<hash>`), Pengirim is the redacted sender id, dates
and times are WIB. Jenis Kegiatan / Jenis Evidence are coarse labels
derived from the media type and status (not content classification);
Link Drive holds the remote URL of the row's evidence file after a
successful upload (empty while exports stay local or when the upload
failed). Formatting: dark-blue
header + white bold text, frozen top row, autofilter, wrapped text,
thin borders, landscape fit-to-width. All cells are plain text, never
formulas. Every file is reopened programmatically after writing to
prove it opens error-free.

A second run over the same window exports only new messages; already-seen
messages are counted as `skipped-seen`. Media that fails after all retries
is recorded as `failed` in the export instead of blocking the run.

### Chat renders

Every unique row that has no document, media, or link gets a PNG
render of the verbatim message text in the style of WhatsApp Web
(`HHMM_<chat-label>_chat.png` in the run's `media/` folder): a
centered date pill, grey incoming-style bubbles with a tail and a
read-check badge, the sender name in green, *bold* and _italic_
WhatsApp markup honoured, verbatim text including typos, a
per-message timestamp, and an avatar circle with the sender's
initial. The render is a **RENDER drawn from text, never a
screenshot** of the app; the Excel row records it as Jenis Evidence
`Screenshot chat (render)`, Nama File the PNG name, Status Unduhan
`dibuat dari teks pesan`. Rows that already have a file or link get
no render. Rendering is Go-native (`golang.org/x/image`, Noto Sans
from the system when present, embedded Go fonts as fallback), so the
binary has no Python/Pillow dependency. Renders use the redacted
sender and text, so they never leak a real name or number. A message
longer than 12000 runes is truncated with a `[...]` marker (the full
text stays in JSON/CSV/Excel).

### Agenda extraction

After dedup, each merged row is parsed into at most one agenda
candidate. The same event mentioned in several messages (a reworded
cross-post, or a correction) is ONE candidate with every source listed,
because extraction runs on the dedup groups, not on raw messages.
Candidates are exported per run to `Agenda-<YYYYMMDD>.json` (written
even when empty, so an empty file proves extraction ran) and as an
"Agenda" sheet in the daily Excel with the same candidates. The
monthly combined file does not carry an Agenda sheet. The Agenda JSON
is uploaded to `dokumen/` like the other exports when upload is on.
**No calendar events are created**: producing candidates is the
scope; calendar integration is a documented next step, because the
user's workflow requires selecting candidates first.

A candidate has: title, date (`YYYY-MM-DD`), start (`HH:MM`), end
(`HH:MM` or null + reason), duration in minutes (or null), location,
link, source message IDs, group(s), status (`lengkap` / `belum
lengkap` + which fields are missing), and a note naming where an
assumed value came from.

**Rules.** A candidate MUST have a date and a time. If either is
missing it is still exported, marked `belum lengkap` with the missing
field recorded — a date or time is never guessed. "Sampai selesai" /
"s.d. selesai" with no end time means the end is start + 2 hours
(`agenda.until_finished_minutes`, default 120), flagged as an
assumption in the note. A single start time with no end and no
"sampai selesai" leaves the end unknown (null). A message with
neither a date nor a time produces no candidate at all.

**Dates.** Month names full and abbreviated ("11 November 2026",
"11 Nov 2026", "11 November", "11 Nov"), numeric ("11/11/2026",
"11-11-26", "11.11.2026" — dotted dates, which dedup does not
recognise, are handled here), weekday names, and relative dates
("hari ini", "besok", "lusa", "kemarin", "Senin depan", "Jumat ini",
"tanggal 12", bare weekday = next occurrence on/after the message
date). Relative dates resolve against the MESSAGE's own timestamp,
never against today, and only count when the message also carries a
time or an event word ("rapat", "webinar", "upacara", ...): without
either, "semangat hari ini!" is a greeting, not an agenda item.
Bare "minggu depan" / "pekan depan" means "next week" and resolves
to Monday of next week (the day within the week is unknown; the note
says so). A date with no year takes the message's year, with no
rollover: a date landing in the past is reported as written.

**Times.** "07:00", "07.00", "jam 7", "pukul 07.00 WIB", ranges
"09:00-12:00", "09:00 s.d. 12:00", "09.00-12.00 WIB", "09:00 s.d.
selesai", and "jam 7 malam" / "jam 2 siang" / "jam 12 malam"
(19:00 / 14:00 / 00:00). Times are WIB. Two times joined by a range
connector (-, –, —, "s.d.", "s/d", "sd", "sampai", "hingga") are
start/end; two unconnected times mean start only, end unknown; a
time preceded by "s.d." / "sampai" alone is an END time (start
unknown). An end equal to the start is dropped as a typo; an end
earlier than the start rolls over midnight (noted). A version number
("Versi 2.7.1") is never read as a date.

**Location and title.** Location is taken from, in priority order:
"tempat:" / "lokasi:", "via <platform>", "di <Place>", a bare venue
phrase ("Ruang Rapat Utama", "Aula Gedung A", "Lapangan Parkir"),
or the meeting platform of a Zoom/Meet/Teams/Webex link in the
message. A meeting link is captured as location + link; the link is
exported RAW (it is the join link) while title and location are
redacted like the rest of the export. The title is the first line /
first sentence, with date, time, link, and location fragments
blanked out and leading greetings and "undangan" dropped.

**Honest limits.** One candidate per row: a message listing two
separate sessions loses the second. Grouping covers only messages
new in one run, so a reworded copy arriving in a later window cannot
match an already-exported row. There is no announcement detection:
a casual mention of a time ("jam 09:00") with a date produces an
incomplete candidate. Dotted "11.11" is read as a date (DD.MM), not
a time, unless prefixed by "jam"/"pukul". "Minggu ini" is read as
this Sunday, "minggu depan" as Monday of next week: both are
ambiguous in Indonesian and the choice is documented here, not
hidden. No stemming or abbreviation expansion ("DT" and "daerah
tertinggal" are different tokens to the overlap check).

### Remote upload (Google Drive / S3-compatible)

Off by default (local-only). Enable it in `config.yaml` (`upload:
enabled: true`, `backend: "drive"` or `"s3"`). After a run's rows are
built, each row's evidence file (downloaded media or chat-render PNG)
is uploaded first, so its remote URL lands in the row's Link Drive
column in the final JSON/CSV/Excel; the export files themselves
(JSON, CSV, XLSX, manifest, monthly recap) are uploaded afterwards
and their URLs are logged and recorded in the run summary.

Remote layout mirrors the local one under the configured root/prefix:

```
<root>/<YYYYMMDD>/media/<evidence files>    (downloaded media + chat renders)
<root>/<YYYYMMDD>/dokumen/<export files>     (evidence-*.json/.csv, recap xlsx, manifest)
<root>/Rekap-Evidence-<YYYYMM>-gabungan.xlsx (monthly recap, at the root)
```

- **Drive** (`google.golang.org/api/drive/v3`, official client). Auth
  is a credentials file: a service-account JSON or an OAuth
  authorized-user JSON at `upload.drive.credentials_file` (never
  commit it; `credentials/` is gitignored). The remote root is a
  folder by ID (`folder_id`) or by path (`folder_path`, resolved and
  created segment by segment from the Drive root). Folders for the
  date, `media/`, and `dokumen/` are created as needed. The row link
  is the file's `webViewLink`; uploaded files are NOT shared — the
  link opens for the credentials' owner, share it from Drive if
  others need access.
- **S3** (`aws-sdk-go-v2`). Works with AWS S3, MinIO, and Cloudflare
  R2 (configurable endpoint). Auth comes from the environment by
  default (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`); config-file
  keys are a fallback and must never be committed. The returned URL
  is `public_url_base + "/" + key` when set, otherwise
  `endpoint/bucket/key` (path style). For a private bucket that URL
  is only a pattern: it needs signing or a public-read setup to open.
  The backend never changes bucket policy or object ACLs.

**Idempotency:** before uploading, the backend checks whether a file
with the same remote path already exists with the same size; if so it
is skipped and the existing URL is returned. This compares name +
size only, NOT a content hash: a different file with the same name
and size would be mistaken for the same file. File names embed
message IDs and timestamps, so that is unlikely in practice.

**Retries and failures:** each file is attempted up to
`upload.max_attempts` times (default 3) with
`upload.retry_delay_seconds` (default 10s) between attempts. A
failure is logged and counted, the row's link stays empty, and the
local file is kept; the run continues. With `upload.strict: true`
any upload failure fails the whole run instead.

### Semantic dedup

A second pass after exact dedup (message ID + content hash). Messages
that are the same event reworded or cross-posted export as **one row
with all sources listed**, instead of two rows. Pure Go, offline, no
ML model: the text is normalized (lowercase, markup/punctuation
stripped, generic words dropped), then messages are compared on
token-set overlap (Jaccard) plus shared key entities (links incl.
Zoom meeting IDs, explicit dates/times, media keys) plus time
proximity. Sender is never used: cross-posts usually come from
different senders.

Rules, in order:

1. Message timestamps further apart than `dedup.time_window_hours`
   (default 48h): never merge, never note.
2. **Correction:** the later message carries a correction marker
   (`ralat`, `koreksi`, `revisi`, `perubahan`, `update`, `perbaikan`,
   `diperbaiki`) and is similar enough (overlap >=
   `correction_threshold`, or a shared link/media/date): it merges
   into the same row, flagged as a correction, and its text becomes
   the row's text (a correction supersedes). A correction may change
   the date or time, so this is checked before the conflict rules.
3. **Date conflict:** both carry explicit (different) dates: never
   merge; a "tanggal berbeda" note is added when the overlap is high
   enough.
4. **Time conflict:** both carry explicit event times differing by
   more than 15 minutes: never merge; a "jam berbeda" note is added
   when the pair is otherwise similar.
5. **Shared Zoom meeting ID:** merge (one meeting).
6. **Shared media key or shared link**, with supporting overlap
   (overlap >= `note_threshold`, or a shared date/time): merge.
7. **Overlap at note level, corroborated** by a shared explicit date
   or time: merge.
8. **Overlap >= `threshold`:** merge.
9. **Overlap >= `note_threshold`:** keep both rows, add a
   "mirip dengan ..." note on both.

A merged row keeps the earliest timestamp as its time, the longest /
most complete message as its text (or the latest correction), every
source message in the JSON `sources` list (chat, sender, timestamp,
text, evidence file), `merged: true` in JSON, and a note in the Excel
Konteks column ("digabung dari 2 sumber, 2 grup", plus "(ralat)" for a
correction). The Excel Grup column lists every source chat label and
Pengirim every source sender. A merged text-only row gets a single
chat render holding all its messages. The CSV stays flat (13 columns);
the full merge detail lives in JSON and Excel.

Grouping runs on the raw message text in memory only: the similarity
signatures are never written to disk, and the state file still holds
content hashes only.

**Honest limits.** Grouping only covers messages that are new in one
run: a reworded copy arriving in a later window cannot be matched
against already-exported rows (the state file holds hashes, never
text), so it exports as its own row. Two genuinely different events
with near-identical wording at the same time and date can merge; the
thresholds are tunable in `dedup:` when that happens. Dates written
with dots ("11.11.2026") are not recognized as dates, and times are
read as written (WIB assumed). No stemming or abbreviation expansion:
"DT" and "daerah tertinggal" are different tokens, so a reword that
only swaps those relies on the other signals (shared link/time/date)
to merge.

## Privacy model

- **Read-only by default.** The collector only reads the local store and
  calls the bridge's read endpoints (`/api/health`, `/api/download`). It
  has no send path at all.
- **Allowlist-only.** Only chats listed in `collection.allowlist` are
  read. Everything else in the store is ignored.
- **Local-only.** All processing happens on your machine. There is no
  telemetry, no analytics, no outbound call except to WhatsApp itself
  (via the bridge) and, if you configure one, to your own storage
  (your Drive folder or your S3 bucket).
- **Redaction (default on).** Senders become `sender:<8 hex of sha256>`,
  JIDs and phone-like numbers in text become `[redacted-jid]` /
  `[redacted-number]`. File names use hashed chat labels (`g-<hash>`),
  never real JIDs. Chat JIDs are kept inside exports as the evidence
  source identifier; turn `redact` off only if you need raw identifiers.
- **Secrets stay out of the repo.** `store/`, `*.db`, tokens, `qr.png`,
  `qr.txt`, `config.yaml`, `credentials/`, `*-service-account.json`,
  `*-credentials.json`, and `evidence/` are gitignored. Only
  `config.yaml.example` (placeholders) is committed.
- **State file holds hashes only** (`state.json` maps
  `chat_jid|message_id` to a content SHA256), never message text.
- **Excel recap is redacted too.** The Grup column uses the hashed chat
  label and Pengirim the redacted sender id, exactly like JSON/CSV.

## Local changes vs upstream

Ported from the operator's deployment into `bridge/` and `pairphone/`
(the MCP server is untouched):

1. **Media download fallback** (`bridge/main.go: downloadViaOriginalURL`).
   If whatsmeow's `Download` fails, the bridge fetches the stored signed
   URL with the default HTTP client and decrypts the `.enc` payload with
   the stored media key (HKDF-SHA256 to iv/cipherKey/macKey, HMAC-SHA256
   10-byte MAC check, AES-256-CBC + PKCS7 unpad, SHA256 checks),
   mirroring whatsmeow's scheme. Needed where the refreshed media-host
   path returns 403 while the original signed URL still works.
2. **Pairing QR to files** (`bridge/main.go`: `connection.renderQR`).
   Every QR code event also writes `store/qr.png` (rsc.io/qr) and
   `store/qr.txt`, so the code can be delivered as an image when stdout
   is not a terminal. Upstream only renders ASCII on a TTY.
3. **Pairing by code** (`pairphone/`, own Go module). Generates an
   8-character pairing code via whatsmeow `PairPhone` using the bridge's
   store. Stop the bridge first (SQLite lock), run it, enter the code on
   the phone. The login window lasts ~160s.

## What is not done yet

Honest stub list for v0.6.0 — these are planned, not implemented:

- **MCP server changes.** `whatsapp-mcp-server/` is upstream as-is.

Calendar integration (creating events from agenda candidates) is a
documented next step, not a stub: candidates are exported for the
user to select first.

## Development

```bash
export PATH=$HOME/workspace/tools/go/bin:$PATH   # if go is not on PATH
go build ./...            # root module (collector, scheduler, cmd)
go vet ./...              # same
go test ./...             # collector + scheduler unit tests
cd bridge && go build ./... && go vet ./...     # bridge module
cd ../pairphone && go build ./...               # pairphone module
```

Bridge and pairphone are separate Go modules (as upstream); the root
module covers `collector/`, `scheduler/`, and `cmd/`.

## License

MIT. See `LICENSE`. This project incorporates code from
[verygoodplugins/whatsapp-mcp](https://github.com/verygoodplugins/whatsapp-mcp)
and [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp),
both MIT.
