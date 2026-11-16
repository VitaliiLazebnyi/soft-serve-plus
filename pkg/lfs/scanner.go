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

	"github.com/charmbracelet/soft-serve/git"
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
// happened to finish cleanly, so it does not pass for a complete one.
func searchPointerBlobs(ctx context.Context, repo *git.Repository, revArgs []string, stdin string, pointerChan chan<- PointerBlob, errChan chan<- error) {
	err := scanPointerBlobs(ctx, repo.Path, revArgs, stdin, func(p PointerBlob) bool {
		select {
		case pointerChan <- p:
			return true
		case <-ctx.Done():
			return false
		}
	})
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
	// Stop both commands on return: early, when found or the parser stops
	// the scan, they would otherwise block on a full pipe.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// rev-list filters out what can't be a pointer itself. Commits are
	// listed whatever the filter; cat-file's output says what each object
	// is, and the parser skips the ones that are not blobs.
	revListArgs := append([]string{
		"rev-list", "--objects", "--no-object-names",
		"--filter=object:type=blob", "--filter=blob:limit=" + strconv.Itoa(blobSizeCutoff+1),
	}, revArgs...)
	var revListStderr, catFileStderr bytes.Buffer
	revList := exec.CommandContext(ctx, "git", revListArgs...)
	revList.Dir = dir
	revList.Stdin = strings.NewReader(stdin)
	revList.Stderr = &revListStderr

	catFile := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	catFile.Dir = dir
	catFile.Stderr = &catFileStderr

	// The ids go from one command to the other through a pipe of their
	// own, not through this process.
	ids, idsWriter, err := os.Pipe()
	if err != nil {
		return err //nolint:wrapcheck
	}
	revList.Stdout = idsWriter
	catFile.Stdin = ids
	out, err := catFile.StdoutPipe()
	if err != nil {
		_ = ids.Close()
		_ = idsWriter.Close()
		return err //nolint:wrapcheck
	}

	revListErr := revList.Start()
	_ = idsWriter.Close()
	if revListErr != nil {
		_ = ids.Close()
		return fmt.Errorf("git rev-list [%s]: %w", dir, revListErr)
	}
	catFileErr := catFile.Start()
	_ = ids.Close()
	if catFileErr != nil {
		cancel()
		_ = revList.Wait()
		return fmt.Errorf("git cat-file --batch [%s]: %w", dir, catFileErr)
	}

	parseErr := parseCatFileBatch(out, found)
	if parseErr != nil {
		cancel()
	}
	// Read what is left, so cat-file is not stuck writing it when the
	// parser stopped early, before waiting for it.
	_, _ = io.Copy(io.Discard, out)
	catFileErr = catFile.Wait()
	revListErr = revList.Wait()

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
		if err != nil {
			return fmt.Errorf("unexpected object header %q", strings.TrimSpace(header))
		}

		if typ != "blob" {
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
		pointer, _ := ReadPointerFromBuffer(content[:size])
		if !pointer.IsValid() {
			continue
		}
		if !found(PointerBlob{Hash: sha, Pointer: pointer}) {
			return errStopScan
		}
	}
}
