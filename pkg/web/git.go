package web

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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
	server *server
}

var _ http.Handler = gitRoute{}

// gitHandler serves a request of a git route once withAccess has let it
// through. gr is never nil, and gr.repo is the repository the caller may
// read.
type gitHandler func(s *server, w http.ResponseWriter, r *http.Request, gr *gitRequest)

// gitRequest is what a git route knows about a request. The route fills in
// the repository's name, directory, the path inside it, the git service and
// the request's logger; withAccess adds the caller, the repository and the
// caller's access level.
type gitRequest struct {
	// logger is the request's logger; see server.requestLogger.
	logger *log.Logger
	// name is the sanitized repository name.
	name string
	// dir is the repository's directory.
	dir string
	// file is the request path inside the repository, such as "info/refs".
	file string
	// service is the git service named by the RPC path or, on the ref
	// advertisement, by ?service=; empty on other routes.
	service gitserve.Service
	user    proto.User
	// repo is nil until withAccess finds the caller may read it.
	repo  proto.Repository
	level access.AccessLevel
	// pushFailed is set by a receive-pack request that failed; see
	// pushCreates.
	pushFailed bool
}

// newRequest returns what the route knows about r.
func (g gitRoute) newRequest(r *http.Request) *gitRequest {
	vars := mux.Vars(r)
	repo := vars["repo"]
	gr := &gitRequest{
		logger: g.server.requestLogger(r),
		file:   strings.TrimPrefix(r.URL.Path, "/"+repo+"/"),
		// The RPC route carries the service in its path; the
		// caller-supplied ?service= only means something on the ref
		// advertisement.
		service: gitserve.Service(vars["service"]),
		name:    proto.SanitizeRepo(repo),
	}
	if gr.service == "" && gr.file == "info/refs" {
		gr.service = getServiceType(r)
	}
	gr.dir = hooks.RepoPath(g.server.cfg, gr.name)
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
func (g gitRoute) access(method string, service gitserve.Service) routeAccess {
	a := routeAccess{lfs: g.lfs, write: slices.Contains(g.write, method)}
	if g.service && service == gitserve.ReceivePackService {
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
	gitHTTPReceiveCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "http",
		Name:      "git_receive_pack_total",
		Help:      "The total number of git push requests",
	}, []string{"repo"})

	gitHTTPUploadCounter = promauto.NewCounterVec(prometheus.CounterOpts{
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
		vars["repo"] = proto.SanitizeRepo(vars["repo"])
		next.ServeHTTP(w, mux.SetURLVars(r, vars))
	})
}

