package git

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// ConfigEntry is a single git configuration key and its value.
type ConfigEntry struct {
	Key   string
	Value string
}

// ServerConfig returns the git configuration the server sets for every git
// process it starts, through the environment (see WithServerConfig and
// ssrf.GitEnv).
//
// git starts automatic maintenance after a push or fetch that writes to the
// repository (receive.autogc, fetch's own auto maintenance). By default that
// process detaches: since git 2.47 it daemonizes before it even checks
// whether there is work. The detached process is an orphan, re-parented to
// PID 1, and when the server is PID 1 (no init in the container) nothing
// reaps it, so every push leaves a zombie. Running maintenance in the
// foreground keeps it a child of the git process that started it, which
// waits for it. The cost is that a push or fetch whose repository is due
// for maintenance waits for it.
//
// maintenance.autoDetach falls back to gc.autoDetach when it is unset, and
// git older than 2.47 knows only gc.autoDetach, so gc.autoDetach alone would
// cover every version. maintenance.autoDetach is set as well because a
// configuration file that sets it (system, global or repository) would
// otherwise take precedence over the fallback.
func ServerConfig() []ConfigEntry {
	return []ConfigEntry{
		{Key: "gc.autoDetach", Value: "false"},
		{Key: "maintenance.autoDetach", Value: "false"},
	}
}

// Names of the variables git reads its environment configuration from.
const (
	configCountEnv = "GIT_CONFIG_COUNT"
	configKeyEnv   = "GIT_CONFIG_KEY_"
	configValueEnv = "GIT_CONFIG_VALUE_"
)

// ConfigEnv renders entries as the GIT_CONFIG_COUNT, GIT_CONFIG_KEY_<n> and
// GIT_CONFIG_VALUE_<n> variables (git 2.31 or newer). git reads them after
// every configuration file, so they override the system, global and
// repository configuration; only "git -c" overrides them. For a key set more
// than once, the last entry wins.
func ConfigEnv(entries ...ConfigEntry) []string {
	env := make([]string, 0, 1+2*len(entries))
	env = append(env, fmt.Sprintf("%s=%d", configCountEnv, len(entries)))
	for i, e := range entries {
		env = append(env,
			fmt.Sprintf("%s%d=%s", configKeyEnv, i, e.Key),
			fmt.Sprintf("%s%d=%s", configValueEnv, i, e.Value),
		)
	}
	return env
}

// WithServerConfig returns environ with ServerConfig added to the git
// configuration it carries in GIT_CONFIG_*. The entries already in environ,
// such as an operator's own GIT_CONFIG_* in the server's environment, are
// kept and come after the server's, so they win for a key both set: an
// operator can still set gc.autoDetach or maintenance.autoDetach to true
// that way (maintenance.autoDetach for git 2.47 or newer, gc.autoDetach for
// older git). Configuration files cannot; the environment overrides them.
//
// When environ's GIT_CONFIG_* is malformed (a count that is not a number, or
// a key or value missing below the count), environ is returned unchanged:
// git refuses to run with it either way, and its error names the problem.
func WithServerConfig(environ []string) []string {
	own, rest, ok := splitConfigEnv(environ)
	if !ok {
		return environ
	}
	entries := append(ServerConfig(), own...)
	return append(rest, ConfigEnv(entries...)...)
}

// splitConfigEnv returns the git configuration entries environ sets through
// GIT_CONFIG_*, and environ without those variables. For a variable set more
// than once, the last one counts, as for os/exec. ok is false when the
// variables are malformed.
func splitConfigEnv(environ []string) (entries []ConfigEntry, rest []string, ok bool) {
	vars := map[string]string{}
	rest = make([]string, 0, len(environ))
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if isConfigEnvName(name) {
			vars[configEnvName(name)] = value
			continue
		}
		rest = append(rest, kv)
	}
	countStr, set := vars[configCountEnv]
	if !set || countStr == "" {
		// git treats an empty count as 0.
		return nil, rest, true
	}
	count, err := strconv.Atoi(countStr)
	if err != nil || count < 0 {
		return nil, nil, false
	}
	entries = make([]ConfigEntry, 0, count)
	for i := range count {
		key, kok := vars[configKeyEnv+strconv.Itoa(i)]
		value, vok := vars[configValueEnv+strconv.Itoa(i)]
		if !kok || !vok {
			return nil, nil, false
		}
		entries = append(entries, ConfigEntry{Key: key, Value: value})
	}
	return entries, rest, true
}

// isConfigEnvName reports whether name is GIT_CONFIG_COUNT or one of the
// GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> variables. Other GIT_CONFIG_*
// variables (GIT_CONFIG_GLOBAL, GIT_CONFIG_NOSYSTEM, GIT_CONFIG_PARAMETERS,
// ...) are not configuration entries and pass through.
func isConfigEnvName(name string) bool {
	name = configEnvName(name)
	return name == configCountEnv ||
		strings.HasPrefix(name, configKeyEnv) ||
		strings.HasPrefix(name, configValueEnv)
}

// configEnvName returns name as git sees it: environment variable names are
// case-insensitive on Windows.
func configEnvName(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
