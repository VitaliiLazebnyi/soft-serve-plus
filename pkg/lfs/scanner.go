package lfs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	gitm "github.com/aymanbagabas/git-module"
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
// revArgs and stdin.
func searchPointerBlobs(ctx context.Context, repo *git.Repository, revArgs []string, stdin string, pointerChan chan<- PointerBlob, errChan chan<- error) {
	basePath := repo.Path

	catFileCheckReader, catFileCheckWriter := io.Pipe()
	shasToBatchReader, shasToBatchWriter := io.Pipe()
	catFileBatchReader, catFileBatchWriter := io.Pipe()

	wg := sync.WaitGroup{}
	wg.Add(6)

	// Create the go-routines in reverse order.

	// 4. Take the output of cat-file --batch and check if each file in turn
	// to see if they're pointers to files in the LFS store
	go createPointerResultsFromCatFileBatch(ctx, catFileBatchReader, &wg, pointerChan, errChan)

	// 3. Take the shas of the blobs and batch read them
	go catFileBatch(ctx, shasToBatchReader, catFileBatchWriter, &wg, basePath, errChan)

	// 2. From the provided objects restrict to blobs of at most
	// blobSizeCutoff bytes
	go smallBlobsFromCatFileBatchCheck(catFileCheckReader, shasToBatchWriter, &wg)

	// 1. Run batch-check on all objects in the repository
	revListReader, revListWriter := io.Pipe()
	shasToCheckReader, shasToCheckWriter := io.Pipe()
	go catFileBatchCheck(ctx, shasToCheckReader, catFileCheckWriter, &wg, basePath, errChan)
	go blobsFromRevListObjects(revListReader, shasToCheckWriter, &wg)
	go revListObjects(ctx, revListWriter, &wg, basePath, revArgs, stdin, errChan)
	wg.Wait()

	// A canceled scan stops early, possibly after git rev-list finished
	// cleanly, so report it rather than let it pass for a complete one.
	// Don't block if a stage already reported an error.
	if err := ctx.Err(); err != nil {
		sendScanError(ctx, errChan, err)
	}

	close(pointerChan)
	close(errChan)
}

// sendScanError reports a failed stage of the scan. A stage that fails
// breaks the pipes around it, so the stages next to it may fail as well; the
// first error is the one that counts and later ones are dropped rather than
// block. Each stage reports before it closes its pipes with the error, so
// the stage that failed first gets in ahead of the ones it broke. A stage
// that fails once ctx is done failed because the scan was canceled, with an
// error that doesn't say so (git's commands report a timeout when they are
// killed), so ctx's error is reported instead.
func sendScanError(ctx context.Context, errChan chan<- error, err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	select {
	case errChan <- err:
	default:
	}
}

func createPointerResultsFromCatFileBatch(ctx context.Context, catFileBatchReader *io.PipeReader, wg *sync.WaitGroup, pointerChan chan<- PointerBlob, errChan chan<- error) {
	defer wg.Done()
	defer catFileBatchReader.Close() //nolint: errcheck

	bufferedReader := bufio.NewReader(catFileBatchReader)
	// Room for the largest blob passed on, plus its trailing newline.
	buf := make([]byte, blobSizeCutoff+1)

	fail := func(err error) {
		err = fmt.Errorf("reading git cat-file --batch output: %w", err)
		sendScanError(ctx, errChan, err)
		_ = catFileBatchReader.CloseWithError(err)
	}

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		default:
		}

		// File descriptor line: sha
		sha, err := bufferedReader.ReadString(' ')
		if err != nil {
			// A clean end of the output between two blobs is the end of
			// the scan.
			if !errors.Is(err, io.EOF) || sha != "" {
				fail(err)
			}
			break
		}
		sha = strings.TrimSpace(sha)
		// Throw away the blob
		if _, err := bufferedReader.ReadString(' '); err != nil {
			fail(err)
			break
		}
		sizeStr, err := bufferedReader.ReadString('\n')
		if err != nil {
			fail(err)
			break
		}
		size, err := strconv.Atoi(sizeStr[:len(sizeStr)-1])
		if err != nil {
			fail(err)
			break
		}
		pointerBuf := buf[:size+1]
		if _, err := io.ReadFull(bufferedReader, pointerBuf); err != nil {
			fail(err)
			break
		}
		pointerBuf = pointerBuf[:size]
		// Now we need to check if the pointerBuf is an LFS pointer
		pointer, _ := ReadPointerFromBuffer(pointerBuf)
		if !pointer.IsValid() {
			continue
		}

		select {
		case pointerChan <- PointerBlob{Hash: sha, Pointer: pointer}:
		case <-ctx.Done():
			break loop
		}
	}
}

