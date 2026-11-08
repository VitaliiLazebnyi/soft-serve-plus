package cmd

import (
	"strconv"
	"strings"
	"testing"

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
