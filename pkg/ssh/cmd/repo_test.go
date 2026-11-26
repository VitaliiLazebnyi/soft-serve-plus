package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
)

// binaryContents is a file git and the blob command treat as binary.
var binaryContents = []byte("bin\x00ary")

// viewRepo creates the repository "view" with one commit holding README.md
// and the binary file data.bin, and returns an admin context and the
// commit's hash.
func viewRepo(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx, be := newAuthTestContext(t)
	ctx = withUser(t, ctx, be, "boss", true)
	_, err := be.CreateRepository(ctx, "view", proto.UserFromContext(ctx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	bare := filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "view.git")
	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "data.bin"), binaryContents, 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	git("push", "-q", bare, "main")
	// The bare repository's HEAD follows the server git's init.defaultBranch,
	// which may be master; point it at the pushed branch.
	git("--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return ctx, git("rev-parse", "HEAD")
}

func TestBlobRawPrintsExactBytes(t *testing.T) {
	t.Parallel()
	ctx, _ := viewRepo(t)

	stdout, _, err := runRepoOutput(t, ctx, "blob", "view", "data.bin", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(stdout), binaryContents) {
		t.Fatal("not true: bytes.Equal([]byte(stdout), binaryContents)")
	} // no newline added
}

func TestBlobMissingPathIsFileNotFound(t *testing.T) {
	t.Parallel()
	ctx, _ := viewRepo(t)

	for _, path := range []string{"nope.txt", "dir/nope.txt"} {
		_, _, err := runRepoOutput(t, ctx, "blob", "view", path)
		if !errors.Is(err, proto.ErrFileNotFound) {
			t.Fatal("not true: errors.Is(err, proto.ErrFileNotFound)")
		}
	}
}

func TestCommitPrintsHash(t *testing.T) {
	t.Parallel()
	ctx, sha := viewRepo(t)

	stdout, _, err := runRepoOutput(t, ctx, "commit", "view", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "commit "+sha+"\n") {
		t.Fatal("not true: strings.HasPrefix(stdout, \"commit \"+sha+\"\\n\")")
	}
}

// The repo commands run git under the session's context: git started after
// the session ended fails rather than running on. The context
// is canceled after the access check, so only git is left to notice.
func TestRepoCommandsRunGitUnderSessionContext(t *testing.T) {
	t.Parallel()
	ctx, sha := viewRepo(t)
	for _, args := range [][]string{
		{"info", "view"},
		{"tree", "view"},
		{"blob", "view", "README.md"},
		{"commit", "view", sha},
		{"branch", "list", "view"},
		{"branch", "default", "view"},
		{"tag", "list", "view"},
	} {
		sessCtx, cancel := context.WithCancel(ctx)
		afterGate(sessCtx, cancel)
		out, _, err := runRepoOutput(t, sessCtx, args...)
		cancel()
		if err == nil {
			t.Errorf("%v after the session ended: no error, output %q", args, out)
		}
	}
}

// escapes holds terminal control sequences a repository or a webhook receiver
// could smuggle into command output: an APC, a DCS, an OSC 52 clipboard
// write and a bare BEL.
const escapes = "\x1b_Gx\x1b\\\x1bP1|x\x1b\\\x1b]52;c;Zm9v\x07\x07"

// assertNoControls fails if out holds any control character other than
// newline and tab.
func assertNoControls(t *testing.T, out string) {
	t.Helper()
	for _, r := range out {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)) {
			t.Fatalf("output holds control character %q: %q", r, out)
		}
	}
}

// A description and project name stored before they were sanitised on
// write must still not reach the terminal raw.
func TestRepoDescriptionAndProjectNameSanitised(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	st, dbx := cmdsOf(ctx).store, cmdsOf(ctx).db
	if err := st.SetRepoDescriptionByName(ctx, dbx, "hooks", "desc"+escapes); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRepoProjectNameByName(ctx, dbx, "hooks", "name"+escapes); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"info", "hooks"},
		{"description", "hooks"},
		{"project-name", "hooks"},
	} {
		stdout, stderr, err := runRepoOutput(t, ownerCtx, args...)
		if err != nil {
			t.Fatal(err)
		}
		out := stdout + stderr
		assertNoControls(t, out)
		if !strings.Contains(out, "desc") && !strings.Contains(out, "name") {
			t.Errorf("%v: output %q lacks the value", args, out)
		}
	}
}

