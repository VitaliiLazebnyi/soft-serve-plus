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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
)

// TestSearchPointerBlobsCancel verifies that the scan stops once its context
// is canceled, even when nobody reads the remaining pointers, and reports
// that it was canceled.
func TestSearchPointerBlobsCancel(t *testing.T) {
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

	repo, err := git.Open(work)
	if err != nil {
		t.Fatal(err)
	}

	// A scan that runs to the end finds every pointer and reports no error,
	// so the error below comes from the cancellation.
	{
		pointerChan := make(chan PointerBlob)
		errChan := make(chan error, 1)
		go SearchPointerBlobs(context.Background(), repo, pointerChan, errChan)
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
		SearchPointerBlobs(ctx, repo, pointerChan, errChan)
		close(done)
	}()

	// Read one pointer, let the scanner block on the next one, then stop
	// reading and cancel.
	<-pointerChan
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
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
	// The scan's own git commands must not read the host's configuration.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
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

	repo, err := git.Open(work)
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
	repo, err := git.Open(work)
	if err != nil {
		t.Fatal(err)
	}

	scan := func(include, exclude []string) ([]string, error) {
		pointerChan := make(chan PointerBlob)
		errChan := make(chan error, 1)
		go SearchPointerBlobsBetween(context.Background(), repo, include, exclude, pointerChan, errChan)
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
