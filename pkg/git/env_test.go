package git

import (
	"bytes"
	"io"
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
		"NOEQUALS",
		"SOFT_SERVE_REPO_NAME=repo",
		"SOFT_SERVE_DATA_PATH=/data",
	}
	if !slices.Equal(got, want) {
		t.Errorf("serviceEnv:\n got %q\nwant %q", got, want)
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
