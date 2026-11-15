package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestConfigEnv(t *testing.T) {
	t.Parallel()

	got := ConfigEnv(ConfigEntry{"a.b", "1"}, ConfigEntry{"c.d", ""})
	want := []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=1",
		"GIT_CONFIG_KEY_1=c.d", "GIT_CONFIG_VALUE_1=",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ConfigEnv() = %q, want %q", got, want)
	}
	if got := ConfigEnv(); !slices.Equal(got, []string{"GIT_CONFIG_COUNT=0"}) {
		t.Errorf("ConfigEnv() = %q, want a zero count", got)
	}
}

// serverEnv is ServerConfig rendered from index 0.
func serverEnv(t *testing.T) []string {
	t.Helper()
	return ConfigEnv(ServerConfig()...)[1:]
}

func TestWithServerConfig(t *testing.T) {
	t.Parallel()

	n := len(ServerConfig())
	server := serverEnv(t)
	countEnv := func(c int) string { return "GIT_CONFIG_COUNT=" + strconv.Itoa(c) }

	tests := []struct {
		name    string
		environ []string
		want    []string
	}{
		{
			name:    "no git config in the environment",
			environ: []string{"PATH=/bin", "GIT_CONFIG_GLOBAL=/dev/null"},
			want:    append([]string{"PATH=/bin", "GIT_CONFIG_GLOBAL=/dev/null", countEnv(n)}, server...),
		},
		{
			name:    "empty count is zero",
			environ: []string{"GIT_CONFIG_COUNT=", "HOME=/h"},
			want:    append([]string{"HOME=/h", countEnv(n)}, server...),
		},
		{
			// The operator's entries come after the server's, so they
			// win for a key both set, and are renumbered.
			name: "operator entries kept after the server's",
			environ: []string{
				"GIT_CONFIG_COUNT=2",
				"GIT_CONFIG_KEY_0=core.compression", "GIT_CONFIG_VALUE_0=1",
				"GIT_CONFIG_KEY_1=maintenance.autoDetach", "GIT_CONFIG_VALUE_1=true",
				"HOME=/h",
			},
			want: append(append([]string{"HOME=/h", countEnv(n + 2)}, server...),
				"GIT_CONFIG_KEY_"+strconv.Itoa(n)+"=core.compression", "GIT_CONFIG_VALUE_"+strconv.Itoa(n)+"=1",
				"GIT_CONFIG_KEY_"+strconv.Itoa(n+1)+"=maintenance.autoDetach", "GIT_CONFIG_VALUE_"+strconv.Itoa(n+1)+"=true",
			),
		},
		{
			// As for os/exec, the last of a duplicated variable counts:
			// here the count of 1, so KEY_1 is beyond it and dropped.
			name: "last duplicate counts, entries past the count dropped",
			environ: []string{
				"GIT_CONFIG_COUNT=2",
				"GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=1",
				"GIT_CONFIG_KEY_1=c.d", "GIT_CONFIG_VALUE_1=2",
				"GIT_CONFIG_COUNT=1",
			},
			want: append(append([]string{countEnv(n + 1)}, server...),
				"GIT_CONFIG_KEY_"+strconv.Itoa(n)+"=a.b", "GIT_CONFIG_VALUE_"+strconv.Itoa(n)+"=1",
			),
		},
		{
			name:    "bogus count left to git",
			environ: []string{"GIT_CONFIG_COUNT=x", "HOME=/h"},
			want:    []string{"GIT_CONFIG_COUNT=x", "HOME=/h"},
		},
		{
			name:    "negative count left to git",
			environ: []string{"GIT_CONFIG_COUNT=-1"},
			want:    []string{"GIT_CONFIG_COUNT=-1"},
		},
		{
			name:    "missing value left to git",
			environ: []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a.b"},
			want:    []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a.b"},
		},
		{
			name:    "missing key left to git",
			environ: []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_VALUE_0=x"},
			want:    []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_VALUE_0=x"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := WithServerConfig(tc.environ)
			if !slices.Equal(got, tc.want) {
				t.Errorf("WithServerConfig(%q)\n got %q\nwant %q", tc.environ, got, tc.want)
			}
		})
	}
}

// TestWithServerConfigPrecedence runs git against the merged environment to
// check the precedence the server relies on: its settings override every
// configuration file, including the repository's, and an operator's own
// GIT_CONFIG_* in the environment overrides the server's.
func TestWithServerConfigPrecedence(t *testing.T) {
	t.Parallel()

	repo := filepath.Join(t.TempDir(), "r.git")
	base := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(t.TempDir(), "none")}
	if runtime.GOOS == "windows" {
		base = append(base, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"))
	}
	base = append(base, "PATH="+os.Getenv("PATH"), "HOME="+t.TempDir())
	run := func(env []string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(base, "init", "--bare", "-q", repo)
	run(base, "-C", repo, "config", "gc.autoDetach", "true")
	run(base, "-C", repo, "config", "maintenance.autoDetach", "true")

	get := func(env []string, key string) string {
		return run(env, "-C", repo, "config", "--get", key)
	}
	server := WithServerConfig(base)
	for _, key := range []string{"gc.autoDetach", "maintenance.autoDetach"} {
		if v := get(server, key); v != "false" {
			t.Errorf("%s = %q under the server's environment, want false over the repository's true", key, v)
		}
	}

	operator := WithServerConfig(append(slices.Clone(base),
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=maintenance.autoDetach", "GIT_CONFIG_VALUE_0=true"))
	if v := get(operator, "maintenance.autoDetach"); v != "true" {
		t.Errorf("maintenance.autoDetach = %q with the operator's GIT_CONFIG_*, want the operator's true", v)
	}
	if v := get(operator, "gc.autoDetach"); v != "false" {
		t.Errorf("gc.autoDetach = %q with the operator's GIT_CONFIG_*, want the server's false", v)
	}
}
