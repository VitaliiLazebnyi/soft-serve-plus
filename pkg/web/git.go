package web

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// gitRoute is a route for git services. It holds the access its requests
// need, so the rule sits next to the route: see access.
type gitRoute struct {
	method  []string
	handler gitHandler
	path    string
	// lfs is set on the Git LFS routes. They are served only when LFS is
	// enabled, and answer refusals with an LFS JSON body.
	lfs bool
	// write lists the methods that need write access. The others need
	// read access.
	write []string
	// service is set on the routes the git service of the request decides:
	// receive-pack needs write access and may create a missing repository
	// (push-to-create); the other services need read access.
	service bool
	// server serves the route. gitController sets it.
	server *gitServer
}

var _ http.Handler = gitRoute{}

// gitHandler serves a request of a git route once withAccess has let it
// through. gr is never nil, and gr.repo is the repository the caller may
// read.
type gitHandler func(s *gitServer, w http.ResponseWriter, r *http.Request, gr *gitRequest)

// gitServer holds what the git routes share.
type gitServer struct {
	cfg       *config.Config
	be        *backend.Backend
	dbx       *db.DB
	datastore store.Store
	// pending tracks repositories created by push-to-create; see
	// pushCreates.
	pending *pushCreates
	// limits bounds how many requests the git routes serve at once; see
	// requestLimits. The router bounds the time of every request; see
	// requestDeadlines.
	limits *requestLimits
}

// newGitServer returns a server for the git routes, using the config,
// backend, database and store in ctx.
func newGitServer(ctx context.Context, pending *pushCreates) *gitServer {
	cfg := config.FromContext(ctx)
	return &gitServer{
		cfg:       cfg,
		be:        backend.FromContext(ctx),
		dbx:       db.FromContext(ctx),
		datastore: store.FromContext(ctx),
		pending:   pending,
		limits:    newRequestLimits(cfg.HTTP),
	}
}

// gitRequest is what a git route knows about a request. The route fills in
// the repository's name, directory, the path inside it and the git service;
// withAccess adds the caller, the repository and the caller's access level.
type gitRequest struct {
	// name is the sanitized repository name.
	name string
	// dir is the repository's directory.
	dir string
	// file is the request path inside the repository, such as "info/refs".
	file string
	// service is the git service named by the RPC path or, on the ref
	// advertisement, by ?service=; empty on other routes.
	service git.Service
	user    proto.User
	// repo is nil until withAccess finds the caller may read it.
	repo  proto.Repository
	level access.AccessLevel
}

// newRequest returns what the route knows about r.
func (g gitRoute) newRequest(r *http.Request) *gitRequest {
	vars := mux.Vars(r)
	repo := vars["repo"]
	gr := &gitRequest{
		file: strings.TrimPrefix(r.URL.Path, "/"+repo+"/"),
		// The RPC route carries the service in its path; the
		// caller-supplied ?service= only means something on the ref
		// advertisement.
		service: git.Service(vars["service"]),
		name:    utils.SanitizeRepo(repo),
	}
	if gr.service == "" && gr.file == "info/refs" {
		gr.service = getServiceType(r)
	}
	gr.dir = filepath.Join(g.server.cfg.DataPath, "repos", gr.name+".git")
	return gr
}

// routeAccess is the access a request of a route needs.
type routeAccess struct {
	// lfs: the route is a Git LFS route.
	lfs bool
	// write: the request needs write access, not just read access.
	write bool
	// create: a caller who may create repositories creates a missing one.
	create bool
}

// access returns the access a request with method and service needs.
func (g gitRoute) access(method string, service git.Service) routeAccess {
	a := routeAccess{lfs: g.lfs, write: slices.Contains(g.write, method)}
	if g.service && service == git.ReceivePackService {
		a.write = true
		// HEAD is answered like GET, but only to check the route, so it
		// does not create the repository.
		a.create = method != http.MethodHead
	}
	return a
}

// ServeHTTP implements http.Handler. The method is checked before
// authorization so a mismatched method can neither probe nor create a
// repository.
func (g gitRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	allowed := g.allowed()
	if !slices.Contains(allowed, r.Method) {
		renderMethodNotAllowed(w, r, allowed)
		return
	}

	g.server.limits.serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.server.withAccess(g, w, r)
	}))
}

