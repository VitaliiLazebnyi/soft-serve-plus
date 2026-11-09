package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/semaphore"
)

// authenticate authenticates the user from the request. A bearer token must
// name one of repos in its audience, and its audience is returned; it is nil
// for other credentials, which are not scoped to repositories. The error is
// ErrInvalidToken or ErrInvalidPassword for bad credentials, an error
// wrapping ErrPasswordCheckBusy when the password could not be checked, and
// proto.ErrUserNotFound otherwise.
func authenticate(r *http.Request, repos ...string) (proto.User, []string, error) {
	// Prefer the Authorization header
	user, audience, err := parseAuthHdr(r, repos)
	if err != nil || user == nil {
		if errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrInvalidPassword) ||
			errors.Is(err, ErrPasswordCheckBusy) {
			return nil, nil, err
		}
		return nil, nil, proto.ErrUserNotFound
	}

	return user, audience, nil
}

// ErrInvalidPassword is returned when the password is invalid.
var ErrInvalidPassword = errors.New("invalid password")

// ErrPasswordCheckBusy is returned when a password could not be checked
// because the client has too many checks running or waiting, or the request
// ended while it waited. The password was not found wrong, so callers must
// not answer it as bad credentials: git would then erase a correct stored
// password from the user's credential helper.
var ErrPasswordCheckBusy = errors.New("too many password checks, try again later")

// parseUsernamePassword authenticates HTTP Basic credentials. It accepts:
//
//   - a registered user's name and password;
//   - an access token as the password, with an empty username, the token
//     owner's username, or a placeholder that names no registered user (such
//     as "git" or "x-access-token");
//   - an access token as the username, with any password or none, the way
//     GitHub clients send it (e.g. "<token>:x-oauth-basic").
//
// A token sent as the password with the name of another registered user is
// refused: "alice:<bob's token>" must not log in as bob. A password that
// could not be checked because client has too many checks pending returns
// an error wrapping ErrPasswordCheckBusy.
func parseUsernamePassword(ctx context.Context, client, username, password string) (proto.User, error) {
	logger := log.FromContext(ctx)
	be := backend.FromContext(ctx)

	if username == "" && password == "" {
		return nil, proto.ErrUserNotFound
	}

	// Clients that follow the GitHub convention send the access token as
	// the username, so the username is logged only when it is a valid one:
	// tokens ("ss_...") and JWTs never are. The client address is already
	// on the request logger.
	if utils.ValidateUsername(username) == nil {
		logger = logger.With("username", username)
	}

	if password != "" {
		// Access token as the password. The lookup is a hash that does not
		// depend on the username, so token clients never pay for bcrypt.
		owner, err := be.UserByAccessToken(ctx, password)
		if err == nil {
			ok, err := tokenMatchesUsername(ctx, owner, username)
			if err != nil {
				logger.Info("failed to look up the user named with an access token", "err", err)
				return nil, ErrInvalidPassword
			}
			if !ok {
				logger.Info("access token does not belong to the named user")
				return nil, ErrInvalidPassword
			}
			return owner, nil
		}

		// The password of a registered user. checkPassword does the same
		// bcrypt work whether or not the user exists or has a password.
		if username != "" {
			user, err := checkPassword(ctx, client, username, password)
			if err != nil {
				logger.Info("password check throttled", "err", err)
				return nil, err
			}
			if user != nil {
				return user, nil
			}
		}
	}

	if username != "" {
		// Access token as the username, with any password. The username is
		// the access token here, so it is not logged.
		owner, err := be.UserByAccessToken(ctx, username)
		if err == nil {
			return owner, nil
		}
		if password == "" {
			logger.Info("invalid access token", "err", err)
			return nil, ErrInvalidToken
		}
	}

	logger.Info("invalid password or token")
	return nil, ErrInvalidPassword
}

