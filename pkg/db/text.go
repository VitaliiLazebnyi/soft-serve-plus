package db

import (
	"strings"
	"unicode/utf8"
)

// StorableText returns s as text every database accepts: Postgres refuses
// invalid UTF-8 and NUL bytes in TEXT columns, so invalid bytes are replaced
// with U+FFFD and NULs are dropped. Use it for text a remote or a receiver
// wrote, and for errors that may quote it.
func StorableText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, string(utf8.RuneError)), "\x00", "")
}