// gitController routes git services and go get.
func (s *server) gitController(r *mux.Router) {
	basePrefix := "/{repo:.*}"

	goGet := withParams(s.withCaller(s.goGetHandler))

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
		route.server = s
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
func isSmartService(service gitserve.Service) bool {
	return service == gitserve.UploadPackService || service == gitserve.ReceivePackService
}

var gitRoutes = []gitRoute{
	// Git services
	// These handlers don't authenticate or authorize the caller: gitRoute
	// wraps each of them in withAccess.
	{
		method:  []string{http.MethodPost},
		handler: (*server).serviceRPC,
		path:    "/{service:(?:git-upload-archive|git-upload-pack|git-receive-pack)$}",
		service: true,
	},
	{
		method:  []string{http.MethodGet},
		handler: (*server).getInfoRefs,
		path:    "/info/refs",
		service: true,
	},
	// Git LFS
	// https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md
	{
		// An upload batch needs write access; serviceLfsBatch checks
		// that, since the operation is in the body.
		method:  []string{http.MethodPost},
		handler: (*server).serviceLfsBatch,
		path:    "/info/lfs/objects/batch",
		lfs:     true,
	},
	{
		// Git LFS basic object handler
		method:  []string{http.MethodGet, http.MethodPut},
		handler: (*server).serviceLfsBasic,
		path:    "/info/lfs/objects/basic/{oid:[0-9a-f]{64}$}",
		lfs:     true,
		write:   []string{http.MethodPut},
	},
	{
		method:  []string{http.MethodPost},
		handler: (*server).serviceLfsBasicVerify,
		path:    "/info/lfs/objects/basic/verify",
		lfs:     true,
	},
	// Git LFS locks
	// https://github.com/git-lfs/git-lfs/blob/main/docs/api/locking.md
	{
		// GET lists locks, which only needs read access.
		method:  []string{http.MethodPost, http.MethodGet},
		handler: (*server).serviceLfsLocks,
		path:    "/info/lfs/locks",
		lfs:     true,
		write:   []string{http.MethodPost},
	},
	{
		method:  []string{http.MethodPost},
		handler: (*server).serviceLfsLocksVerify,
		path:    "/info/lfs/locks/verify",
		lfs:     true,
		write:   []string{http.MethodPost},
	},
	{
		method:  []string{http.MethodPost},
		handler: (*server).serviceLfsLocksDelete,
		path:    "/info/lfs/locks/{lock_id:[0-9]+}/unlock",
		lfs:     true,
		write:   []string{http.MethodPost},
	},
}

func askCredentials(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Basic realm="Git", charset="UTF-8", Token, Bearer`)
	w.Header().Set("LFS-Authenticate", `Basic realm="Git LFS", charset="UTF-8", Token, Bearer`)
}

// caller is who sent a request, as identify found.
type caller struct {
	// user is the authenticated user, or nil for an anonymous caller.
	user proto.User
	// audience lists the repositories the caller's bearer token names. It
	// is nil when the caller did not authenticate with a bearer token, so
	// is not limited to particular repositories.
	audience []string
}

// identify returns the authenticated caller of r and the authentication
// error, if any. A bearer token must name one of repos in its audience.
// logger is the request's logger.
func (s *server) identify(r *http.Request, logger *log.Logger, repos ...string) (caller, error) {
	// authenticate logs why credentials failed, and returns only bad
	// credentials, errAuthUnavailable or proto.ErrUserNotFound.
	user, audience, err := s.authenticate(r, logger, repos...)
	if user != nil {
		logger.Debug("authenticated", "username", user.Username())
	}
	return caller{user: user, audience: audience}, err
}

// userFor returns the caller to authorize for repo: the authenticated
// user, or nil (anonymous) when the user authenticated with a bearer token
// that does not name repo.
func (c caller) userFor(repo string) proto.User {
	if c.audience != nil && !slices.Contains(c.audience, proto.SanitizeRepo(repo)) {
		return nil
	}
	return c.user
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
func refuseUnchecked(w http.ResponseWriter, r *http.Request, logger *log.Logger, err error, lfsJSON bool) bool {
	if !errors.Is(err, errAuthUnavailable) {
		return false
	}
	renderUnavailable(w, r, logger, lfsJSON)
	return true
}

// renderUnavailable answers 503 Service Unavailable with a Retry-After and
// no credentials challenge, so git keeps a stored credential; an LFS
// request gets the LFS JSON error.
func renderUnavailable(w http.ResponseWriter, r *http.Request, logger *log.Logger, lfsJSON bool) {
	w.Header().Set("Retry-After", authRetryAfter)
	if lfsJSON {
		renderJSON(w, logger, http.StatusServiceUnavailable, lfs.ErrorResponse{Message: "service unavailable, try again later"})
	} else {
		renderStatus(w, http.StatusServiceUnavailable)
	}
}

// lookupUnavailable answers a request whose repository or access could not
// be looked up, as when the database is down, with renderUnavailable: the
// repository may well exist and be readable, so answering "not found" or
// asking for credentials would mislead the client. The fault is logged,
// at Debug when the client went away and canceled it.
func lookupUnavailable(w http.ResponseWriter, r *http.Request, logger *log.Logger, repo string, err error, lfsJSON bool) {
	if r.Context().Err() != nil {
		logger.Debug("repository lookup canceled", "repo", repo, "err", err)
	} else {
		logger.Error("failed to look up repository", "repo", repo, "err", err)
	}
	renderUnavailable(w, r, logger, lfsJSON)
}

// denyLFSWrite answers an LFS write by a caller without write access. An
// anonymous caller gets 401 with a credentials challenge, since git-lfs only
// asks for credentials after a 401 and otherwise gives up on the push. A
// signed-in caller gets 403.
func denyLFSWrite(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	if gr.user == nil {
		askCredentials(w, r)
		renderJSON(w, gr.logger, http.StatusUnauthorized, lfs.ErrorResponse{Message: "credentials needed"})
		return
	}
	renderJSON(w, gr.logger, http.StatusForbidden, lfs.ErrorResponse{Message: "write access required"})
}

// denyRead answers gr's caller, who may not read the repository. Missing
// and unreadable repositories get the same answer so neither is revealed.
func denyRead(w http.ResponseWriter, r *http.Request, gr *gitRequest, err error, lfsJSON bool) {
	switch {
	case badCredentials(err) && lfsJSON:
		renderJSON(w, gr.logger, http.StatusForbidden, lfs.ErrorResponse{Message: "bad credentials"})
	case badCredentials(err):
		renderForbidden(w, r)
	case gr.user == nil && lfsJSON:
		askCredentials(w, r)
		renderJSON(w, gr.logger, http.StatusUnauthorized, lfs.ErrorResponse{Message: "credentials needed"})
	case gr.user == nil:
		askCredentials(w, r)
		renderUnauthorized(w, r)
	case lfsJSON:
		renderJSON(w, gr.logger, http.StatusNotFound, lfs.ErrorResponse{Message: "repository not found"})
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

// requireCaller rejects anonymous requests, those whose user is nil, when
// keyless access is disabled. When the setting cannot be read, the request
// is answered as unavailable (see lookupUnavailable) rather than asked for
// credentials. logger is the request's logger.
func (s *server) requireCaller(w http.ResponseWriter, r *http.Request, logger *log.Logger, user proto.User, lfsJSON bool) bool {
	if user != nil {
		return true
	}
	ctx := r.Context()
	allow, err := s.be.AllowKeylessSetting(ctx)
	if err != nil {
		lookupUnavailable(w, r, logger, "", err, lfsJSON)
		return false
	}
	if !allow {
		askCredentials(w, r)
		renderUnauthorized(w, r)
		return false
	}
	return true
}

// callerHandler serves a request whose caller withCaller identified.
// logger is the request's logger.
type callerHandler func(w http.ResponseWriter, r *http.Request, logger *log.Logger, c caller)

// withCaller authenticates the caller without authorizing a repository.
// Handlers behind it must check access to any repository they reveal, as
// c.userFor the repository: the request is for a path inside a repository,
// so a bearer token is accepted when it names the path or any directory
// above it, but it only stands for the caller on the repositories it names.
//
// The list of those directories is built only for a bearer token, the only
// credential scoped to repositories.
func (s *server) withCaller(next callerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logger := s.requestLogger(r)
		var repos []string
		if isBearer(r.Header.Get("Authorization")) {
			repos = pathAncestors(mux.Vars(r)["repo"])
		}
		c, err := s.identify(r, logger, repos...)
		if refuseUnchecked(w, r, logger, err, false) || !s.requireCaller(w, r, logger, c.user, false) {
			return
		}

		if badCredentials(err) {
			renderForbidden(w, r)
			return
		}

		next(w, r, logger, c)
	}
}

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
const maxPathAncestors = proto.MaxRepoSegments

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
		if parent := proto.SanitizeRepo(p[:end]); parent != "" && parent != ancestors[len(ancestors)-1] {
			ancestors = append(ancestors, parent)
		}
	}
	return ancestors
}

// withAccess authenticates the caller, authorizes them for the requested
// repository as route needs, and serves the route's handler. A repository
// created by push-to-create is recorded in pending, which removes it if the
// push fails, or never arrives, without any ref.
func (s *server) withAccess(route gitRoute, w http.ResponseWriter, r *http.Request) {
	gr := route.newRequest(r)
	logger := gr.logger
	c, err := s.identify(r, logger, gr.name)
	gr.user = c.user
	user := gr.user
	need := route.access(r.Method, gr.service)
	lfsJSON := need.lfs
	if refuseUnchecked(w, r, logger, err, lfsJSON) || !s.requireCaller(w, r, logger, user, lfsJSON) {
		return
	}

	ctx := r.Context()
	be := s.be
	repoName := gr.name

	if lfsJSON && !s.cfg.LFS.Enabled {
		logger.Debug("LFS is not enabled, skipping")
		renderNotFound(w, r)
		return
	}

	// repo is nil unless the caller may read it, so a private and a
	// missing repository are already indistinguishable here.
	repo, accessLevel, lerr := be.Readable(ctx, repoName, user)
	if lerr != nil {
		lookupUnavailable(w, r, logger, repoName, lerr, lfsJSON)
		return
	}

	v := authorize(need, repo != nil, accessLevel, user == nil, badCredentials(err))
	if v == createThenServe {
		if verr := proto.ValidateRepo(proto.SanitizeRepo(repoName)); verr != nil {
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
			if s.beforePushCreate != nil {
				s.beforePushCreate()
			}
			repo, cerr = be.CreateRepository(ctx, repoName, user, proto.RepositoryOptions{Private: private})
		}
		switch {
		case errors.Is(cerr, proto.ErrLookupFailed):
			lookupUnavailable(w, r, logger, repoName, cerr, lfsJSON)
			return
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
			repo, accessLevel, lerr = be.Readable(ctx, repoName, user)
			if lerr != nil {
				lookupUnavailable(w, r, logger, repoName, lerr, lfsJSON)
				return
			}
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
			s.pending.add(repoName, repo, user)
		}
	}

	switch v {
	case denyUnreadable:
		denyRead(w, r, gr, err, lfsJSON)
		return
	case forbid:
		if lfsJSON {
			// Only bad credentials forbid an LFS request.
			renderJSON(w, logger, http.StatusForbidden, lfs.ErrorResponse{Message: "bad credentials"})
		} else {
			renderForbidden(w, r)
		}
		return
	case unauthorized:
		askCredentials(w, r)
		renderUnauthorized(w, r)
		return
	case forbidLFSWrite, unauthorizedLFS:
		denyLFSWrite(w, r, gr)
		return
	}

	// A mirror sync would undo any ref a push changes, so refuse the
	// push, ref advertisement included, before git asks for the pack.
	// Only a caller allowed to write gets here, so the answer tells
	// them nothing they could not read from the repository already.
	if gr.service == gitserve.ReceivePackService && repo != nil && repo.IsMirror() {
		logger.Info("refusing push to a mirror", "repo", repoName)
		// git shows a text/plain error body to the user.
		http.Error(w, proto.ErrMirrorReadOnly.Error(), http.StatusForbidden)
		return
	}

	done, waited := s.pending.begin(repoName)
	if waited {
		// The repository was being removed, so repo and accessLevel
		// may describe one that is gone. Start over, as a request
		// arriving now would.
		s.withAccess(route, w, r)
		return
	}
	if done != nil {
		defer func() { done(gr.pushFailed) }()
	}

	gr.repo, gr.level = repo, accessLevel
	route.handler(s, w, r, gr)
}

func (s *server) serviceRPC(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	ctx := r.Context()
	cfg := s.cfg
	logger := gr.logger
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
			if service == gitserve.ReceivePackService {
				gr.pushFailed = true
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

	cmd := gitserve.ServiceCommand{
		Dir:    dir,
		Logger: logger,
	}

	switch service {
	case gitserve.UploadPackService, gitserve.ReceivePackService:
		cmd.Args = append(cmd.Args, "--stateless-rpc")
	}

	// Only receive-pack runs hooks, so only it gets their environment, and
	// a file its post-receive hook writes the updated refs to.
	cmd.Env = hooks.ProtocolEnv(version)
	var refs *hooks.PushRefs
	if service == gitserve.ReceivePackService {
		refs = newPushRefs(cfg, logger, repoName)
		if refs != nil {
			defer removePushRefs(logger, refs)
		}
		cmd.Env = hooks.Env(cfg, hooks.Request{
			Transport: hooks.TransportHTTP,
			Repo:      repoName,
			User:      gr.user,
			Protocol:  version,
			Refs:      refs,
		})
	}

	// The service waits for its read of the body to end, so the body is not
	// read after this handler returns or while the gzip reader is closed.
	// The read is not cut short with a read deadline on the connection,
	// which could cancel the context of this and later requests on it.
	cmd.Stdin = reader
	cmd.JoinStdin = true
	cmd.Stdout = w

	err := s.runService(ctx, service, cmd)
	if service == gitserve.ReceivePackService {
		if err == nil {
			if err := gitserve.EnsureDefaultBranch(ctx, cmd.Dir); err != nil {
				logger.Errorf("failed to ensure default branch: %s", err)
			}
		}
		// The refs a push updated are already updated, whether or not
		// the client stayed to the end, so their work is done either way.
		s.afterPush(ctx, logger, gr.repo, gr.user, refs)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// The client went away before git was done.
			logger.Debugf("service cancelled: %v", err)
			return
		}
		logger.Errorf("failed to handle service: %v", err)
		if service == gitserve.ReceivePackService {
			gr.pushFailed = true
		}
		return
	}
}

// newPushRefs returns the file a push's post-receive hook writes its refs
// to, or nil, after logging why, when it cannot be created: the push then
// goes ahead without sending its webhooks.
func newPushRefs(cfg *config.Config, logger *log.Logger, repo string) *hooks.PushRefs {
	refs, err := hooks.NewPushRefs(cfg)
	if err != nil {
		logger.Error("push webhooks will not be sent", "repo", repo, "err", err)
		return nil
	}
	return refs
}

// removePushRefs removes refs, logging a failure.
func removePushRefs(logger *log.Logger, refs *hooks.PushRefs) {
	if err := refs.Remove(); err != nil {
		logger.Warn("failed to remove the push refs file", "err", err)
	}
}

// afterPush hands the refs a push to repo by user updated to the backend.
func (s *server) afterPush(ctx context.Context, logger *log.Logger, repo proto.Repository, user proto.User, refs *hooks.PushRefs) {
	if refs == nil || repo == nil {
		return
	}
	updated, err := refs.Refs()
	if err != nil {
		logger.Error("failed to read the pushed refs", "repo", repo.Name(), "err", err)
	}
	s.be.AfterPush(ctx, repo, user, updated)
}

func (s *server) getInfoRefs(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	logger := gr.logger
	ctx := r.Context()
	dir, repoName, file, service := gr.dir, gr.name, gr.file, gr.service
	protocol := r.Header.Get("Git-Protocol")

	if !isSmartService(service) {
		// The router refuses these before authorization; this only keeps a
		// routing mistake from running git with no service.
		refuseDumbHTTP(w, r)
		return
	}

	// Only before protocol v2 does the advertisement start with the
	// service line, so it follows the version git serves: the one
	// hooks.ProtocolEnv passes it. A header that asks for no version git
	// knows gets v0.
	version, _ := hooks.ProtocolVersion(protocol)

	// The advertisement is streamed to the client as git writes it,
	// rather than held in memory: a repository with many refs has a large
	// one. The header goes out with git's first output, so a git that
	// fails before writing anything still gets an error status.
	refs := &advertisement{w: w, start: func() {
		hdrNocache(w)
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
		w.WriteHeader(http.StatusOK)
		if version < 2 {
			gitserve.WritePktline(w, "# service="+service.String()) //nolint: errcheck
		}
	}}
	cmd := gitserve.ServiceCommand{
		Stdout: refs,
		Dir:    dir,
		Args:   []string{"--stateless-rpc", "--advertise-refs"},
		Logger: logger,
	}

	// A ref advertisement runs no hooks, for receive-pack either.
	cmd.Env = hooks.ProtocolEnv(protocol)

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
		case errors.Is(err, gitserve.ErrInvalidRepo):
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
	case gitserve.UploadPackService:
		gitHTTPUploadCounter.WithLabelValues(repoName, file).Inc()
	case gitserve.ReceivePackService:
		gitHTTPReceiveCounter.WithLabelValues(repoName).Inc()
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
func getServiceType(r *http.Request) gitserve.Service {
	service := r.URL.Query().Get("service")
	if !strings.HasPrefix(service, "git-") {
		return ""
	}

	return gitserve.Service(service)
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
