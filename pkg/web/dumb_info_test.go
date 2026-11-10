package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// infoFixture is a router serving a public repository "pub", and the
// repository's directory.
type infoFixture struct {
	handler http.Handler
	dir     string
}

func newInfoFixture(t *testing.T) *infoFixture {
	t.Helper()
	is := is.New(t)
	ctx, be, _ := newLFSTestContext(t)
	_, err := be.CreateRepository(ctx, "pub", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	dir := filepath.Join(config.FromContext(ctx).DataPath, "repos", "pub.git")
	return &infoFixture{handler: NewRouter(ctx), dir: dir}
}

func (f *infoFixture) get(t *testing.T, path string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// populate pushes a commit, a lightweight tag, an annotated tag and a tag
// of that tag into the repository, half of it packed.
func (f *infoFixture) populate(t *testing.T) {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "init", "-q", "-b", "master")
	is.New(t).NoErr(os.WriteFile(filepath.Join(work, "a"), []byte("a"), 0o644))
	runGit(t, work, "add", "a")
	runGit(t, work, "commit", "-q", "-m", "first")
	runGit(t, work, "tag", "light")
	runGit(t, work, "tag", "-a", "-m", "annotated", "v1")
	runGit(t, work, "tag", "-a", "-m", "nested", "v1-nested", "v1")
	runGit(t, work, "push", "-q", f.dir, "master", "--tags")
	runGit(t, f.dir, "repack", "-q", "-a", "-d")
	is.New(t).NoErr(os.WriteFile(filepath.Join(work, "b"), []byte("b"), 0o644))
	runGit(t, work, "add", "b")
	runGit(t, work, "commit", "-q", "-m", "second")
	runGit(t, work, "push", "-q", f.dir, "master:refs/heads/other")
	runGit(t, f.dir, "repack", "-q", "-d")
}

// TestDumbInfoDoesNotWrite checks the dumb info/refs and objects/info/packs
// answer what git update-server-info would write, without writing it.
func TestDumbInfoDoesNotWrite(t *testing.T) {
	is := is.New(t)
	f := newInfoFixture(t)
	f.populate(t)

	infoRefs := filepath.Join(f.dir, "info", "refs")
	infoPacks := filepath.Join(f.dir, "objects", "info", "packs")
	is.NoErr(os.RemoveAll(infoRefs))
	is.NoErr(os.RemoveAll(infoPacks))

	refs := f.get(t, "/pub.git/info/refs")
	is.Equal(refs.Code, http.StatusOK)
	is.Equal(refs.Header().Get("Content-Type"), "text/plain; charset=utf-8")
	is.Equal(refs.Header().Get("Cache-Control"), "no-cache, max-age=0, must-revalidate")
	packs := f.get(t, "/pub.git/objects/info/packs")
	is.Equal(packs.Code, http.StatusOK)
	is.Equal(packs.Header().Get("Content-Type"), "text/plain; charset=utf-8")

	// Neither request wrote to the repository.
	_, err := os.Stat(infoRefs)
	is.True(os.IsNotExist(err))
	_, err = os.Stat(infoPacks)
	is.True(os.IsNotExist(err))

	// Both answers are what update-server-info writes.
	runGit(t, f.dir, "update-server-info")
	want, err := os.ReadFile(infoRefs)
	is.NoErr(err)
	is.Equal(refs.Body.String(), string(want))
	is.True(strings.Contains(refs.Body.String(), "\trefs/tags/v1-nested^{}\n"))
	want, err = os.ReadFile(infoPacks)
	is.NoErr(err)
	is.Equal(sortedLines(packs.Body.String()), sortedLines(string(want)))
	is.Equal(strings.Count(packs.Body.String(), "P pack-"), 2)
}

func sortedLines(s string) string {
	lines := strings.Split(s, "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestDumbInfoEmptyRepository checks a repository with no refs and no packs
// has an empty ref list and pack list rather than an error.
func TestDumbInfoEmptyRepository(t *testing.T) {
	is := is.New(t)
	f := newInfoFixture(t)

	refs := f.get(t, "/pub.git/info/refs")
	is.Equal(refs.Code, http.StatusOK)
	is.Equal(refs.Body.String(), "")

	packs := f.get(t, "/pub.git/objects/info/packs")
	is.Equal(packs.Code, http.StatusOK)
	is.Equal(packs.Body.String(), "\n")
}

// TestDumbInfoPacksSkipsUnfinishedPacks checks a pack without its index,
// one git is still writing, is not listed.
func TestDumbInfoPacksSkipsUnfinishedPacks(t *testing.T) {
	is := is.New(t)
	f := newInfoFixture(t)
	f.populate(t)

	name := "pack-" + strings.Repeat("0", 40) + ".pack"
	is.NoErr(os.WriteFile(filepath.Join(f.dir, "objects", "pack", name), []byte("PACK"), 0o644))

	packs := f.get(t, "/pub.git/objects/info/packs")
	is.Equal(packs.Code, http.StatusOK)
	is.True(!strings.Contains(packs.Body.String(), name))
	is.Equal(strings.Count(packs.Body.String(), "P pack-"), 2)
}

// TestSmartInfoRefs checks the streamed smart advertisement keeps its
// header, the service line before protocol v2, and git's output.
func TestSmartInfoRefs(t *testing.T) {
	is := is.New(t)
	f := newInfoFixture(t)
	f.populate(t)
	head := strings.Fields(runGit(t, f.dir, "rev-parse", "refs/heads/master"))[0]

	w := f.get(t, "/pub.git/info/refs?service=git-upload-pack")
	is.Equal(w.Code, http.StatusOK)
	is.Equal(w.Header().Get("Content-Type"), "application/x-git-upload-pack-advertisement")
	is.Equal(w.Header().Get("Cache-Control"), "no-cache, max-age=0, must-revalidate")
	body := w.Body.String()
	is.True(strings.HasPrefix(body, "001e# service=git-upload-pack\n0000"))
	is.True(strings.Contains(body, head+" refs/heads/master"))
	is.True(strings.HasSuffix(body, "0000"))

	w = f.get(t, "/pub.git/info/refs?service=git-upload-pack", "Git-Protocol", "version=2")
	is.Equal(w.Code, http.StatusOK)
	is.True(strings.HasPrefix(w.Body.String(), "000eversion 2\n"))
}

// TestAdvertisementStartsOnFirstWrite checks the header is held back until
// git writes, so a git that fails silently still gets an error status.
func TestAdvertisementStartsOnFirstWrite(t *testing.T) {
	is := is.New(t)
	var out bytes.Buffer
	starts := 0
	a := &advertisement{w: &out, start: func() { starts++; out.WriteString("hdr|") }}

	n, err := a.Write(nil)
	is.NoErr(err)
	is.Equal(n, 0)
	is.Equal(starts, 0)
	is.True(!a.started)

	_, err = a.Write([]byte("one|"))
	is.NoErr(err)
	_, err = a.Write([]byte("two"))
	is.NoErr(err)
	a.begin()
	is.Equal(starts, 1)
	is.Equal(out.String(), "hdr|one|two")
}

// duplexRecorder records whether the handler asked for full duplex.
type duplexRecorder struct {
	*httptest.ResponseRecorder
	fullDuplex bool
}

func (d *duplexRecorder) EnableFullDuplex() error {
	d.fullDuplex = true
	return nil
}

// TestServiceRPCEnablesFullDuplex checks the RPC handler lets git read the
// request while it writes the response.
func TestServiceRPCEnablesFullDuplex(t *testing.T) {
	is := is.New(t)
	f := newInfoFixture(t)
	f.populate(t)

	// A protocol v2 ls-refs request, which needs no negotiation.
	body := "0014command=ls-refs\n0000"
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/pub.git/git-upload-pack", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	w := &duplexRecorder{ResponseRecorder: httptest.NewRecorder()}
	f.handler.ServeHTTP(w, req)

	is.Equal(w.Code, http.StatusOK)
	is.True(w.fullDuplex)
	is.True(strings.Contains(w.Body.String(), "refs/heads/master"))
}

// TestDumbClone checks git can clone over dumb HTTP from what the server
// answers, with no info files in the repository.
func TestDumbClone(t *testing.T) {
	is := is.New(t)
	f := newInfoFixture(t)
	f.populate(t)
	is.NoErr(os.RemoveAll(filepath.Join(f.dir, "info", "refs")))
	is.NoErr(os.RemoveAll(filepath.Join(f.dir, "objects", "info", "packs")))

	srv := httptest.NewServer(f.handler)
	t.Cleanup(srv.Close)

	dst := filepath.Join(t.TempDir(), "clone.git")
	cmd := exec.CommandContext(t.Context(), "git", "clone", "--bare", srv.URL+"/pub", dst)
	cmd.Env = append(os.Environ(), "GIT_SMART_HTTP=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dumb clone: %v: %s", err, out)
	}
	is.Equal(runGit(t, dst, "rev-parse", "refs/heads/other", "refs/tags/v1-nested"),
		runGit(t, f.dir, "rev-parse", "refs/heads/other", "refs/tags/v1-nested"))
}