func catFileBatch(ctx context.Context, shasToBatchReader *io.PipeReader, catFileBatchWriter *io.PipeWriter, wg *sync.WaitGroup, basePath string, errChan chan<- error) {
	defer wg.Done()
	defer shasToBatchReader.Close()  //nolint: errcheck
	defer catFileBatchWriter.Close() //nolint: errcheck

	stderr := new(bytes.Buffer)
	if err := gitm.NewCommandWithContext(ctx, "cat-file", "--batch").
		WithTimeout(-1).
		RunInDirWithOptions(basePath, gitm.RunInDirOptions{
			Stdout: catFileBatchWriter,
			Stdin:  shasToBatchReader,
			Stderr: stderr,
		}); err != nil {
		err = fmt.Errorf("git cat-file --batch [%s]: %w - %s", basePath, err, stderr.String())
		sendScanError(ctx, errChan, err)
		_ = shasToBatchReader.CloseWithError(err)
	}
}

// smallBlobsFromCatFileBatchCheck passes on the blobs of at most
// blobSizeCutoff bytes, the only ones that can be LFS pointers.
func smallBlobsFromCatFileBatchCheck(catFileCheckReader *io.PipeReader, shasToBatchWriter *io.PipeWriter, wg *sync.WaitGroup) {
	defer wg.Done()
	defer catFileCheckReader.Close() //nolint: errcheck
	scanner := bufio.NewScanner(catFileCheckReader)
	defer func() {
		_ = shasToBatchWriter.CloseWithError(scanner.Err())
	}()
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) < 3 || fields[1] != "blob" {
			continue
		}
		size, _ := strconv.Atoi(fields[2])
		if size > blobSizeCutoff {
			continue
		}
		toWrite := []byte(fields[0] + "\n")
		for len(toWrite) > 0 {
			n, err := shasToBatchWriter.Write(toWrite)
			if err != nil {
				_ = catFileCheckReader.CloseWithError(err)
				break
			}
			toWrite = toWrite[n:]
		}
	}
}

func catFileBatchCheck(ctx context.Context, shasToCheckReader *io.PipeReader, catFileCheckWriter *io.PipeWriter, wg *sync.WaitGroup, basePath string, errChan chan<- error) {
	defer wg.Done()
	defer shasToCheckReader.Close()  //nolint: errcheck
	defer catFileCheckWriter.Close() //nolint: errcheck

	stderr := new(bytes.Buffer)
	if err := gitm.NewCommandWithContext(ctx, "cat-file", "--batch-check").
		WithTimeout(-1).
		RunInDirWithOptions(basePath, gitm.RunInDirOptions{
			Stdout: catFileCheckWriter,
			Stdin:  shasToCheckReader,
			Stderr: stderr,
		}); err != nil {
		err = fmt.Errorf("git cat-file --batch-check [%s]: %w - %s", basePath, err, stderr.String())
		sendScanError(ctx, errChan, err)
		_ = shasToCheckReader.CloseWithError(err)
	}
}

func blobsFromRevListObjects(revListReader *io.PipeReader, shasToCheckWriter *io.PipeWriter, wg *sync.WaitGroup) {
	defer wg.Done()
	defer revListReader.Close() //nolint: errcheck
	scanner := bufio.NewScanner(revListReader)
	defer func() {
		_ = shasToCheckWriter.CloseWithError(scanner.Err())
	}()

	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) < 2 || len(fields[1]) == 0 {
			continue
		}
		toWrite := []byte(fields[0] + "\n")
		for len(toWrite) > 0 {
			n, err := shasToCheckWriter.Write(toWrite)
			if err != nil {
				_ = revListReader.CloseWithError(err)
				break
			}
			toWrite = toWrite[n:]
		}
	}
}

func revListObjects(ctx context.Context, revListWriter *io.PipeWriter, wg *sync.WaitGroup, basePath string, revArgs []string, stdin string, errChan chan<- error) {
	defer wg.Done()
	defer revListWriter.Close() //nolint: errcheck

	stderr := new(bytes.Buffer)
	if err := gitm.NewCommandWithContext(ctx, append([]string{"rev-list", "--objects"}, revArgs...)...).
		WithTimeout(-1).
		RunInDirWithOptions(basePath, gitm.RunInDirOptions{
			Stdin:  strings.NewReader(stdin),
			Stdout: revListWriter,
			Stderr: stderr,
		}); err != nil {
		sendScanError(ctx, errChan, fmt.Errorf("git rev-list [%s]: %w - %s", basePath, err, stderr.String()))
	}
}
