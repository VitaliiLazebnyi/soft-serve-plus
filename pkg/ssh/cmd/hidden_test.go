package cmd

import (
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// repo hidden must reject a value that is not a boolean instead of reading it
// as false, which would silently unhide the repository.
func TestRepoHiddenParsesBool(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "secret", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)

	hidden := func() bool {
		t.Helper()
		r, err := be.Repository(ctx, "secret")
		is.NoErr(err)
		return r.IsHidden()
	}

	for _, tc := range []struct {
		arg  string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"1", true},
		{"0", false},
		{"True", true},
		{"F", false},
		{"t", true},
	} {
		is.NoErr(runRepo(t, ownerCtx, "hidden", "secret", tc.arg))
		if got := hidden(); got != tc.want {
			t.Errorf("hidden %s: IsHidden() = %v, want %v", tc.arg, got, tc.want)
		}
	}

	// The repository is hidden now; no bad value may unhide it.
	is.True(hidden())
	for _, arg := range []string{"yes", "no", "off", "maybe", "", "true "} {
		_, _, err := runRepoOutput(t, ownerCtx, "hidden", "secret", arg)
		if err == nil || !strings.Contains(err.Error(), "invalid hidden value") {
			t.Errorf("hidden %q: err = %v, want an invalid hidden value error", arg, err)
		}
		if !hidden() {
			t.Fatalf("hidden %q unhid the repository", arg)
		}
	}

	// A stray extra argument is refused rather than ignored.
	is.True(runRepo(t, ownerCtx, "hidden", "secret", "false", "extra") != nil)
	is.True(hidden())

	// Reading the value still works.
	out, _, err := runRepoOutput(t, ownerCtx, "hidden", "secret")
	is.NoErr(err)
	is.Equal(strings.TrimSpace(out), "true")
}
