package common

import (
	"strings"
	"unicode/utf8"
)

// SanitizeAuditText makes upstream-supplied text safe to store: NUL bytes are
// removed (PostgreSQL rejects them in text columns) and invalid UTF-8 is
// replaced with U+FFFD (PostgreSQL rejects it with SQLSTATE 22021).
func SanitizeAuditText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return s
}

// TruncateAuditBytes returns a sanitized prefix of s no longer than maxBytes,
// cut back to a rune boundary, and whether anything was dropped. It sanitizes
// FIRST and cuts afterwards: replacing an invalid byte with U+FFFD adds two
// bytes, so cutting first could push the result past the cap (MySQL TEXT
// rejects anything over 65535 bytes). A plain s[:n] could also split a
// multi-byte rune and make the row unstorable.
func TruncateAuditBytes(s string, maxBytes int) (string, bool) {
	s = SanitizeAuditText(s)
	if len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// TruncateAuditBytesTail is TruncateAuditBytes for the END of s: the last
// maxBytes of the sanitized text, advanced to a rune boundary.
func TruncateAuditBytesTail(s string, maxBytes int) string {
	s = SanitizeAuditText(s)
	if len(s) > maxBytes {
		start := len(s) - maxBytes
		for start < len(s) && !utf8.RuneStart(s[start]) {
			start++
		}
		s = s[start:]
	}
	return s
}

// TruncateAuditRunes returns sanitized s limited to maxRunes characters, the
// unit varchar(n) is measured in on PostgreSQL and MySQL.
func TruncateAuditRunes(s string, maxRunes int) string {
	s = SanitizeAuditText(s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i]
		}
		n++
	}
	return s
}