// tokenMatchesUsername reports whether an access token owned by owner may be
// used with username as the HTTP Basic username: it may when username is
// empty, is owner's name, or is a placeholder that names no registered user.
// A failed user lookup returns an error, so the login fails closed.
func tokenMatchesUsername(ctx context.Context, owner proto.User, username string) (bool, error) {
	if username == "" || strings.EqualFold(username, owner.Username()) {
		return true, nil
	}
	// A name that is not a valid username cannot be a registered user.
	if utils.ValidateUsername(username) != nil {
		return true, nil
	}
	named, err := backend.FromContext(ctx).User(ctx, username)
	if errors.Is(err, proto.ErrUserNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return named.ID() == owner.ID(), nil
}

// passwordChecks bounds the bcrypt comparisons running at once, so failed
// HTTP Basic logins cannot occupy every CPU. Half the processors leaves the
// rest to serving everything else.
var passwordChecks = semaphore.NewWeighted(int64(max(1, runtime.GOMAXPROCS(0)/2)))

// clientChecks lets each client address run one password check at a time,
// so one client cannot fill passwordChecks and keep other clients' logins
// waiting, and guesses from one address go no faster than bcrypt allows.
var clientChecks = newClientLimiter(maxClientPending)

// maxClientPending is how many password checks one client address may have
// running or waiting before more fail without bcrypt. Clients behind one
// proxy share an address, so it leaves room for a few logins at once.
const maxClientPending = 16

// verifyPassword is backend.VerifyPassword; tests replace it to count calls.
var verifyPassword = backend.VerifyPassword

// checkPassword returns the user named username if password is their
// password, or nil. It does the same bcrypt work whether or not the user
// exists or has a password, so the response time does not reveal which
// accounts do; a name that is not a valid username cannot be an account, so
// it is refused without bcrypt. The check runs one at a time per client
// address, then waits for a free slot in passwordChecks. If the client has
// too many checks pending, or ctx ends first, it returns an error wrapping
// ErrPasswordCheckBusy without checking the password.
func checkPassword(ctx context.Context, client, username, password string) (proto.User, error) {
	if utils.ValidateUsername(username) != nil {
		return nil, nil
	}

	var hash string
	user, err := backend.FromContext(ctx).User(ctx, username)
	if err == nil && user != nil {
		hash = user.Password()
	}

	release, err := clientChecks.acquire(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPasswordCheckBusy, err)
	}
	defer release()

	if err := passwordChecks.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPasswordCheckBusy, err)
	}
	defer passwordChecks.Release(1)

	// An empty hash never verifies, so user is set when this succeeds.
	if !verifyPassword(password, hash) {
		return nil, nil
	}
	return user, nil
}

// errClientBusy is returned when a client has too many password checks
// running or waiting.
var errClientBusy = errors.New("too many password checks from this client")

// clientLimiter runs one task at a time per key and refuses a key that
// already has maxPending tasks running or waiting. Keys are removed once
// nothing holds them, so the map only grows with concurrent clients.
type clientLimiter struct {
	maxPending int
	mu         sync.Mutex
	keys       map[string]*clientSlot
}

type clientSlot struct {
	sem     *semaphore.Weighted
	pending int
}

func newClientLimiter(maxPending int) *clientLimiter {
	return &clientLimiter{maxPending: maxPending, keys: map[string]*clientSlot{}}
}

// acquire waits until key has no other task running. It returns a release
// function, or an error if key has too many tasks pending or ctx ends first.
func (l *clientLimiter) acquire(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	slot := l.keys[key]
	if slot == nil {
		slot = &clientSlot{sem: semaphore.NewWeighted(1)}
		l.keys[key] = slot
	}
	if slot.pending >= l.maxPending {
		l.mu.Unlock()
		return nil, errClientBusy
	}
	slot.pending++
	l.mu.Unlock()

	done := func() {
		l.mu.Lock()
		slot.pending--
		if slot.pending == 0 {
			delete(l.keys, key)
		}
		l.mu.Unlock()
	}

	if err := slot.sem.Acquire(ctx, 1); err != nil {
		done()
		return nil, err
	}
	return func() {
		slot.sem.Release(1)
		done()
	}, nil
}

// clientAddr returns the host part of the request's remote address, which
// keys the per-client password check limit.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ErrInvalidHeader is returned when the authorization header is invalid.
var ErrInvalidHeader = errors.New("invalid authorization header")

