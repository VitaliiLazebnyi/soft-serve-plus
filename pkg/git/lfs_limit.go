package git

import (
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/charmbracelet/soft-serve/pkg/lfs"
)

// errLFSCommandTooLarge is returned by lfsCommandLimitReader once a command
// goes over lfs.MaxBatchRequestSize.
var errLFSCommandTooLarge = fmt.Errorf("lfs: command larger than %d bytes", lfs.MaxBatchRequestSize)

// errLFSInvalidPacket is returned by lfsCommandLimitReader for a packet
// header the pktline reader would refuse.
var errLFSInvalidPacket = errors.New("lfs: invalid packet length")

// lfsCommandLimitReader bounds the bytes of each command a git-lfs-transfer
// client sends. The transfer processor reads every packet of a command up to
// its flush into memory, so without a bound a client can stream a batch, or
// any command's arguments, until the server runs out of memory.
//
// Object data sent with put-object is streamed to storage and must not count,
// but where it starts and ends can't be told from the framing alone: the
// processor stops before the data when put-object's arguments or object ID
// are invalid, and then reads the data packets as the next command. So every
// packet counts unless the backend says it is reading object data, with
// streamData, which lfsTransfer.Upload does while it reads the object.
//
// It reads the stream one packet at a time and passes the bytes through
// unchanged. Each Read returns bytes of at most one packet, so a buffered
// reader on top of it reads a packet only once it needs it, and each packet
// is counted, or not, by what the processor is doing when it reads it. Once a
// command goes over the limit, that packet and every later read fail with
// errLFSCommandTooLarge, which ends the session: the stream can no longer be
// read from a command boundary. A header the pktline reader would refuse
// fails the same way, with errLFSInvalidPacket: the processor answers such a
// header inside a command with a 400 and goes on reading, so passing the
// bytes through unchecked would let a client turn the limit off.
type lfsCommandLimitReader struct {
	r     io.Reader
	limit int
	n     int    // bytes of the current command read so far
	data  bool   // the backend is reading object data
	buf   []byte // the rest of the packet being passed through
	err   error  // returned once buf is drained
}

func newLFSCommandLimitReader(r io.Reader, limit int) *lfsCommandLimitReader {
	return &lfsCommandLimitReader{r: r, limit: limit}
}

// streamData marks whether the packets read from now on are object data the
// backend is streaming to storage, which do not count toward the limit. It is
// a no-op on a nil reader.
func (l *lfsCommandLimitReader) streamData(on bool) {
	if l != nil {
		l.data = on
	}
}

// Read implements io.Reader.
func (l *lfsCommandLimitReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
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
	// Parse the length as the pktline reader does, so both agree on where
	// every packet ends: ParseInt accepts a sign, as in "+00e".
	size, err := strconv.ParseInt(string(hdr), 16, 0)
	if err != nil || size < 0 || size == 2 || size == 3 {
		l.err = errLFSInvalidPacket
		return
	}
	if size == 0 { // flush
		l.buf, l.n = hdr, 0
		return
	}

	pkt := make([]byte, max(int(size), len(hdr)))
	copy(pkt, hdr)
	if n, err := io.ReadFull(l.r, pkt[len(hdr):]); err != nil {
		l.buf, l.err = pkt[:len(hdr)+n], err
		return
	}

	if !l.data {
		l.n += len(pkt)
		if l.n > l.limit {
			l.err = errLFSCommandTooLarge
			return
		}
	}
	l.buf = pkt
}

var _ io.Reader = (*lfsCommandLimitReader)(nil)