// allowed returns the methods the route answers: a route that answers GET
// answers HEAD too, as git http-backend does.
func (g gitRoute) allowed() []string {
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

// withParams sanitizes the repository name in the URL variables of the go
// get route. A path longer than maxGoGetPathLen names no repository, so it
// is answered 404 before anything else is done with it: the go get route
// serves anonymous requests, and the work on a path grows with its length.
func withParams(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		if len(vars["repo"]) > maxGoGetPathLen {
			renderNotFound(w, r)
			return
		}
		vars["repo"] = utils.SanitizeRepo(vars["repo"])
		next.ServeHTTP(w, mux.SetURLVars(r, vars))
	})
}

// gitController routes git services, tracking the repositories HTTP pushes
// create in pending.
func gitController(ctx context.Context, r *mux.Router, pending *pushCreates) {
	basePrefix := "/{repo:.*}"
	server := newGitServer(ctx, pending)

	goGet := withParams(withCaller(http.HandlerFunc(goGetHandler)))

	// The dumb protocol is not served. Its requests get a fixed answer that
	// does not depend on the repository, so they are refused before anything
	// is looked up or authorized. A go get request for a repository or
	// package whose path ends in one of these names, such as "group/HEAD"
	// or "repo/info/refs", is a go get request. The ref advertisement route
	// would take a go get request for info/refs, so it is sent to the go get
	// handler here. The route matches the whole path, so the handler gets
	// the full path as the repository and walks up from it, as it does for
	// "group/HEAD".
	r.Handle(basePrefix, goGet).MatcherFunc(func(r *http.Request, _ *mux.RouteMatch) bool {
		return strings.HasSuffix(r.URL.Path, "/info/refs") && !isSmartService(getServiceType(r)) && isGoGet(r)
	})
	r.Handle(basePrefix+"/info/refs", http.HandlerFunc(refuseDumbHTTP)).MatcherFunc(func(r *http.Request, _ *mux.RouteMatch) bool {
		return !isSmartService(getServiceType(r)) && !isGoGet(r)
	})
	r.Handle(basePrefix+dumbPaths, http.HandlerFunc(refuseDumbHTTP)).MatcherFunc(func(r *http.Request, _ *mux.RouteMatch) bool {
		return !isGoGet(r)
	})

	for _, route := range gitRoutes {
		route.server = server
		r.Handle(basePrefix+route.path, route)
	}

	// Handle go-get
	r.Handle(basePrefix, goGet).Methods(http.MethodGet, http.MethodHead)
}

// dumbPaths matches the repository files only the dumb HTTP protocol fetches:
// HEAD, the alternates and pack lists, loose objects, packs and their indexes.
const dumbPaths = "/{_:(?:HEAD|objects/info/(?:alternates|http-alternates|packs)" +
	"|objects/[0-9a-f]{2}/(?:[0-9a-f]{38}|[0-9a-f]{62})" +
	"|objects/pack/pack-(?:[0-9a-f]{40}|[0-9a-f]{64})\\.(?:pack|idx))$}"

// errDumbHTTP is the answer to a dumb HTTP request.
var errDumbHTTP = errors.New("dumb HTTP is not supported; use smart HTTP (git 1.6.6 or later, without GIT_SMART_HTTP=0)")

// refuseDumbHTTP answers a request of the dumb HTTP protocol with 403, as git
// http-backend does when it is configured not to serve dumb clients. The
// answer is marked uncacheable, so no cache keeps it for a repository file.
func refuseDumbHTTP(w http.ResponseWriter, r *http.Request) {
	allowed := []string{http.MethodGet, http.MethodHead}
	if !slices.Contains(allowed, r.Method) {
		renderMethodNotAllowed(w, r, allowed)
		return
	}
	hdrNocache(w)
	// git shows a text/plain error body to the user.
	http.Error(w, errDumbHTTP.Error(), http.StatusForbidden)
}

// isGoGet reports whether r is a request the go get route answers.
func isGoGet(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Query().Get("go-get") == "1"
}

