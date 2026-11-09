package web

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// A go get request walks up its path to the closest repository, one
// filesystem lookup per segment. An over-long path is refused before the
// walk, and a segment too long for the filesystem is not an error worth
// logging, so one anonymous request can neither burn CPU for minutes nor
// flood the error log.
//
// Backend lookups log through the logger captured when the backend is
// created, which falls back to the global default, so the test swaps the
// global default logger and must not run in parallel.
func TestGoGetLongPath(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)
	prev := log.Default()
	log.SetDefault(logger)
	t.Cleanup(func() { log.SetDefault(prev) })

	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)

	for _, tc := range []struct {
		name     string
		path     string
		wantCode int
	}{
		// A deep package path still resolves to its repository.
		{"deep package", "/pub/" + strings.Repeat("pkg/", 500), http.StatusOK},
		// A segment longer than any file name, under the length cap.
		{"long segment", "/pub/" + strings.Repeat("x", 300) + "/" + strings.Repeat("a/", 100), http.StatusOK},
		{"long segment missing", "/" + strings.Repeat("x", 300) + "/" + strings.Repeat("a/", 100), http.StatusNotFound},
		// Past the cap the request is refused without a single lookup.
		{"over cap", "/pub/" + strings.Repeat("a/", 10000), http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			logs.Reset()
			w := f.do(http.MethodGet, tc.path+"?go-get=1", "")
			is.Equal(w.Code, tc.wantCode)
			if tc.wantCode == http.StatusOK {
				is.True(strings.Contains(w.Body.String(), "/pub git ")) // resolves to the repository
			}
			is.True(!strings.Contains(logs.String(), "ERRO")) // no error logged
			if tc.name == "over cap" {
				is.True(!strings.Contains(logs.String(), "repository name")) // no lookup was made
			}
		})
	}
}