// The response of a webhook receiver is shown to the admin inspecting a
// delivery, so it must not carry escape sequences to their terminal.
func TestWebhookDeliveriesGetSanitisesOutput(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)
	delID := uuid.MustParse("00000000-0000-0000-0000-0000000000d2")
	if err := cmdsOf(ctx).store.CreateWebhookDelivery(ctx, cmdsOf(ctx).db, models.WebhookDelivery{
		ID:              delID,
		WebhookID:       id,
		Event:           int(webhook.EventPush),
		RequestURL:      whURL,
		RequestMethod:   "POST",
		RequestHeaders:  "X-Req: a" + escapes + "\r\n",
		RequestBody:     `{"msg":"` + escapes + `"}`,
		ResponseStatus:  200,
		ResponseHeaders: "X-Reply: b" + escapes + "\r\n",
		ResponseBody:    "reply" + escapes,
	}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "get", "hooks", strconv.FormatInt(id, 10), delID.String())
	if err != nil {
		t.Fatal(err)
	}
	assertNoControls(t, stdout+stderr)
	for _, want := range []string{"  X-Req: a", `  {"msg":""}`, "  X-Reply: b", "  reply"} {
		if !hasLine(stdout, want) {
			t.Fatal("not true: hasLine(stdout, want)")
		}
	}
}

// hostile adds an OSC 8 hyperlink with a hidden target and a C1 CSI to
// escapes. It has no slash, so it can be part of a path in a git tree, but
// it holds control characters, ':', '|' and '\', which Windows does not
// allow in a file name, so it never goes on disk.
const hostile = escapes + "\x1b]8;;https:evil.example\x1b\\link\x1b]8;;\x1b\\" + "\u009b2J"

// pushHostileRepo pushes to repository "hooks" a commit, an annotated tag
// and a file name and contents that all carry hostile, and a branch and a tag
// whose names hold a C1 control (the only kind git allows in a ref name).
func pushHostileRepo(t *testing.T, ctx context.Context) {
	t.Helper()
	bare := filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "hooks.git")
	work := t.TempDir()
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=eve" + hostile, "-c", "user.email=t@example.com"}, args...)...) //nolint:noctx
		cmd.Dir = work
		cmd.Stdin = strings.NewReader(stdin)
		// Ignore the user's git config, which may sign or annotate tags.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n\n// go-comment"+hostile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("", "add", "main.go")
	// The hostile file name goes straight into the index, so no file of that
	// name is made; core.protectNTFS would reject the '\' in it on Windows.
	blob := git("plain\n", "hash-object", "-w", "--stdin")
	git("", "-c", "core.protectNTFS=false", "update-index", "--add", "--cacheinfo", "100644,"+blob+",name"+hostile+"end.txt")
	git("", "commit", "-q", "-m", "subject"+hostile+"\n\nbody"+hostile)
	git("", "tag", "-a", "v1", "-m", "tag"+hostile)
	git("", "tag", "tag\u009b2Jname")
	git("", "push", "-q", bare, "main", "main:branch\u009b2Jname", "v1", "tag\u009b2Jname")
	if err := exec.Command("git", "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main").Run(); err != nil { //nolint:noctx
		t.Fatal(err)
	}
}

// Every command that shows git data for reading in a terminal strips what
// the repository carries, while the text around it stays.
func TestRepoGitDataSanitised(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	pushHostileRepo(t, ownerCtx)

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"info", "hooks"}, []string{"branch2Jname", "main"}},
		{[]string{"branch", "list", "hooks"}, []string{"branch2Jname"}},
		{[]string{"branch", "default", "hooks"}, []string{"main"}},
		{[]string{"tag", "list", "hooks"}, []string{"v1", "tag2Jname"}},
		{[]string{"tree", "hooks"}, []string{"main.go", "name"}},
		{[]string{"commit", "hooks", "main"}, []string{"subject", "body", "eve", "go-comment", "end.txt"}},
		{[]string{"commit", "--color", "hooks", "main"}, []string{"subject", "go-comment"}},
		{[]string{"commit", "--color", "--patch", "hooks", "main"}, []string{"go-comment"}},
		{[]string{"blob", "--color", "hooks", "main.go"}, []string{"go-comment"}},
		{[]string{"blob", "--linenumber", "hooks", "main.go"}, []string{"go-comment"}},
	} {
		stdout, stderr, err := runRepoOutput(t, ownerCtx, tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		out := csiSeq.ReplaceAllString(stdout+stderr, "")
		assertNoControls(t, out)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: output lacks %q: %q", tc.args, w, out)
			}
		}
	}

	// A plain patch and a plain blob are meant to be piped (git apply, a
	// file) and stay byte for byte.
	stdout, _, err := runRepoOutput(t, ownerCtx, "commit", "--patch", "hooks", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "go-comment"+hostile) {
		t.Fatal("not true: strings.Contains(stdout, \"go-comment\"+hostile)")
	}
	stdout, _, err = runRepoOutput(t, ownerCtx, "blob", "hooks", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "go-comment"+hostile) {
		t.Fatal("not true: strings.Contains(stdout, \"go-comment\"+hostile)")
	}
}

// csiSeq matches the SGR styles the commands themselves emit with --color.
var csiSeq = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
