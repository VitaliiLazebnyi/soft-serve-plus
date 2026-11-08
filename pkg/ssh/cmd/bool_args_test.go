package cmd

import (
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

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

// wantInvalidBool accepts both the shared parseBool error and the
// command-specific errors of hidden and allow-keyless; all of them name the
// accepted values as "true or false".
func wantInvalidBool(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "invalid ") || !strings.Contains(err.Error(), "true or false") {
		t.Fatalf("expected invalid boolean error, got %v", err)
	}
}

// TestRepoBoolArgs is the regression test for repository boolean properties
// treating anything other than the exact string "true" as false: an
// uppercase TRUE turned the property off and a typo was silently accepted.
func TestRepoBoolArgs(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	_, err := be.CreateRepository(adminCtx, "r", proto.UserFromContext(adminCtx), proto.RepositoryOptions{})
	is.NoErr(err)

	get := map[string]func() bool{
		"hidden": func() bool {
			v, err := be.IsHidden(ctx, "r")
			is.NoErr(err)
			return v
		},
		"private": func() bool {
			v, err := be.IsPrivate(ctx, "r")
			is.NoErr(err)
			return v
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

	// hidden used to accept and ignore any number of extra arguments.
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
