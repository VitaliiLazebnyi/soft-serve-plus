package git

import "strings"

// noReplaceObjects makes git ignore refs/replace (see serviceEnv).
const noReplaceObjects = "GIT_NO_REPLACE_OBJECTS=1"

// gitProtocolEnv is the variable git reads the protocol version from. Only
// the request sets it (see serviceEnv).
const gitProtocolEnv = "GIT_PROTOCOL"

// softServeEnvPrefix is the prefix of the variables Soft Serve passes to git
// and its hooks.
const softServeEnvPrefix = "SOFT_SERVE_"

// inheritedSoftServeEnv lists the SOFT_SERVE_* variables a git child may
// still inherit from the server's own environment. They are process-wide
// switches that say nothing about the request: debug logging, colour output
// and test hooks. Every other SOFT_SERVE_* variable a hook reads is either
// the server config, which the caller passes in full from config.Environ, or
// describes the request (repository, user, public key), which the caller
// sets for that request alone.
var inheritedSoftServeEnv = map[string]struct{}{
	"SOFT_SERVE_DEBUG":    {},
	"SOFT_SERVE_VERBOSE":  {},
	"SOFT_SERVE_NO_COLOR": {},
	"SOFT_SERVE_TESTRUN":  {},
}

// inheritedSoftServeEnvPrefixes lists prefixes of SOFT_SERVE_* variables a
// git child may inherit, for test-only switches.
var inheritedSoftServeEnvPrefixes = []string{
	"SOFT_SERVE_TEST_",
}

// serviceEnv returns the environment for a git service process: the server's
// environment without the SOFT_SERVE_* variables a caller is meant to set,
// then GIT_NO_REPLACE_OBJECTS=1, then extra.
//
// GIT_NO_REPLACE_OBJECTS makes the service and its hooks read objects as
// they are stored, ignoring refs/replace, whether or not the server's own
// environment has it yet (backend.New sets it for the process): a hook
// builds webhook payloads from commits a pusher could otherwise replace.
//
// Without this, a SOFT_SERVE_USERNAME or SOFT_SERVE_PUBLIC_KEY left in the
// server's environment would reach the hooks whenever the request does not
// set it itself (an anonymous or keyless push), and the push would be
// attributed to that user.
//
// GIT_PROTOCOL is dropped too: it is the client's to send, and every
// transport passes the version its client asked for in extra. A
// GIT_PROTOCOL in the server's own environment would otherwise be what git
// serves to a client that asks for no version, while the transport, which
// reads only the client's value, answers as for v0: HTTP's info/refs would
// send a service line that contradicts git's advertisement.
func serviceEnv(environ, extra []string) []string {
	env := make([]string, 0, len(environ)+1+len(extra))
	for _, kv := range environ {
		if keepInheritedEnv(kv) {
			env = append(env, kv)
		}
	}
	env = append(env, noReplaceObjects)
	return append(env, extra...)
}

// keepInheritedEnv reports whether the inherited "KEY=value" entry kv may be
// passed on to a git service process.
func keepInheritedEnv(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	// Environment variable names are case-insensitive on Windows.
	upper := strings.ToUpper(key)
	if upper == gitProtocolEnv {
		return false
	}
	if !strings.HasPrefix(upper, softServeEnvPrefix) {
		return true
	}
	if _, ok := inheritedSoftServeEnv[upper]; ok {
		return true
	}
	for _, p := range inheritedSoftServeEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}
