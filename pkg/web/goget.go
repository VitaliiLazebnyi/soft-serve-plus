package web

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var goGetCounter = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "soft_serve",
	Subsystem: "http",
	Name:      "go_get_total",
	Help:      "The total number of go get requests",
}, []string{"repo"})

var repoIndexHTMLTpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta http-equiv="Content-Type" content="text/html; charset=utf-8"/>
    <meta http-equiv="refresh" content="0; url={{ .DocURL }}">
    <meta name="go-import" content="{{ .ImportRoot }}/{{ .Repo }} git {{ .Config.HTTP.PublicURL }}/{{ .Repo }}.git">
</head>
<body>
Redirecting to docs at <a href="{{ .DocURL }}">pkg.go.dev/{{ .ImportRoot }}/{{ .Repo }}</a>...
</body>
</html>
`))

// goImportRoot returns the import path prefix of the repositories served at
// the public URL u: its host followed by its path, so a server published
// under "https://example.com/git" serves "example.com/git/<repo>".
func goImportRoot(u *url.URL) string {
	return u.Host + strings.TrimRight(u.Path, "/")
}

// maxGoGetPathLen bounds the import path a go get request may walk. Each
// step of the walk costs one filesystem lookup, and a step per segment of an
// unbounded request path would let one anonymous request run for minutes. No
// repository can be named by a path longer than the usual PATH_MAX, so a
// longer import path never resolves to one.
const maxGoGetPathLen = 4096

// GoGetHandler handles go get requests.
func GoGetHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := config.FromContext(ctx)
	be := backend.FromContext(ctx)
	logger := log.FromContext(ctx)
	repo := mux.Vars(r)["repo"]

	// Handle go get requests.
	//
	// Walk up the path to the closest repository the caller can read, or
	// return a 404 if there is none.
	//
	// https://golang.org/cmd/go/#hdr-Remote_import_paths
	// https://go.dev/ref/mod#vcs-branch
	if r.URL.Query().Get("go-get") == "1" {
		repo := repo
		if len(repo) > maxGoGetPathLen {
			renderNotFound(w, r)
			return
		}

		importRoot, err := url.Parse(cfg.HTTP.PublicURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Find the closest repository the caller can read; skip the rest. A
		// bearer token only stands for the caller on the repositories it
		// names, so a token for "repo" does not reveal "repo/sub".
		for {
			if ctx.Err() != nil {
				return
			}

			if _, err := be.Repository(ctx, repo); err == nil && be.AccessLevelForUser(ctx, repo, callerFor(ctx, repo)) >= access.ReadOnlyAccess {
				break
			}

			if repo == "" || repo == "." || repo == "/" {
				renderNotFound(w, r)
				return
			}

			repo = path.Dir(repo)
		}

		// Render into a buffer so a failure can still answer with a 500.
		var buf bytes.Buffer
		name := utils.SanitizeRepo(repo)
		root := goImportRoot(importRoot)
		// html/template refuses a URL it cannot vet inside the refresh
		// content attribute, so hand it one built and escaped here.
		docURL := (&url.URL{Scheme: "https", Host: "pkg.go.dev", Path: "/" + root + "/" + name}).String()
		if err := repoIndexHTMLTpl.Execute(&buf, struct {
			Repo       string
			Config     *config.Config
			ImportRoot string
			DocURL     template.URL
		}{
			Repo:       name,
			Config:     cfg,
			ImportRoot: root,
			DocURL:     template.URL(docURL), //nolint:gosec // built from a parsed URL above
		}); err != nil {
			logger.Error("failed to render go get template", "err", err)
			renderInternalServerError(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		buf.WriteTo(w) //nolint: errcheck

		goGetCounter.WithLabelValues(repo).Inc()
		return
	}

	renderNotFound(w, r)
}
