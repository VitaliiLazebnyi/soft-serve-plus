package web

import (
	"net/http"
	"net/url"
	"path"
	"text/template"

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
    <meta http-equiv="refresh" content="0; url=https://pkg.go.dev/{{ .ImportRoot }}/{{ .Repo }}">
    <meta name="go-import" content="{{ .ImportRoot }}/{{ .Repo }} git {{ .Config.HTTP.PublicURL }}/{{ .Repo }}.git">
</head>
<body>
Redirecting to docs at <a href="https://pkg.go.dev/{{ .ImportRoot }}/{{ .Repo }}">pkg.go.dev/{{ .ImportRoot }}/{{ .Repo }}</a>...
</body>
</html>
`))

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

		if err := repoIndexHTMLTpl.Execute(w, struct {
			Repo       string
			Config     *config.Config
			ImportRoot string
		}{
			Repo:       utils.SanitizeRepo(repo),
			Config:     cfg,
			ImportRoot: importRoot.Host,
		}); err != nil {
			logger.Error("failed to render go get template", "err", err)
			renderInternalServerError(w, r)
			return
		}

		goGetCounter.WithLabelValues(repo).Inc()
		return
	}

	renderNotFound(w, r)
}
