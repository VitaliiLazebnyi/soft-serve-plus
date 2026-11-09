package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestInfoRefsGitFailure covers a ref advertisement that git fails to make
// for a repository that exists. It used to answer 404, as if the repository
// were missing, and log nothing; it must answer 500 and log git's reason.
func TestInfoRefsGitFailure(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)

	ctx, be, _ := newLFSTestContext(t)
	ctx = log.WithContext(ctx, logger)
	_, err := be.CreateRepository(ctx, "broken", nil, proto.RepositoryOptions{})
	is.NoErr(err)

	// A HEAD git cannot read makes the directory no longer a repository
	// to git, while soft-serve still knows it.
	dir := filepath.Join(config.FromContext(ctx).DataPath, "repos", "broken.git")
	is.NoErr(os.WriteFile(filepath.Join(dir, "HEAD"), []byte("garbage\n"), 0o600))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/broken.git/info/refs?service=git-upload-pack", nil)
	w := httptest.NewRecorder()
	NewRouter(ctx).ServeHTTP(w, req)

	is.Equal(w.Code, http.StatusInternalServerError)
	out := logs.String()
	if !strings.Contains(out, "failed to advertise refs") || !strings.Contains(out, "fatal:") {
		t.Errorf("log does not carry git's reason:\n%s", out)
	}
}
