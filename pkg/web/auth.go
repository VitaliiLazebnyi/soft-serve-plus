package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/golang-jwt/jwt/v5"
)

// authenticate authenticates the user from the request. A bearer token must
// name one of repos in its audience, and its audience is returned; it is nil
// for other credentials, which are not scoped to repositories. The error is
// ErrInvalidToken or ErrInvalidPassword for bad credentials, errAuthUnavailable
// when the credentials could not be checked, and proto.ErrUserNotFound
// otherwise.
func authenticate(r *http.Request, repos ...string) (proto.User, []string, error) {
	// Prefer the Authorization header
	user, audience, err := parseAuthHdr(r, repos)
	if err != nil || user == nil {
		if errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrInvalidPassword) || errors.Is(err, errAuthUnavailable) {
			return nil, nil, err
		}
		return nil, nil, proto.ErrUserNotFound
	}

	return user, audience, nil
}

// errAuthUnavailable is returned when credentials could not be checked
// because a lookup failed, as when the database is down. The caller sent
// credentials that may well be valid, so the request is neither anonymous
// nor a failed login: answering either way would make git discard a good
// stored credential, or serve the caller as anonymous.
var errAuthUnavailable = errors.New("authentication unavailable")

// lookupFailed reports whether err, from a user or access token lookup, is
// a server fault rather than a missing user or a bad or expired token.
func lookupFailed(err error) bool {
	return err != nil && !errors.Is(err, proto.ErrUserNotFound) && !errors.Is(err, proto.ErrTokenExpired)
}

// authUnavailable logs a failed lookup as the server error it is, unless the
// client went away and canceled it, and returns it as errAuthUnavailable.
func authUnavailable(ctx context.Context, logger *log.Logger, err error) error {
	if ctx.Err() != nil {
		logger.Debug("credential lookup canceled", "err", err)
	} else {
		logger.Error("failed to look up credentials", "err", err)
	}
	return fmt.Errorf("%w: %w", errAuthUnavailable, err)
}

// ErrInvalidPassword is returned for HTTP Basic credentials that hold no
// valid access token. There is no password login, so any other password is
// refused without being checked against anything.
var ErrInvalidPassword = errors.New("invalid password")

// parseUsernamePassword authenticates HTTP Basic credentials. There is no
// password login: a Basic login must carry an access token. It accepts:
//
//   - an access token as the password, with any username or none: the token
//     decides the user, as on GitHub, so placeholders such as "git" or
//     "x-access-token" work whoever has that name, and the answer does not
//     reveal whether the name is registered;
//   - an access token as the username, with any password or none, the way
//     GitHub clients send it (e.g. "<token>:x-oauth-basic").
//
// Anything else is refused after at most two token lookups, which are
// hashes, so a failed login costs no bcrypt and waits for nothing.
func parseUsernamePassword(ctx context.Context, username, password string) (proto.User, error) {
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
		// Access token as the password. The token's owner is the user,
		// whatever the username, which is not looked up.
		// When both fields are valid tokens of different users, the password's wins.
		owner, err := be.UserByAccessToken(ctx, password)
		if err == nil {
			return owner, nil
		}
		if lookupFailed(err) {
			return nil, authUnavailable(ctx, logger, err)
		}
	}

	if username != "" {
		// Access token as the username, with any password. The username is
		// the access token here, so it is not logged.
		owner, err := be.UserByAccessToken(ctx, username)
		if err == nil {
			return owner, nil
		}
		if lookupFailed(err) {
			return nil, authUnavailable(ctx, logger, err)
		}
		if password == "" {
			logger.Info("invalid access token", "err", err)
			return nil, ErrInvalidToken
		}
	}

	logger.Info("invalid password or token")
	return nil, ErrInvalidPassword
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
			if lookupFailed(err) {
				return nil, nil, authUnavailable(ctx, logger, err)
			}
			// An unknown or expired token is a failed login, not an
			// anonymous request: answering it as anonymous would let it
			// through wherever anonymous users may go.
			logger.Info("invalid token", "err", err)
			return nil, nil, ErrInvalidToken
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
		// A name that is not a valid username names no user; the
		// lookup would refuse it with an error that is not
		// proto.ErrUserNotFound.
		parts := strings.SplitN(claims.Subject, "#", 2)
		if len(parts) != 2 || utils.ValidateUsername(parts[0]) != nil {
			logger.Info("invalid jwt subject", "subject", claims.Subject)
			return nil, nil, ErrInvalidToken
		}

		user, err := be.User(ctx, parts[0])
		if err != nil {
			if lookupFailed(err) {
				return nil, nil, authUnavailable(ctx, logger, err)
			}
			logger.Info("jwt user not found", "subject", claims.Subject)
			return nil, nil, ErrInvalidToken
		}

		expectedSubject := jwk.Subject(user)
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

		user, err := parseUsernamePassword(ctx, username, password)
		return user, nil, err
	}
}

// ErrInvalidToken is returned when a token is invalid.
var ErrInvalidToken = errors.New("invalid token")

// parseJWT validates a bearer token whose audience must name one of repos,
// so the answer does not depend on whether they exist. The token must expire.
func parseJWT(ctx context.Context, bearer string, repos []string) (*jwt.RegisteredClaims, error) {
	claims, err := jwk.Verify(config.FromContext(ctx), bearer, repos)
	if err != nil {
		logger := log.FromContext(ctx).WithPrefix("http.auth")
		if errors.Is(err, jwk.ErrHostKey) {
			// The server cannot verify any token without its host key. That
			// is a server fault, so log it as one; the caller still sent a
			// token, so answer as for a bad one rather than as anonymous.
			logger.Error("cannot verify jwt", "err", err)
			return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
		}
		logger.Info("failed to parse jwt", "err", err)
		return nil, ErrInvalidToken
	}
	return claims, nil
}
