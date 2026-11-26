package lfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/safego"
)

// scanBlockedOnSend reports whether a scan waits to send a pointer nobody
// reads.
func scanBlockedOnSend() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, frames, _ := strings.Cut(g, "\n")
		blocked := strings.Contains(header, "[select") || strings.Contains(header, "[chan send")
		if blocked && strings.HasPrefix(frames, "github.com/charmbracelet/soft-serve/pkg/lfs.searchPointerBlobs.func") {
			return true
		}
	}
	return false
}

// TestSearchPointerBlobsCancel verifies that the scan stops once its context
// is canceled, even when nobody reads the remaining pointers, and reports
// that it was canceled.
func TestSearchPointerBlobsCancel(t *testing.T) {
	// Not parallel: scanBlockedOnSend looks at every goroutine, and would
	// also see other tests' scans.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	work := t.TempDir()
	for i := 0; i < 5; i++ {
		p, err := GeneratePointer(bytes.NewReader([]byte(fmt.Sprintf("object %d", i))))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("file%d", i)), []byte(p.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"commit", "-q", "-m", "add lfs pointers"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	repo, err := git.Open(t.Context(), work)
	if err != nil {
		t.Fatal(err)
	}

	// A scan that runs to the end finds every pointer and reports no error,
	// so the error below comes from the cancellation.
	{
		pointerChan := make(chan PointerBlob)
		errChan := make(chan error, 1)
		go SearchPointerBlobs(context.Background(), nil, repo, pointerChan, errChan)
		n := 0
		for range pointerChan {
			n++
		}
		if err := <-errChan; err != nil {
			t.Fatalf("uncanceled scan: %v", err)
		}
		if n != 5 {
			t.Fatalf("uncanceled scan found %d pointers, want 5", n)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pointerChan := make(chan PointerBlob)
	errChan := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		SearchPointerBlobs(ctx, nil, repo, pointerChan, errChan)
		close(done)
	}()

	// Read one pointer, let the scanner block on the next one, then stop
	// reading and cancel.
	<-pointerChan
	deadline := time.Now().Add(time.Minute)
	for !scanBlockedOnSend() {
		if time.Now().After(deadline) {
			t.Fatal("the scan did not block on sending the next pointer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("SearchPointerBlobs did not return after the context was canceled")
	}

	err, ok := <-errChan
	if !ok || err == nil {
		t.Fatal("SearchPointerBlobs did not report the canceled scan")
	}
	// The git commands killed by the cancellation fail too, but the scan
	// must report why it stopped rather than their errors.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// The scan reports each pointer blob of at most blobSizeCutoff bytes, with
// its blob id, and nothing else: not a larger blob that starts like one, not
// a blob that only has the first line of one, and not commits, trees or
// tags, however large.
func TestSearchPointerBlobsFindsOnlySmallPointers(t *testing.T) {
	t.Parallel()
	// The scan's own git commands must not read the host's configuration.
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
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	small, err := GeneratePointer(strings.NewReader("small"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := GeneratePointer(strings.NewReader("other"))
	if err != nil {
		t.Fatal(err)
	}

	run("", "init", "-q", "-b", "main")
	write("small", small.String())
	if err := os.Mkdir(filepath.Join(work, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	write("dir/other", other.String())
	write("not-a-pointer", "version https://git-lfs.github.com/spec/v1\n")
	// A blob past the cutoff that starts like a pointer.
	write("large", other.String()+strings.Repeat("x", 64<<10))
	run("", "add", ".")
	// A commit and a tag larger than the cutoff.
	run(strings.Repeat("long message\n", 200), "commit", "-q", "-F", "-")
	run(strings.Repeat("long tag\n", 200), "tag", "-a", "v1", "-F", "-")
	// A ref that names the large blob itself: rev-list lists the objects
	// it is given whatever its filter, unless told not to.
	run("", "tag", "large-blob", run("", "rev-parse", "HEAD:large"))

	// The parser would skip the large blob too, so the filter is checked
	// on rev-list's own output: a ref naming a blob lists it unless
	// --filter-provided-objects is given.
	largeBlob := run("", "rev-parse", "HEAD:large")
	if listed := run("", scanRevListArgs([]string{"--all"})...); strings.Contains(listed, largeBlob) {
		t.Errorf("the scan's rev-list lists the large blob %s a tag names", largeBlob)
	}

	repo, err := git.Open(t.Context(), work)
	if err != nil {
		t.Fatal(err)
	}
	pointers, err := searchPointers(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range pointers {
		got[p.Oid] = p.Hash
	}
	want := map[string]string{
		small.Oid: run("", "rev-parse", "HEAD:small"),
		other.Oid: run("", "rev-parse", "HEAD:dir/other"),
	}
	if len(pointers) != len(want) || !maps.Equal(got, want) {
		t.Errorf("found %+v, want %v", pointers, want)
	}
}

// TestSearchPointerBlobsBetween verifies that the scan only finds the
// pointers reachable from include but not from exclude.
func TestSearchPointerBlobsBetween(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	work := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	var pointers []Pointer
	var tips []string
	run("init", "-q", "-b", "main")
	for i := 0; i < 2; i++ {
		p, err := GeneratePointer(bytes.NewReader([]byte(fmt.Sprintf("object %d", i))))
		if err != nil {
			t.Fatal(err)
		}
		pointers = append(pointers, p)
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("file%d", i)), []byte(p.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-q", "-m", fmt.Sprintf("add pointer %d", i))
		tips = append(tips, run("rev-parse", "HEAD"))
	}
	repo, err := git.Open(t.Context(), work)
	if err != nil {
		t.Fatal(err)
	}

	scan := func(include, exclude []string) ([]string, error) {
		pointerChan := make(chan PointerBlob)
		errChan := make(chan error, 1)
		go SearchPointerBlobsBetween(context.Background(), nil, repo, include, exclude, pointerChan, errChan)
		var oids []string
		for p := range pointerChan {
			oids = append(oids, p.Oid)
		}
		return oids, <-errChan
	}

	for _, tc := range []struct {
		name             string
		include, exclude []string
		want             []string
	}{
		{"everything", tips[1:], nil, []string{pointers[0].Oid, pointers[1].Oid}},
		{"new commit", tips[1:], tips[:1], []string{pointers[1].Oid}},
		{"nothing new", tips[1:], tips[1:], nil},
		{"no include", nil, tips[:1], nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := scan(tc.include, tc.exclude)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("oids = %v, want %v", got, want)
			}
		})
	}

	// An object that isn't in the repository fails the scan.
	if _, err := scan(tips[1:], []string{strings.Repeat("1", 40)}); err == nil {
		t.Error("scan with a missing object succeeded")
	}
}

// newPointerRepo returns a repository with one commit holding an LFS
// pointer file.
func newPointerRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	pointer := "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\n" +
		"size 12345\n"
	if err := os.WriteFile(filepath.Join(dir, "file.bin"), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "file.bin"},
		{"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	repo, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// failGit puts a git first on PATH that fails, writing "injected failure"
// to stderr, when its arguments include arg, and runs the real git
// otherwise.
func failGit(t *testing.T, arg string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
	if [ "$a" = %q ]; then
		echo "injected failure" >&2
		exit 1
	fi
done
exec %q "$@"
`, arg, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func searchPointers(repo *git.Repository) ([]PointerBlob, error) {
	pointerChan := make(chan PointerBlob)
	errChan := make(chan error, 1)
	go SearchPointerBlobs(context.Background(), nil, repo, pointerChan, errChan)
	var pointers []PointerBlob
	for p := range pointerChan {
		pointers = append(pointers, p)
	}
	return pointers, <-errChan
}

func TestSearchPointerBlobs(t *testing.T) {
	t.Parallel()
	pointers, err := searchPointers(newPointerRepo(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pointers) != 1 || pointers[0].Size != 12345 {
		t.Errorf("pointers = %+v, want the one in file.bin", pointers)
	}
}

// Every git command of the scan must report its failure: a failing cat-file
// that ended the scan early with no error would pass for a complete scan
// that found fewer pointers.
func TestSearchPointerBlobsReportsGitFailures(t *testing.T) {
	// Not parallel: failGit sets PATH.
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as git")
	}
	for _, tc := range []struct{ arg, label string }{
		{"rev-list", "git rev-list"},
		{"--batch", "git cat-file --batch "},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			repo := newPointerRepo(t)
			failGit(t, tc.arg)
			_, err := searchPointers(repo)
			if err == nil {
				t.Fatal("scan reported no error")
			}
			if !strings.Contains(err.Error(), tc.label) {
				t.Errorf("error %q does not name %q", err, tc.label)
			}
			if !strings.Contains(err.Error(), "injected failure") {
				t.Errorf("error %q does not include git's stderr", err)
			}
		})
	}
}

// catFileObject renders one object of git cat-file --batch output.
func catFileObject(id, typ string, size int, content string) string {
	return fmt.Sprintf("%s %s %d\n%s\n", id, typ, size, content)
}

// A blob whose size git cat-file reports past the cutoff is skipped, not read
// into the cutoff-sized buffer, and the scan goes on to the next object. A
// pointer that comes after it is still found.
func TestParseCatFileBatchSkipsLargeBlob(t *testing.T) {
	t.Parallel()
	p, err := GeneratePointer(strings.NewReader("after"))
	if err != nil {
		t.Fatal(err)
	}
	large := p.String() + strings.Repeat("x", 4*blobSizeCutoff)
	out := catFileObject(strings.Repeat("1", 40), "blob", len(large), large) +
		catFileObject(strings.Repeat("2", 40), "blob", len(p.String()), p.String())

	var found []PointerBlob
	err = parseCatFileBatch(strings.NewReader(out), ReadPointerFromBuffer, func(pb PointerBlob) bool {
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

// A negative size, or one past any object git stores, is malformed output,
// an error rather than a panic or a skip that overflows.
func TestParseCatFileBatchBadSize(t *testing.T) {
	t.Parallel()
	for _, size := range []string{"-1", "9223372036854775807", "1125899906842625"} {
		for _, typ := range []string{"blob", "commit"} {
			out := strings.Repeat("1", 40) + " " + typ + " " + size + "\n\n"
			err := parseCatFileBatch(strings.NewReader(out), ReadPointerFromBuffer, func(PointerBlob) bool { return true })
			if err == nil {
				t.Errorf("parse accepted a %s of size %s", typ, size)
			}
		}
	}
}

// A refs/replace/ ref does not change what the scan reads: git would
// otherwise report the replacement's size and content for the pointer's id.
// Here the replacement is larger than the cutoff, so a scan that honours it
// either drops the pointer or reads a blob it can't hold.
func TestSearchPointerBlobsIgnoresReplaceRefs(t *testing.T) {
	t.Parallel()
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

	repo, err := git.Open(t.Context(), work)
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

// panickingReader is a pointerReader that panics.
func panickingReader([]byte) (Pointer, error) { panic("scanner boom") }

// A panic in the scan is logged and reported as the scan's error, and both
// channels are closed, rather than taking the process down.
func TestSearchPointerBlobsRecoversPanic(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	repo := newPointerRepo(t)
	pointerChan := make(chan PointerBlob)
	errChan := make(chan error, 1)
	go searchPointerBlobs(t.Context(), log.New(&logs), panickingReader, repo, []string{"--all"}, "", pointerChan, errChan)

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
