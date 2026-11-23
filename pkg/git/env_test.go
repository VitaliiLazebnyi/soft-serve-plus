package git

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestServiceEnv(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"HOME=/home/soft",
		"SOFT_SERVE_USERNAME=mallory",
		"SOFT_SERVE_PUBLIC_KEY=ssh-ed25519 AAAA",
		"SOFT_SERVE_REPO_NAME=other",
		"SOFT_SERVE_DATA_PATH=/stale",
		"soft_serve_username=mallory",
		"SOFT_SERVE_DEBUG=1",
		"SOFT_SERVE_VERBOSE=1",
		"SOFT_SERVE_NO_COLOR=1",
		"SOFT_SERVE_TESTRUN=1",
		"SOFT_SERVE_TEST_WEBHOOK_ADDR=127.0.0.1:1",
		"SOFT_SERVEX=kept",
		"GIT_PROTOCOL=version=2",
		"git_protocol=version=2",
		"GIT_PROTOCOLX=kept",
		"NOEQUALS",
	}
	extra := []string{
		"SOFT_SERVE_REPO_NAME=repo",
		"SOFT_SERVE_DATA_PATH=/data",
	}

	got := serviceEnv(environ, extra)
	want := []string{
		"PATH=/usr/bin",
		"HOME=/home/soft",
		"SOFT_SERVE_DEBUG=1",
		"SOFT_SERVE_VERBOSE=1",
		"SOFT_SERVE_NO_COLOR=1",
		"SOFT_SERVE_TESTRUN=1",
		"SOFT_SERVE_TEST_WEBHOOK_ADDR=127.0.0.1:1",
		"SOFT_SERVEX=kept",
		"GIT_PROTOCOLX=kept",
		"NOEQUALS",
		"GIT_NO_REPLACE_OBJECTS=1",
		"SOFT_SERVE_REPO_NAME=repo",
		"SOFT_SERVE_DATA_PATH=/data",
	}
	if !slices.Equal(got, want) {
		t.Errorf("serviceEnv:\n got %q\nwant %q", got, want)
	}
}

// A GIT_PROTOCOL in the server's own environment does not reach git: a
// request that asks for no version is served v0, as the transport answers
// it, and a request's own version is still the one git serves. The server's
// environment is the environ argument, so the variable is set for the git
// child alone.
func TestServiceEnvDropsInheritedProtocol(t *testing.T) {
	t.Parallel()
	dir := initBare(t)
	environ := []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_PROTOCOL=version=2",
	}
	for _, tc := range []struct {
		name  string
		extra []string
		v2    bool
	}{
		{"no version asked", nil, false},
		{"version=1 asked", []string{"GIT_PROTOCOL=version=1"}, false},
		{"version=2 asked", []string{"GIT_PROTOCOL=version=2"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "git", "upload-pack", "--stateless-rpc", "--advertise-refs", dir)
			cmd.Env = serviceEnv(environ, tc.extra)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("upload-pack: %v", err)
			}
			if got := bytes.HasPrefix(out, []byte("000eversion 2\n")); got != tc.v2 {
				t.Errorf("git served v2: %t, want %t; advertisement %q", got, tc.v2, out)
			}
		})
	}
}

// TestServiceHandlerDropsInheritedIdentity checks that a SOFT_SERVE_USERNAME
// or SOFT_SERVE_PUBLIC_KEY in the server's environment does not reach git
// when the request does not set it, so a hook cannot attribute the push to
// that user.
func TestServiceHandlerDropsInheritedIdentity(t *testing.T) {
	t.Setenv("SOFT_SERVE_USERNAME", "mallory")
	t.Setenv("SOFT_SERVE_PUBLIC_KEY", "ssh-ed25519 AAAAmallory")
	t.Setenv("SOFT_SERVE_DEBUG", "true")

	var env []string
	var stdout bytes.Buffer
	err := UploadPackService.Handler(t.Context(), ServiceCommand{
		Stdin:  bytes.NewReader(nil),
		Stdout: &stdout,
		Stderr: io.Discard,
		Dir:    initBare(t),
		Args:   []string{"--advertise-refs"},
		Env:    []string{"SOFT_SERVE_REPO_NAME=repo"},
		CmdFunc: func(cmd *exec.Cmd) {
			env = slices.Clone(cmd.Env)
		},
	})
	if err != nil {
		t.Fatalf("upload-pack: %v", err)
	}
	if env == nil {
		t.Fatal("CmdFunc was not called")
	}

	for _, kv := range env {
		if strings.HasPrefix(kv, "SOFT_SERVE_USERNAME=") || strings.HasPrefix(kv, "SOFT_SERVE_PUBLIC_KEY=") {
			t.Errorf("git inherited %q from the server environment", kv)
		}
	}
	for _, kv := range []string{"SOFT_SERVE_REPO_NAME=repo", "SOFT_SERVE_DEBUG=true"} {
		if !slices.Contains(env, kv) {
			t.Errorf("git environment is missing %q", kv)
		}
	}
}
