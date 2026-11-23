package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestRepoPropertyLevels checks the level each property command needs to
// change its property: read-write for the text properties, admin for the
// boolean ones, whose access check comes before the value is parsed. Any
// reader may print every property.
func TestRepoPropertyLevels(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	writerCtx := withUser(t, ctx, be, "writer", false)
	_, err := be.CreateRepository(ownerCtx, "r", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(be.AddCollaborator(ownerCtx, "r", "writer", access.ReadWriteAccess))

	for _, prop := range []string{"description", "project-name"} {
		is.NoErr(runRepo(t, writerCtx, prop, "r", "a", "b"))
		out, _, err := runRepoOutput(t, writerCtx, prop, "r")
		is.NoErr(err)
		is.Equal(strings.TrimSpace(out), "a b")
	}
	for _, prop := range []string{"hidden", "mirror", "private"} {
		for _, v := range []string{"true", "maybe"} {
			if err := runRepo(t, writerCtx, prop, "r", v); !errors.Is(err, proto.ErrUnauthorized) {
				t.Errorf("repo %s r %s as a writer: err = %v, want %v", prop, v, err, proto.ErrUnauthorized)
			}
		}
		out, _, err := runRepoOutput(t, writerCtx, prop, "r")
		is.NoErr(err)
		is.Equal(strings.TrimSpace(out), "false")
	}
}

func TestParseBool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
		ok   bool
	}{
		{"true", true, true},
		{"TRUE", true, true},
		{"True", true, true},
		{"1", true, true},
		{"false", false, true},
		{"FALSE", false, true},
		{"0", false, true},
		{"yes", false, false},
		{"ture", false, false},
		{"", false, false},
	} {
		got, err := parseBool(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("parseBool(%q) error = %v, want ok=%t", tc.in, err, tc.ok)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBool(%q) = %t, want %t", tc.in, got, tc.want)
		}
	}
}

// wantInvalidBool accepts only the shared parseBool error, which every SSH
// command that takes a boolean argument returns.
func wantInvalidBool(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "invalid boolean value") || !strings.Contains(err.Error(), "true or false") {
		t.Fatalf("expected invalid boolean error, got %v", err)
	}
}

// TestRepoBoolArgs checks that repository boolean properties do not treat
// anything other than the exact string "true" as false: an uppercase TRUE
// turns the property on and a typo is refused.
func TestRepoBoolArgs(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	_, err := be.CreateRepository(adminCtx, "r", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
	is.NoErr(err)

	get := map[string]func() bool{
		"hidden": func() bool {
			r, err := be.Repository(ctx, "r")
			is.NoErr(err)
			return r.IsHidden()
		},
		"private": func() bool {
			r, err := be.Repository(ctx, "r")
			is.NoErr(err)
			return r.IsPrivate()
		},
	}
	for _, prop := range []string{"hidden", "private"} {
		is.NoErr(runRepo(t, adminCtx, prop, "r", "TRUE"))
		if !get[prop]() {
			t.Errorf("repo %s r TRUE: property not set", prop)
		}
		wantInvalidBool(t, runRepo(t, adminCtx, prop, "r", "yes"))
		if !get[prop]() {
			t.Errorf("repo %s r yes: property changed despite the error", prop)
		}
		is.NoErr(runRepo(t, adminCtx, prop, "r", "False"))
		if get[prop]() {
			t.Errorf("repo %s r False: property not cleared", prop)
		}
	}

	// Mirroring cannot be turned on without a remote, so TRUE must reach
	// the backend and be refused there rather than being read as false and
	// succeeding as a no-op.
	if err := runRepo(t, adminCtx, "mirror", "r", "TRUE"); err == nil || !strings.Contains(err.Error(), "no remote URL") {
		t.Errorf("repo mirror r TRUE: expected the no-remote error, got %v", err)
	}
	wantInvalidBool(t, runRepo(t, adminCtx, "mirror", "r", "yes"))
	is.NoErr(runRepo(t, adminCtx, "mirror", "r", "FALSE"))

	// hidden refuses extra arguments rather than ignore them.
	if err := runRepo(t, adminCtx, "hidden", "r", "true", "extra"); err == nil {
		t.Error("repo hidden r true extra: expected an argument count error")
	}
	if get["hidden"]() {
		t.Error("repo hidden r true extra: property changed")
	}
}

// TestUserSetAdminBoolArg checks that `user set-admin alice True` promotes
// alice instead of demoting her, and that a typo is rejected.
func TestUserSetAdminBoolArg(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	_, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)

	isAdmin := func() bool {
		u, err := be.User(ctx, "alice")
		is.NoErr(err)
		return u.IsAdmin()
	}

	is.NoErr(runUser(t, adminCtx, "set-admin", "alice", "True"))
	is.True(isAdmin())
	wantInvalidBool(t, runUser(t, adminCtx, "set-admin", "alice", "no"))
	is.True(isAdmin())
	is.NoErr(runUser(t, adminCtx, "set-admin", "alice", "false"))
	is.True(!isAdmin())
}

// TestSettingsAllowKeylessBoolArg checks that an unparsable value is rejected
// instead of disabling keyless access server-wide.
func TestSettingsAllowKeylessBoolArg(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, _ := newSettingsTestContext(t)

	_, _, err := runSettings(t, ctx, "allow-keyless", "true")
	is.NoErr(err)
	_, _, err = runSettings(t, ctx, "allow-keyless", "yes")
	wantInvalidBool(t, err)
	out, _, err := runSettings(t, ctx, "allow-keyless")
	is.NoErr(err)
	is.Equal(strings.TrimSpace(out), "true")
}
