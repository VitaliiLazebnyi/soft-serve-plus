package lfs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/safego"
)

// SearchPointerBlobs scans the whole repository for LFS pointer files. It
// sends at most one error on errChan, which must have room for it.
func SearchPointerBlobs(ctx context.Context, repo *git.Repository, pointerChan chan<- PointerBlob, errChan chan<- error) {
	searchPointerBlobs(ctx, repo, []string{"--all"}, "", pointerChan, errChan)
}

// SearchPointerBlobsBetween is SearchPointerBlobs for only the objects
// reachable from the objects in include but not from those in exclude, as
// git rev-list --objects <include...> --not <exclude...> lists them. Each
// one is an object id. With no include, nothing is scanned.
func SearchPointerBlobsBetween(ctx context.Context, repo *git.Repository, include, exclude []string, pointerChan chan<- PointerBlob, errChan chan<- error) {
	// The ids go on stdin, as a repository can have more refs than fit on
	// a command line.
	var revs strings.Builder
	for _, id := range include {
		revs.WriteString(id + "\n")
	}
	for _, id := range exclude {
		revs.WriteString("^" + id + "\n")
	}
	searchPointerBlobs(ctx, repo, []string{"--stdin"}, revs.String(), pointerChan, errChan)
}

// searchPointerBlobs scans the objects git rev-list --objects lists with
// revArgs and stdin, sends the pointers it finds on pointerChan, and closes
// pointerChan and then errChan, after sending the error of the scan, if any.
// A scan that stops because ctx is done reports ctx's error, even if git
// happened to finish cleanly, so it does not pass for a complete one. A scan
// that panics, as it reads what git prints, is logged and reports the panic,
// since it runs in a goroutine of its own that nothing else recovers.
func searchPointerBlobs(ctx context.Context, repo *git.Repository, revArgs []string, stdin string, pointerChan chan<- PointerBlob, errChan chan<- error) {
	var err error
	if perr := safego.Run(log.FromContext(ctx).WithPrefix("lfs"), func() {
		err = scanPointerBlobs(ctx, repo.Path, revArgs, stdin, func(p PointerBlob) bool {
			select {
			case pointerChan <- p:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}); perr != nil {
		err = perr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The git commands killed by the cancellation fail too, with
		// errors that don't say why.
		err = ctxErr
	}
	close(pointerChan)
	if err != nil {
		errChan <- err
	}
	close(errChan)
}

// scanPointerBlobs runs git rev-list, keeping only the blobs that are small
// enough to be pointers, into git cat-file --batch in dir, and calls found
// with each pointer, until found returns false.
func scanPointerBlobs(ctx context.Context, dir string, revArgs []string, stdin string, found func(PointerBlob) bool) error {
	// Stop both commands when the scan ends early, as when found or the
	// parser stops it: they would otherwise block on a full pipe.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// rev-list filters out what can't be a pointer itself, the objects
	// named on its command line or stdin included. Commits are listed
	// whatever the filter; cat-file's output says what each object is, and
	// the parser skips the ones that are not blobs. The filters need git
	// 2.32; imports and mirror syncs, the only scans, need 2.37.
	//
	// Both commands read objects as stored, not through refs/replace/: a
	// replace ref that appears between their lookups, which a user can push
	// during their own import, would otherwise make cat-file report another
	// object than rev-list filtered, of any size.
	revListArgs := append([]string{
		"--no-replace-objects", "rev-list", "--objects", "--no-object-names", "--filter-provided-objects",
		"--filter=object:type=blob", "--filter=blob:limit=" + strconv.Itoa(blobSizeCutoff+1),
	}, revArgs...)
	revListStderr := &cappedBuffer{limit: scanStderrLimit}
	catFileStderr := &cappedBuffer{limit: scanStderrLimit}
	revList := exec.CommandContext(ctx, "git", revListArgs...)
	revList.Dir = dir
	revList.Stdin = strings.NewReader(stdin)
	revList.Stderr = revListStderr
	revList.WaitDelay = scanWaitDelay

	catFile := exec.CommandContext(ctx, "git", "--no-replace-objects", "cat-file", "--batch")
	catFile.Dir = dir
	catFile.Stderr = catFileStderr
	catFile.WaitDelay = scanWaitDelay

	// The ids go from one command to the other through a pipe of their
	// own, and cat-file's output through another this process holds both
	// ends of, so every end is closed whatever fails to start.
	ids, idsWriter, err := os.Pipe()
	if err != nil {
		return err //nolint:wrapcheck
	}
	out, outWriter, err := os.Pipe()
	if err != nil {
		_ = ids.Close()
		_ = idsWriter.Close()
		return err //nolint:wrapcheck
	}
	defer out.Close() //nolint:errcheck
	revList.Stdout = idsWriter
	catFile.Stdin = ids
	catFile.Stdout = outWriter
	// The commands hold their own copies once started.
	closeChildEnds := func() {
		_ = ids.Close()
		_ = idsWriter.Close()
		_ = outWriter.Close()
	}

	if err := catFile.Start(); err != nil {
		closeChildEnds()
		return fmt.Errorf("git cat-file --batch [%s]: %w", dir, err)
	}
	if err := revList.Start(); err != nil {
		// cat-file sees the end of its input and exits.
		closeChildEnds()
		_ = catFile.Wait()
		return fmt.Errorf("git rev-list [%s]: %w", dir, err)
	}
	closeChildEnds()

	// Whatever happens to the parser, a panic included, both commands are
	// stopped and waited for, so neither is left running or unreaped.
	waited := false
	defer func() {
		if !waited {
			cancel()
			_ = catFile.Wait()
			_ = revList.Wait()
		}
	}()

	parseErr := parseCatFileBatch(out, found)
	if parseErr != nil {
		// Stopped early: cat-file may be blocked writing the rest.
		cancel()
	}
	catFileErr := catFile.Wait()
	revListErr := revList.Wait()
	waited = true

	switch {
	case parseErr != nil && !errors.Is(parseErr, errStopScan):
		return fmt.Errorf("reading git cat-file --batch output: %w", parseErr)
	case catFileErr != nil:
		// rev-list then fails on the broken pipe, so cat-file's error is
		// the one that says what went wrong.
		return fmt.Errorf("git cat-file --batch [%s]: %w - %s", dir, catFileErr, catFileStderr.String())
	case revListErr != nil:
		return fmt.Errorf("git rev-list [%s]: %w - %s", dir, revListErr, revListStderr.String())
	}
	return nil
}

// scanWaitDelay is how long a scan stopped early waits for its git commands'
// stderr once they are killed: a process they started (git.exe's launcher
// on Windows) can hold it open past them.
const scanWaitDelay = 5 * time.Second

// scanStderrLimit caps how much of a scan command's stderr is kept for its
// error.
const scanStderrLimit = 4 << 10

// cappedBuffer keeps the first limit bytes written to it and discards the
// rest, reporting every write as complete. The buffer is a named field, not
// embedded, so cappedBuffer has no ReadFrom for io.Copy to bypass the cap
// with.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

// Write implements io.Writer.
func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// String returns the bytes kept so far.
func (b *cappedBuffer) String() string {
	return b.buf.String()
}

// errStopScan is what parseCatFileBatch returns when found stops it.
var errStopScan = errors.New("scan stopped")

// parseCatFileBatch reads git cat-file --batch output from r and calls found
// with each blob that is an LFS pointer. It returns nil at the end of the
// output, and errStopScan once found returns false.
func parseCatFileBatch(r io.Reader, found func(PointerBlob) bool) error {
	br := bufio.NewReader(r)
	// Room for the largest blob read, plus its trailing newline.
	buf := make([]byte, blobSizeCutoff+1)
	for {
		// The header: "<id> <type> <size>\n", or "<id> missing\n".
		header, err := br.ReadString('\n')
		if err != nil {
			// A clean end of the output between two objects is the end
			// of the scan.
			if errors.Is(err, io.EOF) && header == "" {
				return nil
			}
			return err //nolint:wrapcheck
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			return fmt.Errorf("unexpected object header %q", strings.TrimSpace(header))
		}
		sha, typ := fields[0], fields[1]
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 {
			return fmt.Errorf("unexpected object header %q", strings.TrimSpace(header))
		}

		// Only a blob small enough to be a pointer is read. rev-list
		// filtered out larger ones, but the size that counts is the one
		// cat-file reports, which buf has to hold.
		if typ != "blob" || size > blobSizeCutoff {
			// The content and its trailing newline.
			if _, err := io.CopyN(io.Discard, br, int64(size)+1); err != nil {
				return err //nolint:wrapcheck
			}
			continue
		}

		content := buf[:size+1]
		if _, err := io.ReadFull(br, content); err != nil {
			return err //nolint:wrapcheck
		}
		pointer, _ := readPointer(content[:size])
		if !pointer.IsValid() {
			continue
		}
		if !found(PointerBlob{Hash: sha, Pointer: pointer}) {
			return errStopScan
		}
	}
}

// readPointer parses a blob as a pointer. Tests replace it.
var readPointer = ReadPointerFromBuffer
