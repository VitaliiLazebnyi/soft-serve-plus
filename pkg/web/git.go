package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	gitb "github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// GitRoute is a route for git services.
type GitRoute struct {
	method  []string
	handler http.HandlerFunc
	path    string
	// pending tracks repositories created by push-to-create; see
	// pushCreates. GitController sets it.
	pending *pushCreates
}

var _ http.Handler = GitRoute{}

// ServeHTTP implements http.Handler. The method is checked before
// authorization so a mismatched method can neither probe nor create a
// repository.
func (g GitRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	allowed := g.allowed()
	if !slices.Contains(allowed, r.Method) {
		renderMethodNotAllowed(w, r, allowed)
		return
	}

	withAccess(g.pending, g.handler)(w, r)
}

// allowed returns the methods the route answers: a route that answers GET
// answers HEAD too, as git http-backend does.
func (g GitRoute) allowed() []string {
	allowed := g.method
	if slices.Contains(allowed, http.MethodGet) {
		allowed = append(slices.Clip(allowed), http.MethodHead)
	}
	return allowed
}

var (
	//nolint:revive
	gitHttpReceiveCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "http",
		Name:      "git_receive_pack_total",
		Help:      "The total number of git push requests",
	}, []string{"repo"})

	//nolint:revive
	gitHttpUploadCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "http",
		Name:      "git_upload_pack_total",
		Help:      "The total number of git fetch/pull requests",
	}, []string{"repo", "file"})
)

func withParams(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		cfg := config.FromContext(ctx)
		vars := mux.Vars(r)
		repo := vars["repo"]

		// Construct "file" param from path
		vars["file"] = strings.TrimPrefix(r.URL.Path, "/"+repo+"/")

		// The RPC route carries the service in its path; the caller-supplied
		// ?service= only means something on the ref advertisement.
		if vars["service"] == "" && vars["file"] == "info/refs" {
			vars["service"] = getServiceType(r).String()
		}

		repo = utils.SanitizeRepo(repo)
		vars["repo"] = repo
		vars["dir"] = filepath.Join(cfg.DataPath, "repos", repo+".git")
		r = mux.SetURLVars(r, vars)

		next.ServeHTTP(w, r)
	})
}

// GitController is a router for git services.
func GitController(_ context.Context, r *mux.Router) {
	gitController(r, newPushCreates())
}

// gitController routes git services, tracking the repositories HTTP pushes
// create in pending.
func gitController(r *mux.Router, pending *pushCreates) {
	basePrefix := "/{repo:.*}"
	for _, route := range gitRoutes {
		route.pending = pending
		// NOTE: withParams must always be the outermost wrapper, otherwise the
		// request vars will not be set.
		r.Handle(basePrefix+route.path, withParams(route))
	}

	// Handle go-get
	r.Handle(basePrefix, withParams(withCaller(http.HandlerFunc(GoGetHandler)))).Methods(http.MethodGet, http.MethodHead)
}

var gitRoutes = []GitRoute{
	// Git services
	// These handlers don't authenticate or authorize the caller: GitRoute
	// wraps each of them in withAccess.
	{
		method:  []string{http.MethodPost},
		handler: serviceRpc,
		path:    "/{service:(?:git-upload-archive|git-upload-pack|git-receive-pack)$}",
	},
	{
		method:  []string{http.MethodGet},
		handler: getInfoRefs,
		path:    "/info/refs",
	},
	{
		method:  []string{http.MethodGet},
		handler: getTextFile,
		path:    "/{_:(?:HEAD|objects/info/alternates|objects/info/http-alternates)$}",
	},
	{
		method:  []string{http.MethodGet},
		handler: getInfoPacks,
		path:    "/objects/info/packs",
	},
	{
		method:  []string{http.MethodGet},
		handler: getLooseObject,
		path:    "/objects/{_:[0-9a-f]{2}/(?:[0-9a-f]{38}|[0-9a-f]{62})$}",
	},
	{
		method:  []string{http.MethodGet},
		handler: getPackFile,
		path:    "/objects/pack/{_:pack-(?:[0-9a-f]{40}|[0-9a-f]{64})\\.pack$}",
	},
	{
		method:  []string{http.MethodGet},
		handler: getIdxFile,
		path:    "/objects/pack/{_:pack-(?:[0-9a-f]{40}|[0-9a-f]{64})\\.idx$}",
	},
	// Git LFS
	{
		method:  []string{http.MethodPost},
		handler: serviceLfsBatch,
		path:    "/info/lfs/objects/batch",
	},
	{
		// Git LFS basic object handler
		method:  []string{http.MethodGet, http.MethodPut},
		handler: serviceLfsBasic,
		path:    "/info/lfs/objects/basic/{oid:[0-9a-f]{64}$}",
	},
	{
		method:  []string{http.MethodPost},
		handler: serviceLfsBasicVerify,
		path:    "/info/lfs/objects/basic/verify",
	},
	// Git LFS locks
	{
		method:  []string{http.MethodPost, http.MethodGet},
		handler: serviceLfsLocks,
		path:    "/info/lfs/locks",
	},
	{
		method:  []string{http.MethodPost},
		handler: serviceLfsLocksVerify,
		path:    "/info/lfs/locks/verify",
	},
	{
		method:  []string{http.MethodPost},
		handler: serviceLfsLocksDelete,
		path:    "/info/lfs/locks/{lock_id:[0-9]+}/unlock",
	},
}

