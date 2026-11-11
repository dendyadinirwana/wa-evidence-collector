package collector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Message is one row from the bridge messages table.
type Message struct {
	ID        string
	ChatJID   string
	ChatName  string
	Sender    string
	Content   string
	Timestamp time.Time
	IsFromMe  bool
	MediaType string
	Filename  string
	URL       string
	FileLen   int64
}

// ContentHash identifies the message content for dedup: chat + id + timestamp
// + text. Two rows with the same hash are the same evidence.
func (m Message) ContentHash() string {
	sum := sha256.Sum256([]byte(m.ChatJID + "\n" + m.ID + "\n" + m.Timestamp.UTC().Format(time.RFC3339) + "\n" + m.Content))
	return hex.EncodeToString(sum[:])
}

// StateKey is the dedup key for the state file.
func (m Message) StateKey() string { return m.ChatJID + "|" + m.ID }

// OpenStore opens the bridge SQLite store read-only. It never creates the
// file and never writes to it.
func OpenStore(dbPath string) (*sql.DB, error) {
	// mode=ro: fail if the file is missing instead of creating an empty DB.
	// _pragma=query_only(1) is a second guard against accidental writes.
	dsn := "file:" + dbPath + "?mode=ro&_pragma=query_only(1)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", dbPath, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store %s: %w", dbPath, err)
	}
	return db, nil
}

// FetchMessages returns messages from allowlisted chats inside [since, until).
// Deleted messages (deleted_at set) are skipped. Results are ordered oldest
// first. Timestamps in the store are TEXT in UTC ("2006-01-02 15:04:05+00:00"),
// so string comparison matches chronological order.
func FetchMessages(ctx context.Context, db *sql.DB, allowlist []string, since, until time.Time) ([]Message, error) {
	if len(allowlist) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(allowlist))
	placeholders = strings.TrimSuffix(placeholders, ",")
	query := `
		SELECT m.id, m.chat_jid, COALESCE(c.name, ''), m.sender, m.content,
		       m.timestamp, m.is_from_me, COALESCE(m.media_type, ''),
		       COALESCE(m.filename, ''), COALESCE(m.url, ''),
		       COALESCE(m.file_length, 0)
		FROM messages m
		LEFT JOIN chats c ON c.jid = m.chat_jid
		WHERE m.chat_jid IN (` + placeholders + `)
		  AND m.timestamp >= ?
		  AND m.timestamp < ?
		  AND m.deleted_at IS NULL
		ORDER BY m.timestamp ASC`
	args := make([]interface{}, 0, len(allowlist)+2)
	for _, jid := range allowlist {
		args = append(args, jid)
	}
	args = append(args, formatStoreTime(since), formatStoreTime(until))

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var ts string
		var isFromMe bool
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.ChatName, &m.Sender, &m.Content,
			&ts, &isFromMe, &m.MediaType, &m.Filename, &m.URL, &m.FileLen); err != nil {
			return nil, fmt.Errorf("scan message row: %w", err)
		}
		m.IsFromMe = isFromMe
		parsed, err := time.Parse("2006-01-02 15:04:05-07:00", ts)
		if err != nil {
			// Fall back to RFC3339; skip the row if it parses neither way.
			parsed, err = time.Parse(time.RFC3339, ts)
			if err != nil {
				continue
			}
		}
		m.Timestamp = parsed.UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read message rows: %w", err)
	}
	return out, nil
}

// formatStoreTime renders a time the way the store keeps timestamps.
func formatStoreTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05+00:00")
}
