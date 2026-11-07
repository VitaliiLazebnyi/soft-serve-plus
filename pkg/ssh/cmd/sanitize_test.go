package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
	"github.com/matryer/is"
)

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
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	st, dbx := store.FromContext(ctx), db.FromContext(ctx)
	is.NoErr(st.SetRepoDescriptionByName(ctx, dbx, "hooks", "desc"+escapes))
	is.NoErr(st.SetRepoProjectNameByName(ctx, dbx, "hooks", "name"+escapes))

	for _, args := range [][]string{
		{"info", "hooks"},
		{"description", "hooks"},
		{"project-name", "hooks"},
	} {
		stdout, stderr, err := runRepoOutput(t, ownerCtx, args...)
		is.NoErr(err)
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
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)
	delID := uuid.MustParse("00000000-0000-0000-0000-0000000000d2")
	is.NoErr(store.FromContext(ctx).CreateWebhookDelivery(
		ctx, db.FromContext(ctx), delID, id, int(webhook.EventPush),
		whURL, "POST", nil,
		"X-Req: a"+escapes+"\r\n", `{"msg":"`+escapes+`"}`,
		200, "X-Reply: b"+escapes+"\r\n", "reply"+escapes,
	))

	stdout, stderr, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "get", "hooks", strconv.FormatInt(id, 10), delID.String())
	is.NoErr(err)
	assertNoControls(t, stdout+stderr)
	for _, want := range []string{"  X-Req: a", `  {"msg":""}`, "  X-Reply: b", "  reply"} {
		is.True(hasLine(stdout, want))
	}
}

// hostile adds an OSC 8 hyperlink with a hidden target and a C1 CSI to
// escapes; it has no slash, so it can be part of a file name.
const hostile = escapes + "\x1b]8;;https:evil.example\x1b\\link\x1b]8;;\x1b\\" + "\u009b2J"

// pushHostileRepo pushes to repository "hooks" a commit, an annotated tag
// and a file name and contents that all carry hostile, and a branch and a tag
// whose names hold a C1 control (the only kind git allows in a ref name).
func pushHostileRepo(t *testing.T, ctx context.Context) {
	t.Helper()
	bare := filepath.Join(config.FromContext(ctx).DataPath, "repos", "hooks.git")
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=eve" + hostile, "-c", "user.email=t@example.com"}, args...)...) //nolint:noctx
		cmd.Dir = work
		// Ignore the user's git config, which may sign or annotate tags.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	for name, content := range map[string]string{
		"main.go":                    "package main\n\n// go-comment" + hostile + "\n",
		"name" + hostile + "end.txt": "plain\n",
	} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "subject"+hostile+"\n\nbody"+hostile)
	git("tag", "-a", "v1", "-m", "tag"+hostile)
	git("tag", "tag\u009b2Jname")
	git("push", "-q", bare, "main", "main:branch\u009b2Jname", "v1", "tag\u009b2Jname")
	is.New(t).NoErr(exec.Command("git", "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main").Run()) //nolint:noctx
}

// Every command that shows git data for reading in a terminal strips what
// the repository carries, while the text around it stays.
func TestRepoGitDataSanitised(t *testing.T) {
	is := is.New(t)
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
		is.NoErr(err)
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
	is.NoErr(err)
	is.True(strings.Contains(stdout, "go-comment"+hostile))
	stdout, _, err = runRepoOutput(t, ownerCtx, "blob", "hooks", "main.go")
	is.NoErr(err)
	is.True(strings.Contains(stdout, "go-comment"+hostile))
}

// csiSeq matches the SGR styles the commands themselves emit with --color.
var csiSeq = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