func parseAuthHdr(r *http.Request, repos []string) (proto.User, []string, error) {
	// Check for auth header
	header := r.Header.Get("Authorization")
	if header == "" {
		return nil, nil, ErrInvalidHeader
	}

	ctx := r.Context()
	logger := log.FromContext(ctx).WithPrefix("http.auth")
	be := backend.FromContext(ctx)

	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return nil, nil, ErrInvalidHeader
	}

	// Log only a known scheme: the rest of the header is a credential, and
	// in a malformed header the first word can be one too.
	scheme := strings.ToLower(parts[0])
	logged := "other"
	switch scheme {
	case "token", "bearer", "basic":
		logged = scheme
	}
	logger.Debug("authorization auth header", "scheme", logged)

	switch scheme {
	case "token":
		user, err := be.UserByAccessToken(ctx, parts[1])
		if err != nil {
			// An unknown or expired token is a failed login, not an
			// anonymous request: answering it as anonymous would let it
			// through wherever anonymous users may go.
			if errors.Is(err, proto.ErrUserNotFound) || errors.Is(err, proto.ErrTokenExpired) {
				logger.Info("invalid token", "err", err)
				return nil, nil, ErrInvalidToken
			}
			logger.Error("failed to get user", "err", err)
			return nil, nil, err
		}

		return user, nil, nil
	case "bearer":
		claims, err := parseJWT(ctx, parts[1], repos)
		if err != nil {
			return nil, nil, err
		}

		// Find the user
		// A valid signature on a token whose user is gone, or was
		// replaced by another of the same name, is still a failed login.
		parts := strings.SplitN(claims.Subject, "#", 2)
		if len(parts) != 2 {
			logger.Info("invalid jwt subject", "subject", claims.Subject)
			return nil, nil, ErrInvalidToken
		}

		user, err := be.User(ctx, parts[0])
		if err != nil {
			if errors.Is(err, proto.ErrUserNotFound) {
				logger.Info("jwt user not found", "subject", claims.Subject)
				return nil, nil, ErrInvalidToken
			}
			logger.Error("failed to get user", "err", err)
			return nil, nil, err
		}

		expectedSubject := fmt.Sprintf("%s#%d", user.Username(), user.ID())
		if expectedSubject != claims.Subject {
			logger.Info("invalid jwt subject", "subject", claims.Subject, "expected", expectedSubject)
			return nil, nil, ErrInvalidToken
		}

		return user, claims.Audience, nil
	default:
		username, password, ok := r.BasicAuth()
		if !ok {
			return nil, nil, ErrInvalidHeader
		}

		user, err := parseUsernamePassword(ctx, clientAddr(r), username, password)
		return user, nil, err
	}
}

// ErrInvalidToken is returned when a token is invalid.
var ErrInvalidToken = errors.New("invalid token")

// parseJWT validates a bearer token whose audience must name one of repos,
// so the answer does not depend on whether they exist. The token must expire.
func parseJWT(ctx context.Context, bearer string, repos []string) (*jwt.RegisteredClaims, error) {
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("http.auth")
	if len(repos) == 0 {
		// jwt.WithAudience checks nothing without an audience.
		return nil, ErrInvalidToken
	}
	kp, err := config.KeyPair(cfg)
	if err != nil {
		return nil, err
	}

	pub := kp.CryptoPublicKey()
	method, err := jwk.SigningMethodFor(pub)
	if err != nil {
		return nil, err
	}

	token, err := jwt.ParseWithClaims(bearer, &jwt.RegisteredClaims{}, func(*jwt.Token) (interface{}, error) {
		return pub, nil
	},
		jwt.WithValidMethods([]string{method.Alg()}),
		jwt.WithIssuer(cfg.HTTP.PublicURL),
		jwt.WithIssuedAt(),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(repos...),
	)
	if err != nil {
		logger.Info("failed to parse jwt", "err", err)
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*jwt.RegisteredClaims)
	if !token.Valid || !ok {
		return nil, ErrInvalidToken
	}

	return claims, nil
}
