package git

import "strings"

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
// followed by extra.
//
// Without this, a SOFT_SERVE_USERNAME or SOFT_SERVE_PUBLIC_KEY left in the
// server's environment would reach the hooks whenever the request does not
// set it itself (an anonymous or keyless push), and the push would be
// attributed to that user.
func serviceEnv(environ, extra []string) []string {
	env := make([]string, 0, len(environ)+len(extra))
	for _, kv := range environ {
		if keepInheritedEnv(kv) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// keepInheritedEnv reports whether the inherited "KEY=value" entry kv may be
// passed on to a git service process.
func keepInheritedEnv(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	// Environment variable names are case-insensitive on Windows.
	upper := strings.ToUpper(key)
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
