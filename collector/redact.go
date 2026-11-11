package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Redaction replaces identifying values with stable, non-reversible tokens so
// exported evidence can be shared without leaking phone numbers or names.
//
//   - A sender JID becomes "sender:<first 8 hex of sha256(jid)>". The same
//     sender always maps to the same token within and across runs, so
//     conversations stay readable, but the token cannot be reversed into a
//     phone number.
//   - JID-shaped strings inside message text become "[redacted-jid]".
//   - Long digit runs (8+ digits, optionally spaced/dashed, with an optional
//     leading +) become "[redacted-number]". Short numbers (years, counts,
//     times) are left alone.

var (
	jidInText    = regexp.MustCompile(`\d+@(s\.whatsapp\.net|g\.us|lid)`)
	phoneInText  = regexp.MustCompile(`\+?\d[\d\s\-()]{6,}\d`)
	whatsappLink = regexp.MustCompile(`https?://(wa\.me|api\.whatsapp\.com/send)\S*`)
)

// RedactSender returns the pseudonym for a sender JID. Non-JID senders
// (for example "me") are passed through unchanged.
func RedactSender(sender string) string {
	if sender == "" || sender == "me" {
		return sender
	}
	sum := sha256.Sum256([]byte(sender))
	return "sender:" + hex.EncodeToString(sum[:])[:8]
}

// RedactContent masks JIDs, wa.me links, and phone-like numbers in text.
func RedactContent(content string) string {
	out := jidInText.ReplaceAllString(content, "[redacted-jid]")
	out = whatsappLink.ReplaceAllString(out, "[redacted-link]")
	out = phoneInText.ReplaceAllString(out, "[redacted-number]")
	return out
}

// ChatShort returns a short, non-identifying label for a chat JID, used in
// file names so exported file names never contain a real JID. Group chats
// get "g-<hash>", 1:1 chats get "dm-<hash>", anything else gets "c-<hash>".
func ChatShort(chatJID string) string {
	sum := sha256.Sum256([]byte(chatJID))
	hash := hex.EncodeToString(sum[:])[:8]
	switch {
	case strings.HasSuffix(chatJID, "@g.us"):
		return "g-" + hash
	case strings.HasSuffix(chatJID, "@s.whatsapp.net"), strings.HasSuffix(chatJID, "@lid"):
		return "dm-" + hash
	default:
		return "c-" + hash
	}
}
