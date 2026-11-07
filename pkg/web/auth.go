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
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/semaphore"
)

// authenticate authenticates the user from the request. repo is the
// requested repository name, used to check bearer token audiences.
func authenticate(r *http.Request, repo string) (proto.User, error) {
	// Prefer the Authorization header
	user, err := parseAuthHdr(r, repo)
	if err != nil || user == nil {
		if errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrInvalidPassword) {
			return nil, err
		}
		return nil, proto.ErrUserNotFound
	}

	return user, nil
}

// ErrInvalidPassword is returned when the password is invalid.
var ErrInvalidPassword = errors.New("invalid password")

func parseUsernamePassword(ctx context.Context, client, username, password string) (proto.User, error) {
	logger := log.FromContext(ctx)
	be := backend.FromContext(ctx)

	if username != "" && password != "" {
		// Try to authenticate using access token as the password first:
		// the lookup is a cheap hash that does not depend on the username,
		// and token clients then never pay for bcrypt.
		user, err := be.UserByAccessToken(ctx, password)
		if err == nil {
			return user, nil
		}

		if user := checkPassword(ctx, client, username, password); user != nil {
			return user, nil
		}

		// Clients that follow the GitHub convention send the access token
		// as the username, so the username is logged only when it is a
		// valid one: tokens ("ss_...") and JWTs never are. The client
		// address is already on the request logger.
		if utils.ValidateUsername(username) == nil {
			logger = logger.With("username", username)
		}
		logger.Info("invalid password or token", "err", err)
		return nil, ErrInvalidPassword
	} else if username != "" {
		// Try to authenticate using access token as the username
		// The username is the access token here, so it is not logged.
		logger.Debug("trying to authenticate using access token as username")
		user, err := be.UserByAccessToken(ctx, username)
		if err == nil {
			return user, nil
		}

		logger.Info("failed to get user", "err", err)
		return nil, ErrInvalidToken
	}

	return nil, proto.ErrUserNotFound
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
// address, then waits for a free slot in passwordChecks, and fails if ctx
// ends first.
func checkPassword(ctx context.Context, client, username, password string) proto.User {
	if utils.ValidateUsername(username) != nil {
		return nil
	}

	var hash string
	user, err := backend.FromContext(ctx).User(ctx, username)
	if err == nil && user != nil {
		hash = user.Password()
	}

	release, err := clientChecks.acquire(ctx, client)
	if err != nil {
		return nil
	}
	defer release()

	if err := passwordChecks.Acquire(ctx, 1); err != nil {
		return nil
	}
	defer passwordChecks.Release(1)

	// An empty hash never verifies, so user is set when this succeeds.
	if !verifyPassword(password, hash) {
		return nil
	}
	return user
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

func parseAuthHdr(r *http.Request, repo string) (proto.User, error) {
	// Check for auth header
	header := r.Header.Get("Authorization")
	if header == "" {
		return nil, ErrInvalidHeader
	}

	ctx := r.Context()
	logger := log.FromContext(ctx).WithPrefix("http.auth")
	be := backend.FromContext(ctx)

	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return nil, errors.New("invalid authorization header")
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
			logger.Info("failed to get user", "err", err)
			return nil, err
		}

		return user, nil
	case "bearer":
		claims, err := parseJWT(ctx, parts[1], repo)
		if err != nil {
			return nil, err
		}

		// Find the user
		parts := strings.SplitN(claims.Subject, "#", 2)
		if len(parts) != 2 {
			logger.Error("invalid jwt subject", "subject", claims.Subject)
			return nil, errors.New("invalid jwt subject")
		}

		user, err := be.User(ctx, parts[0])
		if err != nil {
			logger.Error("failed to get user", "err", err)
			return nil, err
		}

		expectedSubject := fmt.Sprintf("%s#%d", user.Username(), user.ID())
		if expectedSubject != claims.Subject {
			logger.Error("invalid jwt subject", "subject", claims.Subject, "expected", expectedSubject)
			return nil, errors.New("invalid jwt subject")
		}

		return user, nil
	default:
		username, password, ok := r.BasicAuth()
		if !ok {
			return nil, ErrInvalidHeader
		}

		return parseUsernamePassword(ctx, clientAddr(r), username, password)
	}
}

// ErrInvalidToken is returned when a token is invalid.
var ErrInvalidToken = errors.New("invalid token")

// parseJWT validates a bearer token whose audience must name the requested
// repository, so the answer does not depend on whether it exists.
func parseJWT(ctx context.Context, bearer, repo string) (*jwt.RegisteredClaims, error) {
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("http.auth")
	kp, err := config.KeyPair(cfg)
	if err != nil {
		return nil, err
	}

	token, err := jwt.ParseWithClaims(bearer, &jwt.RegisteredClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, errors.New("invalid signing method")
		}

		return kp.CryptoPublicKey(), nil
	},
		jwt.WithIssuer(cfg.HTTP.PublicURL),
		jwt.WithIssuedAt(),
		jwt.WithAudience(repo),
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
