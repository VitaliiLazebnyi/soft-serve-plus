package web

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	gitb "github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestHTTPMirrorRefusesPush verifies that a caller allowed to write gets a
// 403 carrying ErrMirrorReadOnly for both the receive-pack advertisement and
// the receive-pack RPC on a mirror, while upload-pack still serves it, and
// that turning mirroring off lets the push advertisement through again.
func TestHTTPMirrorRefusesPush(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	auth := "Token " + f.token
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)

	_, err := f.be.CreateRepository(ownerCtx, "m", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(f.be.AddCollaborator(ownerCtx, "m", "stranger", access.ReadWriteAccess))
	r, err := gitb.Open(filepath.Join(config.FromContext(f.ctx).DataPath, "repos", "m.git"))
	is.NoErr(err)
	rcfg, err := r.Config()
	is.NoErr(err)
	rcfg.Section("remote").Subsection("origin").SetOption("url", "https://1.1.1.1/x.git")
	is.NoErr(r.SetConfig(rcfg))
	is.NoErr(f.be.SetMirror(ownerCtx, "m", true))

	want := proto.ErrMirrorReadOnly.Error()
	adv := f.do(http.MethodGet, "/m.git/info/refs?service=git-receive-pack", auth)
	is.Equal(adv.Code, http.StatusForbidden)
	is.Equal(strings.TrimSpace(adv.Body.String()), want)

	rpc := f.do(http.MethodPost, "/m.git/git-receive-pack", auth)
	is.Equal(rpc.Code, http.StatusForbidden)
	is.Equal(strings.TrimSpace(rpc.Body.String()), want)

	// Reads are not refused.
	up := f.do(http.MethodGet, "/m.git/info/refs?service=git-upload-pack", auth)
	is.Equal(up.Code, http.StatusOK)
	is.True(!strings.Contains(up.Body.String(), want))

	// Without mirroring the push advertisement is served.
	is.NoErr(f.be.SetMirror(ownerCtx, "m", false))
	adv = f.do(http.MethodGet, "/m.git/info/refs?service=git-receive-pack", auth)
	is.Equal(adv.Code, http.StatusOK)
}