// isSmartService reports whether service is one a smart HTTP ref
// advertisement is served for.
func isSmartService(service git.Service) bool {
	return service == git.UploadPackService || service == git.ReceivePackService
}

var gitRoutes = []gitRoute{
	// Git services
	// These handlers don't authenticate or authorize the caller: gitRoute
	// wraps each of them in withAccess.
	{
		method:  []string{http.MethodPost},
		handler: (*gitServer).serviceRpc,
		path:    "/{service:(?:git-upload-archive|git-upload-pack|git-receive-pack)$}",
		service: true,
	},
	{
		method:  []string{http.MethodGet},
		handler: (*gitServer).getInfoRefs,
		path:    "/info/refs",
		service: true,
	},
	// Git LFS
	// https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md
	{
		// An upload batch needs write access; serviceLfsBatch checks
		// that, since the operation is in the body.
		method:  []string{http.MethodPost},
		handler: (*gitServer).serviceLfsBatch,
		path:    "/info/lfs/objects/batch",
		lfs:     true,
	},
	{
		// Git LFS basic object handler
		method:  []string{http.MethodGet, http.MethodPut},
		handler: (*gitServer).serviceLfsBasic,
		path:    "/info/lfs/objects/basic/{oid:[0-9a-f]{64}$}",
		lfs:     true,
		write:   []string{http.MethodPut},
	},
	{
		method:  []string{http.MethodPost},
		handler: (*gitServer).serviceLfsBasicVerify,
		path:    "/info/lfs/objects/basic/verify",
		lfs:     true,
	},
	// Git LFS locks
	// https://github.com/git-lfs/git-lfs/blob/main/docs/api/locking.md
	{
		// GET lists locks, which only needs read access.
		method:  []string{http.MethodPost, http.MethodGet},
		handler: (*gitServer).serviceLfsLocks,
		path:    "/info/lfs/locks",
		lfs:     true,
		write:   []string{http.MethodPost},
	},
	{
		method:  []string{http.MethodPost},
		handler: (*gitServer).serviceLfsLocksVerify,
		path:    "/info/lfs/locks/verify",
		lfs:     true,
		write:   []string{http.MethodPost},
	},
	{
		method:  []string{http.MethodPost},
		handler: (*gitServer).serviceLfsLocksDelete,
		path:    "/info/lfs/locks/{lock_id:[0-9]+}/unlock",
		lfs:     true,
		write:   []string{http.MethodPost},
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
	// credentials, errAuthUnavailable or proto.ErrUserNotFound.
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
	return errors.Is(err, errInvalidToken) || errors.Is(err, errInvalidPassword)
}

// authRetryAfter is the Retry-After, in seconds, of the answer to a request
// whose credentials could not be checked.
const authRetryAfter = "10"

// refuseUnchecked answers a request whose credentials could not be checked,
// see errAuthUnavailable, with 503 Service Unavailable, and reports whether
// it did. The answer carries no credentials challenge, so git keeps the
// stored credential, and authenticate has already logged the fault.
func refuseUnchecked(w http.ResponseWriter, r *http.Request, err error, lfsJSON bool) bool {
	if !errors.Is(err, errAuthUnavailable) {
		return false
	}
	w.Header().Set("Retry-After", authRetryAfter)
	if lfsJSON {
		renderJSON(w, r, http.StatusServiceUnavailable, lfs.ErrorResponse{Message: "service unavailable, try again later"})
	} else {
		renderStatus(w, http.StatusServiceUnavailable)
	}
	return true
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

// authorize decides what to do with a request that needs need, without
// touching the response, so the decision order can be tested as a table.
// readable is whether the caller may read an existing repository; a private
// repository the caller cannot read and a missing one both arrive as
// !readable, so they get the same verdict. Bad credentials are rejected
// before a push can create anything.
func authorize(need routeAccess, readable bool, level access.AccessLevel, anonymous, badCreds bool) verdict {
	canWrite := level >= access.ReadWriteAccess
	switch {
	case need.lfs:
		switch {
		case !readable:
			return denyUnreadable
		case badCreds:
			return forbid
		case need.write && !canWrite && anonymous:
			return unauthorizedLFS
		case need.write && !canWrite:
			return forbidLFSWrite
		}
	case need.write:
		switch {
		case badCreds:
			return forbid
		case !readable && canWrite && need.create:
			return createThenServe
		case !readable:
			return denyUnreadable
		case !canWrite && anonymous:
			return unauthorized
		case !canWrite:
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
//
// The list of those directories is built only for a bearer token, the only
// credential scoped to repositories.
func withCaller(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var repos []string
		if isBearer(r.Header.Get("Authorization")) {
			repos = pathAncestors(mux.Vars(r)["repo"])
		}
		r, err := identify(r, repos...)
		if refuseUnchecked(w, r, err, false) || !requireCaller(w, r) {
			return
		}

		if badCredentials(err) {
			renderForbidden(w, r)
			return
		}

		next.ServeHTTP(w, r)
	}
}

// testHookBeforePushCreate, when set, runs in withAccess after a push to a
// missing name was authorized and before the repository is created.
var testHookBeforePushCreate func()

// isBearer reports whether the Authorization header value hdr carries a
// bearer token. The scheme is case-insensitive, as parseAuthHdr reads it.
func isBearer(hdr string) bool {
	scheme, _, ok := strings.Cut(hdr, " ")
	return ok && strings.EqualFold(scheme, "bearer")
}

// maxPathAncestors bounds how many parent directories pathAncestors lists.
// Each costs a copy of the path, so listing every one would make the work of
// a request quadratic in its path length. The parents kept are the ones
// nearest the root, where repositories are: a go get path is a repository
// followed by a package path, which may be deep. A new repository name has
// at most this many segments, so it is always among them.
const maxPathAncestors = utils.MaxRepoSegments

// pathAncestors returns p followed by its parent directories, each spelled
// as a sanitized repository name, which is what `soft jwt` mints. Only the
// maxPathAncestors parents nearest the root are listed.
func pathAncestors(p string) []string {
	ancestors := []string{p}
	// The parents nearest the root end at the first slashes of p, in order.
	var ends []int
	for i := 0; i < len(p) && len(ends) < maxPathAncestors; i++ {
		if p[i] == '/' && i > 0 {
			ends = append(ends, i)
		}
	}
	for _, end := range slices.Backward(ends) {
		if parent := utils.SanitizeRepo(p[:end]); parent != "" && parent != ancestors[len(ancestors)-1] {
			ancestors = append(ancestors, parent)
		}
	}
	return ancestors
}

// withAccess authenticates the caller, authorizes them for the requested
// repository as route needs, and serves the route's handler. A repository
// created by push-to-create is recorded in pending, which removes it if the
// push fails, or never arrives, without any ref.
func (s *gitServer) withAccess(route gitRoute, w http.ResponseWriter, orig *http.Request) {
	gr := route.newRequest(orig)
	r, err := identify(orig, gr.name)
	need := route.access(r.Method, gr.service)
	lfsJSON := need.lfs
	if refuseUnchecked(w, r, err, lfsJSON) || !requireCaller(w, r) {
		return
	}

	ctx := r.Context()
	logger := log.FromContext(ctx)
	be := s.be
	repoName := gr.name
	user := proto.UserFromContext(ctx)

	if lfsJSON && !s.cfg.LFS.Enabled {
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

	v := authorize(need, repo != nil, accessLevel, user == nil, badCredentials(err))
	if v == createThenServe {
		if verr := utils.ValidateRepo(utils.SanitizeRepo(repoName)); verr != nil {
			logger.Debug("refusing push-to-create", "repo", repoName, "err", verr)
			// git shows a text/plain error body to the user.
			http.Error(w, verr.Error(), http.StatusBadRequest)
			return
		}
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
			if testHookBeforePushCreate != nil {
				testHookBeforePushCreate()
			}
			repo, cerr = be.CreateRepository(ctx, repoName, user, proto.RepositoryOptions{Private: private})
		}
		switch {
		case errors.Is(cerr, proto.ErrUnauthorized) && user == nil:
			logger.Info("refusing anonymous push-to-create", "repo", repoName, "err", cerr)
			// As above: git sends credentials only after a 401, so ask
			// for them. A signed-in admin then creates the repository; an
			// anonymous client sees this body.
			askCredentials(w, r)
			http.Error(w, cerr.Error()+"; authenticate to create this repository by pushing", http.StatusUnauthorized)
			return
		case errors.Is(cerr, proto.ErrRepoLimitReached) || errors.Is(cerr, proto.ErrUnauthorized):
			logger.Info("refusing push-to-create", "repo", repoName, "err", cerr)
			http.Error(w, cerr.Error(), http.StatusForbidden)
			return
		case errors.Is(cerr, proto.ErrRepoExist):
			// Another request created the name since it was looked
			// up. Authorize the caller for the repository now there,
			// as if it had been there all along.
			repo, accessLevel = be.Readable(ctx, repoName, user)
			ctx = proto.WithRepositoryContext(ctx, repo)
			ctx = access.WithContext(ctx, accessLevel)
			r = r.WithContext(ctx)
			v = authorize(need, repo != nil, accessLevel, user == nil, false)
			if v == createThenServe {
				// The name was freed again before it could be looked
				// up, by a creation that failed or a removal.
				logger.Info("refusing push-to-create", "repo", repoName, "err", cerr)
				http.Error(w, cerr.Error(), http.StatusConflict)
				return
			}
		case cerr != nil:
			logger.Error("failed to create repository", "repo", repoName, "err", cerr)
			renderInternalServerError(w, r)
			return
		default:
			ctx = proto.WithRepositoryContext(ctx, repo)
			r = r.WithContext(ctx)
			s.pending.add(ctx, repoName, repo, gr.dir)
		}
	}

	switch v {
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

	// A mirror sync would undo any ref a push changes, so refuse the
	// push, ref advertisement included, before git asks for the pack.
	// Only a caller allowed to write gets here, so the answer tells
	// them nothing they could not read from the repository already.
	if gr.service == git.ReceivePackService && repo != nil && repo.IsMirror() {
		logger.Info("refusing push to a mirror", "repo", repoName)
		// git shows a text/plain error body to the user.
		http.Error(w, proto.ErrMirrorReadOnly.Error(), http.StatusForbidden)
		return
	}

	ctx, done, waited := s.pending.begin(ctx, repoName)
	if waited {
		// The repository was being removed, so repo and accessLevel
		// may describe one that is gone. Start over, as a request
		// arriving now would.
		s.withAccess(route, w, orig)
		return
	}
	if done != nil {
		defer done()
		r = r.WithContext(ctx)
	}

	gr.user, gr.repo, gr.level = user, repo, accessLevel
	route.handler(s, w, r, gr)
}

//nolint:revive
func (s *gitServer) serviceRpc(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	ctx := r.Context()
	cfg := s.cfg
	logger := log.FromContext(ctx)
	service, dir, repoName := gr.service, gr.dir, gr.name

	// git http-backend answers 415 to any other content type.
	if !hasMediaType(r, fmt.Sprintf("application/x-%s-request", service)) {
		renderStatus(w, http.StatusUnsupportedMediaType)
		return
	}

	// Fetches and pushes are counted on their ref advertisement, in
	// getInfoRefs, not here: one fetch may send several upload-pack RPCs, and
	// one push may send a probe RPC before the real one.

	// Open a gzip body before writing any header, so a body that is not
	// gzip gets a client error rather than a 200 followed by an error page.
	var reader io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			logger.Debugf("bad gzip request body: %v", err)
			if service == git.ReceivePackService {
				markPushFailed(ctx)
			}
			renderStatus(w, http.StatusBadRequest)
			return
		}
		defer gz.Close() //nolint: errcheck
		reader = gz
	}

	// git reads the request while it writes the response: receive-pack
	// reports on each ref as it goes, and upload-pack sends progress. An
	// HTTP/1.x handler cannot read the body once it has started the response
	// unless full duplex is on. HTTP/2 is always full duplex, so the call is
	// a no-op there; a writer that cannot do it at all returns
	// ErrNotSupported, which is fine to ignore.
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		logger.Debugf("failed to enable full duplex: %v", err)
	}

	hdrNocache(w)
	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", service))
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

	user := gr.user
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

	// The service waits for its read of the body to end, so the body is not
	// read after this handler returns or while the gzip reader is closed.
	// The read is not cut short with a read deadline on the connection,
	// which could cancel the context of this and later requests on it.
	cmd.Stdin = reader
	cmd.JoinStdin = true
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

func (s *gitServer) getInfoRefs(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	ctx := r.Context()
	cfg := s.cfg
	logger := log.FromContext(ctx)
	dir, repoName, file, service := gr.dir, gr.name, gr.file, gr.service
	protocol := r.Header.Get("Git-Protocol")

	if !isSmartService(service) {
		// The router refuses these before authorization; this only keeps a
		// routing mistake from running git with no service.
		refuseDumbHTTP(w, r)
		return
	}

	var version int
	for _, p := range strings.Split(protocol, ":") {
		if strings.HasPrefix(p, "version=") {
			if v, _ := strconv.Atoi(p[8:]); v > version {
				version = v
			}
		}
	}

	// The advertisement is streamed to the client as git writes it,
	// rather than held in memory: a repository with many refs has a large
	// one. The header goes out with git's first output, so a git that
	// fails before writing anything still gets an error status.
	refs := &advertisement{w: w, start: func() {
		hdrNocache(w)
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
		w.WriteHeader(http.StatusOK)
		if version < 2 {
			git.WritePktline(w, "# service="+service.String()) //nolint: errcheck
		}
	}}
	cmd := git.ServiceCommand{
		Stdout: refs,
		Dir:    dir,
		Args:   []string{"--stateless-rpc", "--advertise-refs"},
	}

	user := gr.user
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

	if err := service.Handler(ctx, cmd); err != nil {
		if refs.started {
			// The status is already sent; the client sees the
			// advertisement cut short.
			logger.Debugf("ref advertisement cut short: %v", err)
			return
		}
		switch {
		case errors.Is(err, context.Canceled):
			// The client went away before git was done.
			logger.Debugf("service cancelled: %v", err)
		case errors.Is(err, git.ErrInvalidRepo):
			renderNotFound(w, r)
		default:
			logger.Errorf("failed to advertise refs: %v", err)
			renderInternalServerError(w, r)
		}
		return
	}

	// A fetch or push asks for its advertisement once, so count it here
	// rather than per RPC. A fetch may send several upload-pack RPCs
	// (ls-refs and fetch in protocol v2, one per negotiation round). A
	// push whose pack is larger than http.postBuffer first sends a probe
	// receive-pack RPC holding only a flush packet, then the real one.
	// A push with nothing to send sends no RPC, and is still counted,
	// as over SSH.
	switch service {
	case git.UploadPackService:
		gitHttpUploadCounter.WithLabelValues(repoName, file).Inc()
	case git.ReceivePackService:
		gitHttpReceiveCounter.WithLabelValues(repoName).Inc()
	}

	refs.begin()
}

// advertisement passes git's ref advertisement through to the response,
// calling start before the first byte.
type advertisement struct {
	w       io.Writer
	start   func()
	started bool
}

// begin calls start, once.
func (a *advertisement) begin() {
	if !a.started {
		a.started = true
		a.start()
	}
}

// Write implements io.Writer.
func (a *advertisement) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	a.begin()
	return a.w.Write(p)
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

// HTTP error response handling functions

// renderMethodNotAllowed answers 405 and lists the allowed methods.
func renderMethodNotAllowed(w http.ResponseWriter, r *http.Request, allowed []string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	renderStatus(w, http.StatusMethodNotAllowed)
}

func renderNotFound(w http.ResponseWriter, r *http.Request) {
	renderStatus(w, http.StatusNotFound)
}

func renderUnauthorized(w http.ResponseWriter, r *http.Request) {
	renderStatus(w, http.StatusUnauthorized)
}

func renderForbidden(w http.ResponseWriter, r *http.Request) {
	renderStatus(w, http.StatusForbidden)
}

func renderInternalServerError(w http.ResponseWriter, r *http.Request) {
	renderStatus(w, http.StatusInternalServerError)
}

// Header writing functions

func hdrNocache(w http.ResponseWriter) {
	w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
}
