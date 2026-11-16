package lfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/safego"
)

// catFileObject renders one object of git cat-file --batch output.
func catFileObject(id, typ string, size int, content string) string {
	return fmt.Sprintf("%s %s %d\n%s\n", id, typ, size, content)
}

// A blob whose size git cat-file reports past the cutoff is skipped, not read
// into the cutoff-sized buffer, and the scan goes on to the next object. A
// pointer that comes after it is still found.
func TestParseCatFileBatchSkipsLargeBlob(t *testing.T) {
	p, err := GeneratePointer(strings.NewReader("after"))
	if err != nil {
		t.Fatal(err)
	}
	large := p.String() + strings.Repeat("x", 4*blobSizeCutoff)
	out := catFileObject(strings.Repeat("1", 40), "blob", len(large), large) +
		catFileObject(strings.Repeat("2", 40), "blob", len(p.String()), p.String())

	var found []PointerBlob
	err = parseCatFileBatch(strings.NewReader(out), func(pb PointerBlob) bool {
		found = append(found, pb)
		return true
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(found) != 1 || found[0].Hash != strings.Repeat("2", 40) || found[0].Oid != p.Oid {
		t.Errorf("found %+v, want only the pointer after the large blob", found)
	}
}

// A negative size is malformed output, an error rather than a panic.
func TestParseCatFileBatchNegativeSize(t *testing.T) {
	out := strings.Repeat("1", 40) + " blob -1\n\n"
	err := parseCatFileBatch(strings.NewReader(out), func(PointerBlob) bool { return true })
	if err == nil {
		t.Fatal("parse accepted a negative size")
	}
}

// A refs/replace/ ref does not change what the scan reads: git would
// otherwise report the replacement's size and content for the pointer's id.
// Here the replacement is larger than the cutoff, so a scan that honours it
// either drops the pointer or reads a blob it can't hold.
func TestSearchPointerBlobsIgnoresReplaceRefs(t *testing.T) {
	work := t.TempDir()
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = work
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	// The scan's own git commands must not read the host's configuration
	// either.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	p, err := GeneratePointer(strings.NewReader("replaced"))
	if err != nil {
		t.Fatal(err)
	}
	run("", "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "file"), []byte(p.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	run("", "add", ".")
	run("", "commit", "-q", "-m", "pointer")
	pointerBlob := run("", "rev-parse", "HEAD:file")
	large := run(p.String()+strings.Repeat("x", 4*blobSizeCutoff), "hash-object", "-w", "--stdin")
	run("", "replace", pointerBlob, large)

	repo, err := git.Open(work)
	if err != nil {
		t.Fatal(err)
	}
	pointers, err := searchPointers(repo)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(pointers) != 1 || pointers[0].Hash != pointerBlob || pointers[0].Oid != p.Oid {
		t.Errorf("found %+v, want the pointer in %s", pointers, pointerBlob)
	}
}

// A panic in the scan is logged and reported as the scan's error, and both
// channels are closed, rather than taking the process down.
func TestSearchPointerBlobsRecoversPanic(t *testing.T) {
	orig := readPointer
	readPointer = func([]byte) (Pointer, error) { panic("scanner boom") }
	t.Cleanup(func() { readPointer = orig })

	var logs bytes.Buffer
	ctx := log.WithContext(context.Background(), log.New(&logs))
	repo := newPointerRepo(t)
	pointerChan := make(chan PointerBlob)
	errChan := make(chan error, 1)
	go SearchPointerBlobs(ctx, repo, pointerChan, errChan)

	deadline := time.After(time.Minute)
	for open := true; open; {
		select {
		case _, open = <-pointerChan:
		case <-deadline:
			t.Fatal("the pointer channel was not closed")
		}
	}
	var err error
	select {
	case err = <-errChan:
	case <-deadline:
		t.Fatal("no error reported")
	}
	var perr *safego.PanicError
	if !errors.As(err, &perr) {
		t.Fatalf("error = %v, want a *safego.PanicError", err)
	}
	if !strings.Contains(logs.String(), "scanner boom") {
		t.Errorf("panic not logged: %q", logs.String())
	}
	if _, open := <-errChan; open {
		t.Error("the error channel was not closed")
	}
}
