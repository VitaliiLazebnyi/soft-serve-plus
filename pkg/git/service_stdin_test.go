package git

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stallingReader is a client that sends bytes git rejects, so git exits,
// and then sends nothing more without going away: every later read blocks
// until it is closed, or a read deadline is set on it when it takes one.
type stallingReader struct {
	sent    atomic.Bool
	reading atomic.Bool
	stopped atomic.Bool
	once    sync.Once
	stop    chan struct{}
}

func newStallingReader() *stallingReader {
	return &stallingReader{stop: make(chan struct{})}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if !r.sent.Swap(true) {
		return copy(p, "zzzz"), nil
	}
	r.reading.Store(true)
	defer r.reading.Store(false)
	<-r.stop
	return 0, os.ErrDeadlineExceeded
}

// close ends a blocked read.
func (r *stallingReader) close() {
	r.once.Do(func() { close(r.stop) })
}

// deadlineStallingReader is a stallingReader whose reads a read deadline
// ends, as a network connection's or an HTTP request body's.
type deadlineStallingReader struct {
	*stallingReader
}

func (r deadlineStallingReader) SetReadDeadline(t time.Time) error {
	if !t.After(time.Now()) {
		r.stopped.Store(true)
		r.close()
	}
	return nil
}

// When git exits before the client is done, the handler must not return
// while the copy still reads the client's input: an input that takes a
// read deadline gets one that ends the read, and the copy is waited for.
func TestGitServiceJoinsStdinCopy(t *testing.T) {
	r := newStallingReader()
	t.Cleanup(r.close)

	dir := initBare(t)
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPack(t.Context(), ServiceCommand{
			Stdin: deadlineStallingReader{r},
			Dir:   dir,
		})
	}()
	if err := waitErr(t, errc); err == nil {
		t.Fatal("upload-pack accepted garbage input")
	}

	if !r.stopped.Load() {
		t.Error("the read of the client's input was not ended by a read deadline")
	}
	if r.reading.Load() {
		t.Error("the handler returned while the client's input was still being read")
	}
}

// An input that takes no read deadline, such as an SSH channel, cannot be
// interrupted; the handler must not wait for its read, which only ends once
// the input's owner closes it after the handler returns.
func TestGitServiceLeavesUninterruptibleStdin(t *testing.T) {
	r := newStallingReader()
	t.Cleanup(r.close)

	dir := initBare(t)
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPack(t.Context(), ServiceCommand{
			Stdin: r,
			Dir:   dir,
		})
	}()
	if err := waitErr(t, errc); err == nil {
		t.Fatal("upload-pack accepted garbage input")
	}
}
