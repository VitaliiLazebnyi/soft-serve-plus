package lfs

import (
	"encoding/json"
	"errors"
	"io"
)

const (
	// maxBatchResponseSize bounds the batch response a remote may send. Each
	// object takes a few hundred bytes, or a few KiB with long presigned
	// hrefs and headers, so this leaves ample room for the 100 objects Git
	// LFS sends per batch by default.
	maxBatchResponseSize = 10 << 20 // 10 MiB

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