func askCredentials(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Basic realm="Git", charset="UTF-8", Token, Bearer`)
	w.Header().Set("LFS-Authenticate", `Basic realm="Git LFS", charset="UTF-8", Token, Bearer`)
}

// identify stores the authenticated caller in the request context and
// returns the authentication error, if any. A bearer token must name one of
// repos in its audience; its audience is stored too, see tokenAudience.
func identify(r *http.Request, repos ...string) (*http.Request, error) {
	ctx := r.Context()
	logger := log.FromContext(ctx)

	// authenticate logs why credentials failed, and returns only bad
	// credentials or proto.ErrUserNotFound.
	user, audience, err := authenticate(r, repos...)
	if user != nil {
		logger.Debug("authenticated", "username", user.Username())
	}

	ctx = proto.WithUserContext(ctx, user)
	if audience != nil {
		ctx = context.WithValue(ctx, tokenAudienceKey{}, audience)
	}
	return r.WithContext(ctx), err
}

type tokenAudienceKey struct{}

// tokenAudience returns the repositories the caller's bearer token names,
// and false when the caller did not authenticate with a bearer token, so is
// not limited to particular repositories.
func tokenAudience(ctx context.Context) ([]string, bool) {
	audience, ok := ctx.Value(tokenAudienceKey{}).([]string)
	return audience, ok
}

// callerFor returns the caller to authorize for repo: the authenticated
// user, or nil (anonymous) when the user authenticated with a bearer token
// that does not name repo.
func callerFor(ctx context.Context, repo string) proto.User {
	if audience, ok := tokenAudience(ctx); ok && !slices.Contains(audience, utils.SanitizeRepo(repo)) {
		return nil
	}
	return proto.UserFromContext(ctx)
}

// badCredentials reports whether the caller sent credentials that failed.
func badCredentials(err error) bool {
	return errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrInvalidPassword)
}

// denyLFSWrite answers an LFS write by a caller without write access. An
// anonymous caller gets 401 with a credentials challenge, since git-lfs only
// asks for credentials after a 401 and otherwise gives up on the push. A
// signed-in caller gets 403.
func denyLFSWrite(w http.ResponseWriter, r *http.Request) {
	if proto.UserFromContext(r.Context()) == nil {
		askCredentials(w, r)
		renderJSON(w, r, http.StatusUnauthorized, lfs.ErrorResponse{Message: "credentials needed"})
		return
	}
	renderJSON(w, r, http.StatusForbidden, lfs.ErrorResponse{Message: "write access required"})
}

// denyRead answers a caller who may not read the repository. Missing and
// unreadable repositories get the same answer so neither is revealed.
func denyRead(w http.ResponseWriter, r *http.Request, err error, lfsJSON bool) {
	switch {
	case badCredentials(err) && lfsJSON:
		renderJSON(w, r, http.StatusForbidden, lfs.ErrorResponse{Message: "bad credentials"})
	case badCredentials(err):
		renderForbidden(w, r)
	case proto.UserFromContext(r.Context()) == nil && lfsJSON:
		askCredentials(w, r)
		renderJSON(w, r, http.StatusUnauthorized, lfs.ErrorResponse{Message: "credentials needed"})
	case proto.UserFromContext(r.Context()) == nil:
		askCredentials(w, r)
		renderUnauthorized(w, r)
	case lfsJSON:
		renderJSON(w, r, http.StatusNotFound, lfs.ErrorResponse{Message: "repository not found"})
	default:
		renderNotFound(w, r)
	}
}

// lfsNeedsWrite reports whether an LFS request needs write access.
// https://github.com/git-lfs/git-lfs/blob/main/docs/api/locking.md
func lfsNeedsWrite(file, method string) bool {
	switch {
	case strings.HasPrefix(file, "info/lfs/locks"):
		return strings.HasSuffix(file, "lfs/locks") ||
			strings.HasSuffix(file, "lfs/locks/verify") ||
			strings.HasSuffix(file, "/unlock") && method == http.MethodPost
	case strings.HasPrefix(file, "info/lfs/objects/basic"):
		return method == http.MethodPut
	}
	return false
}

// isPushEndpoint reports whether a receive-pack request on file may create
// the repository: only the ref advertisement and the RPC itself.
func isPushEndpoint(file string) bool {
	return file == "info/refs" || file == git.ReceivePackService.String()
}

// verdict is the outcome of authorizing a git or LFS request.
type verdict int

const (
	// serve lets the request through.
	serve verdict = iota
	// createThenServe creates the missing repository, then serves.
	createThenServe
	// denyUnreadable answers a caller who may not read the repository; see
	// denyRead. Missing and private repositories get this same verdict.
	denyUnreadable
	// forbid answers 403: bad credentials, or a signed-in reader who may
	// not write.
	forbid
	// unauthorized answers 401 with a credentials challenge: an anonymous
	// reader who may not write.
	unauthorized
	// forbidLFSWrite answers the LFS "write access required" error: a
	// signed-in reader who may not write.
	forbidLFSWrite
	// unauthorizedLFS answers an LFS 401 with a credentials challenge: an
	// anonymous reader who may not write. git-lfs only asks for credentials
	// after a 401; a 403 ends the push.
	unauthorizedLFS
)

// authorize decides what to do with a request without touching the
// response, so the decision order can be tested as a table. readable is
// whether the caller may read an existing repository; a private repository
// the caller cannot read and a missing one both arrive as !readable, so
// they get the same verdict. Bad credentials are rejected before a push can
// create anything.
func authorize(service git.Service, file, method string, readable bool, level access.AccessLevel, anonymous, badCreds bool) verdict {
	switch {
	case strings.HasPrefix(file, "info/lfs"):
		switch {
		case !readable:
			return denyUnreadable
		case badCreds:
			return forbid
		case lfsNeedsWrite(file, method) && level < access.ReadWriteAccess && anonymous:
			return unauthorizedLFS
		case lfsNeedsWrite(file, method) && level < access.ReadWriteAccess:
			return forbidLFSWrite
		}
	case service == git.ReceivePackService:
		switch {
		case badCreds:
			return forbid
		case !readable && level >= access.ReadWriteAccess && isPushEndpoint(file) && method != http.MethodHead:
			// HEAD is answered like GET, but only to check the
			// route, so it does not create the repository.
			return createThenServe
		case !readable:
			return denyUnreadable
		case level < access.ReadWriteAccess && anonymous:
			return unauthorized
		case level < access.ReadWriteAccess:
			return forbid
		}
	default:
		switch {
		case badCreds:
			return forbid
		case !readable:
			return denyUnreadable
		}
	}
	return serve
}

// requireCaller rejects anonymous requests when keyless access is disabled.
func requireCaller(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	if proto.UserFromContext(ctx) == nil && !backend.FromContext(ctx).AllowKeyless(ctx) {
		askCredentials(w, r)
		renderUnauthorized(w, r)
		return false
	}
	return true
}

// withCaller authenticates the caller without authorizing a repository.
// Handlers behind it must check access to any repository they reveal, as
// callerFor the repository: the request is for a path inside a repository,
// so a bearer token is accepted when it names the path or any directory
// above it, but it only stands for the caller on the repositories it names.
func withCaller(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, err := identify(r, pathAncestors(mux.Vars(r)["repo"])...)
		if !requireCaller(w, r) {
			return
		}

		if badCredentials(err) {
			renderForbidden(w, r)
			return
		}

		next.ServeHTTP(w, r)
	}
}

// pathAncestors returns p followed by each of its parent directories, each
// spelled as a sanitized repository name, which is what `soft jwt` mints.
func pathAncestors(p string) []string {
	ancestors := []string{p}
	for {
		p = path.Dir(p)
		if p == "." || p == "/" {
			return ancestors
		}
		ancestors = append(ancestors, utils.SanitizeRepo(p))
	}
}

// withAccess authenticates the caller and authorizes them for the requested
// repository. A repository created by push-to-create is recorded in pending,
// which removes it if the push fails, or never arrives, without any ref.
func withAccess(pending *pushCreates, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, err := identify(r, mux.Vars(r)["repo"])
		if !requireCaller(w, r) {
			return
		}

		ctx := r.Context()
		cfg := config.FromContext(ctx)
		logger := log.FromContext(ctx)
		be := backend.FromContext(ctx)
		repoName := mux.Vars(r)["repo"]
		user := proto.UserFromContext(ctx)
		file := mux.Vars(r)["file"]
		service := git.Service(mux.Vars(r)["service"])
		lfsJSON := strings.HasPrefix(file, "info/lfs")

		if lfsJSON && !cfg.LFS.Enabled {
			logger.Debug("LFS is not enabled, skipping")
			renderNotFound(w, r)
			return
		}

		// repo is nil unless the caller may read it, so a private and a
		// missing repository are already indistinguishable here.
		repo, accessLevel := be.Readable(ctx, repoName, user)
		ctx = proto.WithRepositoryContext(ctx, repo)
		ctx = access.WithContext(ctx, accessLevel)
		r = r.WithContext(ctx)

		switch authorize(service, file, r.Method, repo != nil, accessLevel, user == nil, badCredentials(err)) {
		case createThenServe:
			private, cerr := be.DefaultPrivate(ctx, user)
			if errors.Is(cerr, proto.ErrAnonymousPrivate) {
				logger.Info("refusing anonymous push-to-create", "repo", repoName, "err", cerr)
				// Ask for credentials, as for the unauthorized verdict: git
				// sends none on its first request, and only retries with
				// them after a 401. A signed-in user then creates the
				// repository; an anonymous client sees this body.
				askCredentials(w, r)
				http.Error(w, cerr.Error()+"; authenticate to create a repository by pushing, or ask an admin", http.StatusUnauthorized)
				return
			}
			if cerr == nil {
				repo, cerr = be.CreateRepository(ctx, repoName, user, proto.RepositoryOptions{Private: private})
			}
			if errors.Is(cerr, proto.ErrRepoLimitReached) {
				logger.Info("refusing push-to-create", "repo", repoName, "err", cerr)
				// git shows a text/plain error body to the user.
				http.Error(w, cerr.Error(), http.StatusForbidden)
				return
			}
			if cerr != nil {
				logger.Error("failed to create repository", "repo", repoName, "err", cerr)
				renderInternalServerError(w, r)
				return
			}

			ctx = proto.WithRepositoryContext(ctx, repo)
			r = r.WithContext(ctx)
			pending.add(ctx, repoName, repo, mux.Vars(r)["dir"])
		case denyUnreadable:
			denyRead(w, r, err, lfsJSON)
			return
		case forbid:
			if lfsJSON {
				// Only bad credentials forbid an LFS request.
				renderJSON(w, r, http.StatusForbidden, lfs.ErrorResponse{Message: "bad credentials"})
			} else {
				renderForbidden(w, r)
			}
			return
		case unauthorized:
			askCredentials(w, r)
			renderUnauthorized(w, r)
			return
		case forbidLFSWrite, unauthorizedLFS:
			denyLFSWrite(w, r)
			return
		}

		ctx, done := pending.begin(ctx, repoName)
		if done != nil {
			defer done()
			r = r.WithContext(ctx)
		}

		next.ServeHTTP(w, r)
	}
}

//nolint:revive
func serviceRpc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx)
	service, dir, repoName := git.Service(mux.Vars(r)["service"]), mux.Vars(r)["dir"], mux.Vars(r)["repo"]

	// git http-backend answers 415 to any other content type.
	if !hasMediaType(r, fmt.Sprintf("application/x-%s-request", service)) {
		renderStatus(http.StatusUnsupportedMediaType)(w, r)
		return
	}

	if service == git.ReceivePackService {
		gitHttpReceiveCounter.WithLabelValues(repoName)
	}

	hdrNocache(w)
	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", service))
	w.Header().Set("Connection", "Keep-Alive")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	version := r.Header.Get("Git-Protocol")

	cmd := git.ServiceCommand{
		Dir: dir,
	}

	switch service {
	case git.UploadPackService, git.ReceivePackService:
		cmd.Args = append(cmd.Args, "--stateless-rpc")
	}

	user := proto.UserFromContext(ctx)
	cmd.Env = cfg.Environ()
	cmd.Env = append(cmd.Env, []string{
		"SOFT_SERVE_REPO_NAME=" + repoName,
		"SOFT_SERVE_REPO_PATH=" + dir,
		"SOFT_SERVE_LOG_PATH=" + filepath.Join(cfg.DataPath, "log", "hooks.log"),
	}...)
	if user != nil {
		cmd.Env = append(cmd.Env, []string{
			"SOFT_SERVE_USERNAME=" + user.Username(),
		}...)
	}
	if len(version) != 0 {
		cmd.Env = append(cmd.Env, []string{
			fmt.Sprintf("GIT_PROTOCOL=%s", version),
		}...)
	}

	var (
		err    error
		reader io.ReadCloser
	)

	// Handle gzip encoding
	reader = r.Body
	switch r.Header.Get("Content-Encoding") {
	case "gzip":
		reader, err = gzip.NewReader(reader)
		if err != nil {
			logger.Errorf("failed to create gzip reader: %v", err)
			renderInternalServerError(w, r)
			return
		}
		defer reader.Close() //nolint: errcheck
	}

	cmd.Stdin = reader
	cmd.Stdout = w

	if err := service.Handler(ctx, cmd); err != nil {
		if errors.Is(err, context.Canceled) {
			// The client went away before git was done.
			logger.Debugf("service cancelled: %v", err)
			return
		}
		logger.Errorf("failed to handle service: %v", err)
		if service == git.ReceivePackService {
			markPushFailed(ctx)
		}
		return
	}

	if service == git.ReceivePackService {
		if err := git.EnsureDefaultBranch(ctx, cmd.Dir); err != nil {
			logger.Errorf("failed to ensure default branch: %s", err)
		}
	}
}

func getInfoRefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := config.FromContext(ctx)
	dir, repoName, file := mux.Vars(r)["dir"], mux.Vars(r)["repo"], mux.Vars(r)["file"]
	service := git.Service(mux.Vars(r)["service"])
	protocol := r.Header.Get("Git-Protocol")

	gitHttpUploadCounter.WithLabelValues(repoName, file).Inc()

	if service != "" && (service == git.UploadPackService || service == git.ReceivePackService) {
		// Smart HTTP
		var refs bytes.Buffer
		cmd := git.ServiceCommand{
			Stdout: &refs,
			Dir:    dir,
			Args:   []string{"--stateless-rpc", "--advertise-refs"},
		}

		user := proto.UserFromContext(ctx)
		cmd.Env = cfg.Environ()
		cmd.Env = append(cmd.Env, []string{
			"SOFT_SERVE_REPO_NAME=" + repoName,
			"SOFT_SERVE_REPO_PATH=" + dir,
			"SOFT_SERVE_LOG_PATH=" + filepath.Join(cfg.DataPath, "log", "hooks.log"),
		}...)
		if user != nil {
			cmd.Env = append(cmd.Env, []string{
				"SOFT_SERVE_USERNAME=" + user.Username(),
			}...)
		}
		if len(protocol) != 0 {
			cmd.Env = append(cmd.Env, fmt.Sprintf("GIT_PROTOCOL=%s", protocol))
		}

		var version int
		for _, p := range strings.Split(protocol, ":") {
			if strings.HasPrefix(p, "version=") {
				if v, _ := strconv.Atoi(p[8:]); v > version {
					version = v
				}
			}
		}

		if err := service.Handler(ctx, cmd); err != nil {
			renderNotFound(w, r)
			return
		}

		hdrNocache(w)
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
		w.WriteHeader(http.StatusOK)
		if version < 2 {
			git.WritePktline(w, "# service="+service.String()) //nolint: errcheck
		}
		w.Write(refs.Bytes()) //nolint: errcheck
	} else {
		// Dumb HTTP
		updateServerInfo(ctx, dir) //nolint: errcheck
		hdrNocache(w)
		sendFile("text/plain; charset=utf-8", w, r)
	}
}

func getInfoPacks(w http.ResponseWriter, r *http.Request) {
	// info/packs changes on every repack, so it must not be cached.
	hdrNocache(w)
	sendFile("text/plain; charset=utf-8", w, r)
}

func getLooseObject(w http.ResponseWriter, r *http.Request) {
	hdrCacheForever(w, r)
	sendFile("application/x-git-loose-object", w, r)
}

func getPackFile(w http.ResponseWriter, r *http.Request) {
	hdrCacheForever(w, r)
	sendFile("application/x-git-packed-objects", w, r)
}

func getIdxFile(w http.ResponseWriter, r *http.Request) {
	hdrCacheForever(w, r)
	sendFile("application/x-git-packed-objects-toc", w, r)
}

func getTextFile(w http.ResponseWriter, r *http.Request) {
	hdrNocache(w)
	sendFile("text/plain", w, r)
}

// sendFile serves the requested repository file. Anything that is not a
// readable regular file, whatever the reason, is not found.
func sendFile(contentType string, w http.ResponseWriter, r *http.Request) {
	dir, file := mux.Vars(r)["dir"], mux.Vars(r)["file"]
	reqFile := filepath.Join(dir, file)

	f, err := os.Open(reqFile)
	if err != nil {
		renderNotFound(w, r)
		return
	}
	defer f.Close() //nolint: errcheck

	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		renderNotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// getServiceType returns the service named by the ?service= query parameter.
// It never reads the request body: this runs before the method check and
// authentication, and parsing a form would let any caller spool a multipart
// body of any size to a temp file that is never removed.
func getServiceType(r *http.Request) git.Service {
	service := r.URL.Query().Get("service")
	if !strings.HasPrefix(service, "git-") {
		return ""
	}

	return git.Service(service)
}

// hasMediaType reports whether the request body has the media type want,
// ignoring any parameters such as charset.
func hasMediaType(r *http.Request, want string) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == want
}

func updateServerInfo(ctx context.Context, dir string) error {
	return gitb.UpdateServerInfo(ctx, dir)
}

// HTTP error response handling functions

// renderMethodNotAllowed answers 405 and lists the allowed methods.
func renderMethodNotAllowed(w http.ResponseWriter, r *http.Request, allowed []string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	renderStatus(http.StatusMethodNotAllowed)(w, r)
}

func renderNotFound(w http.ResponseWriter, r *http.Request) {
	renderStatus(http.StatusNotFound)(w, r)
}

func renderUnauthorized(w http.ResponseWriter, r *http.Request) {
	renderStatus(http.StatusUnauthorized)(w, r)
}

func renderForbidden(w http.ResponseWriter, r *http.Request) {
	renderStatus(http.StatusForbidden)(w, r)
}

func renderInternalServerError(w http.ResponseWriter, r *http.Request) {
	renderStatus(http.StatusInternalServerError)(w, r)
}

// Header writing functions

func hdrNocache(w http.ResponseWriter) {
	w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
}

// hdrCacheForever marks an immutable object as cacheable for a year. Shared
// caches may only store it when the repository is public and the request
// carries no credentials; otherwise it is private to the caller.
func hdrCacheForever(w http.ResponseWriter, r *http.Request) {
	const maxAge = 365 * 24 * time.Hour
	scope := "public"
	ctx := r.Context()
	if repo := proto.RepositoryFromContext(ctx); repo == nil || repo.IsPrivate() ||
		proto.UserFromContext(ctx) != nil || r.Header.Get("Authorization") != "" {
		scope = "private"
	}
	w.Header().Set("Expires", time.Now().Add(maxAge).UTC().Format(http.TimeFormat))
	w.Header().Set("Cache-Control", fmt.Sprintf("%s, max-age=%d", scope, int(maxAge.Seconds())))
}
