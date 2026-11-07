package git

import (
	"bytes"
	"fmt"
	"io"
	"strconv"

	"github.com/charmbracelet/soft-serve/pkg/lfs"
)

// errLFSCommandTooLarge is returned by lfsCommandLimitReader once a command
// goes over lfs.MaxBatchRequestSize.
var errLFSCommandTooLarge = fmt.Errorf("lfs: command larger than %d bytes", lfs.MaxBatchRequestSize)

// lfsCommandState is where an lfsCommandLimitReader is in the command stream.
type lfsCommandState int

const (
	lfsCommandStart   lfsCommandState = iota // next packet starts a command
	lfsCommandArgs                           // in a command, up to its flush
	lfsCommandPutArgs                        // in put-object's arguments, up to the delimiter
	lfsCommandPutData                        // in put-object's object data, up to the flush
	lfsCommandRaw                            // framing broken, passing bytes through
)

// lfsCommandLimitReader bounds the bytes of each command a git-lfs-transfer
// client sends. The transfer processor reads every packet of a command up to
// its flush into memory, so without a bound a client can stream a batch, or
// any command's arguments, until the server runs out of memory. Object data
// sent with put-object is streamed to storage and is not counted.
//
// It reads the stream one packet at a time and passes the bytes through
// unchanged. Once a command goes over the limit, that packet and every later
// read fail with errLFSCommandTooLarge, which ends the session: the stream
// can no longer be read from a command boundary.
type lfsCommandLimitReader struct {
	r     io.Reader
	limit int
	n     int // bytes of the current command read so far
	state lfsCommandState
	buf   []byte // the rest of the packet being passed through
	err   error  // returned once buf is drained
}

func newLFSCommandLimitReader(r io.Reader, limit int) *lfsCommandLimitReader {
	return &lfsCommandLimitReader{r: r, limit: limit}
}

// Read implements io.Reader.
func (l *lfsCommandLimitReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if l.state == lfsCommandRaw && len(l.buf) == 0 && l.err == nil {
		return l.r.Read(p)
	}
	if len(l.buf) == 0 && l.err == nil {
		l.next()
	}
	if len(l.buf) > 0 {
		n := copy(p, l.buf)
		l.buf = l.buf[n:]
		return n, nil
	}
	return 0, l.err
}

// next reads the next packet into buf, or sets err.
func (l *lfsCommandLimitReader) next() {
	hdr := make([]byte, 4)
	if n, err := io.ReadFull(l.r, hdr); err != nil {
		// Pass a partial header on; the pktline reader reports it.
		l.buf, l.err = hdr[:n], err
		return
	}
	size, err := strconv.ParseUint(string(hdr), 16, 16)
	if err != nil || (size > 2 && size < 4) {
		// Not a packet the processor can read. Pass the bytes on and let
		// it fail the session.
		l.buf, l.state = hdr, lfsCommandRaw
		return
	}

	switch size {
	case 0: // flush
		l.buf, l.n, l.state = hdr, 0, lfsCommandStart
		return
	case 1: // delimiter
		if l.state == lfsCommandPutArgs {
			l.state = lfsCommandPutData
		}
	}

	pkt := make([]byte, max(int(size), len(hdr)))
	copy(pkt, hdr)
	if n, err := io.ReadFull(l.r, pkt[len(hdr):]); err != nil {
		l.buf, l.err = pkt[:len(hdr)+n], err
		return
	}

	switch l.state {
	case lfsCommandPutData:
		l.buf = pkt
		return
	case lfsCommandStart:
		l.state = lfsCommandArgs
		if bytes.HasPrefix(pkt[len(hdr):], []byte("put-object ")) {
			l.state = lfsCommandPutArgs
		}
	}

	l.n += len(pkt)
	if l.n > l.limit {
		l.err = errLFSCommandTooLarge
		return
	}
	l.buf = pkt
}

var _ io.Reader = (*lfsCommandLimitReader)(nil)
