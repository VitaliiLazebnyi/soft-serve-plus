package lfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxBatchResponseSize bounds the batch response a remote may send. Each
	// object takes a few hundred bytes, or a few KiB with long presigned
	// hrefs and headers, and the client asks for at most 20 objects at a
	// time, so this leaves ample room.
	maxBatchResponseSize = 1 << 20 // 1 MiB

	// maxRemoteText is how much of any text a remote sends, such as an
	// error message or a status line's reason, the client keeps: the rest
	// would only be logged, once per object and per sync.
	maxRemoteText = 256

	// maxErrorResponseSize bounds the JSON error body of a failed transfer
	// request, which only carries a message.
	maxErrorResponseSize = 1 << 20 // 1 MiB
)

// errResponseTooLarge is returned when a remote's JSON response is larger
// than its limit.
var errResponseTooLarge = errors.New("lfs: response body too large")

// decodeJSONLimit decodes the first JSON value in r into v, reading at most
// limit bytes of it. The limit applies to what the transport hands over,
// which is already decompressed, so it also stops a gzip bomb.
func decodeJSONLimit(r io.Reader, limit int64, v any) error {
	return json.NewDecoder(&maxBytesReader{r: r, n: limit}).Decode(v) //nolint:wrapcheck
}

// maxBytesReader reads up to n bytes from r and fails with
// errResponseTooLarge once r has more, unlike io.LimitReader, which ends
// with io.EOF and would let a truncated value look merely malformed.
type maxBytesReader struct {
	r   io.Reader
	n   int64
	err error
}

func (l *maxBytesReader) Read(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Read one byte past the limit to tell whether there is more.
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	if int64(n) <= l.n {
		l.n -= int64(n)
		l.err = err
		return n, err //nolint:wrapcheck
	}
	n = int(l.n)
	l.n = 0
	l.err = errResponseTooLarge
	return n, l.err
}

// remoteText returns s, text a remote sent, cut to maxRemoteText bytes, at a
// character boundary if one is near, with a mark if it was cut, and with
// every byte or character that is not printable escaped, so a remote cannot
// write terminal control sequences into the log or a client's session.
func remoteText(s string) string {
	suffix := ""
	if len(s) > maxRemoteText {
		cut := maxRemoteText
		// Back off at most to the start of the character the cut is in;
		// text that is not UTF-8 is cut where it is, and escaped below.
		for back := 0; back < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); back++ {
			cut--
		}
		if !utf8.RuneStart(s[cut]) {
			cut = maxRemoteText
		}
		s, suffix = s[:cut], "... (truncated)"
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case r == ' ' || unicode.IsGraphic(r):
			b.WriteRune(r)
		default:
			q := strconv.QuoteRuneToASCII(r)
			b.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return b.String() + suffix
}
