package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/git"
)

func TestParseRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		want gitRequest
		err  error
	}{
		{"git-upload-pack", gitRequest{}, git.ErrInvalidRequest},
		{"git-upload-pack /r.git", gitRequest{}, git.ErrInvalidRequest},
		{"git-receive-pack /r.git\x00host=h\x00", gitRequest{}, ErrPushNotSupported},
		{"git-x /r.git\x00host=h\x00", gitRequest{}, git.ErrInvalidRequest},
		{"git-upload-pack /r.git\x00host=h\x00", gitRequest{service: git.UploadPackService, name: "r", host: "h"}, nil},
		{"git-upload-pack /a/../r\x00host=h:9418\x00", gitRequest{service: git.UploadPackService, name: "r", host: "h:9418"}, nil},
		{"git-upload-archive /r.git\x00\x00", gitRequest{service: git.UploadArchiveService, name: "r"}, nil},
		{"git-upload-pack /r.git\x00host=h\x00\x00version=2\x00", gitRequest{service: git.UploadPackService, name: "r", host: "h", version: "2"}, nil},
		{"git-upload-pack /r.git\x00host=h\x00\x00version=2:x\x00", gitRequest{service: git.UploadPackService, name: "r", host: "h"}, nil},
	} {
		got, err := parseRequest([]byte(tc.line))
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("parseRequest(%q) = %+v, %v; want %+v, %v", tc.line, got, err, tc.want, tc.err)
		}
	}
}

func TestEnviron(t *testing.T) {
	env := testDaemon.environ(gitRequest{service: git.UploadPackService, name: "r", host: "h", version: "2"})
	for _, want := range []string{"SOFT_SERVE_REPO_NAME=r", "SOFT_SERVE_HOST=h", "GIT_PROTOCOL=version=2"} {
		if !slices.Contains(env, want) {
			t.Errorf("environment lacks %q: %q", want, env)
		}
	}
	env = testDaemon.environ(gitRequest{service: git.UploadPackService, name: "r"})
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_PROTOCOL=") {
			t.Errorf("GIT_PROTOCOL set without a requested version: %q", e)
		}
	}
}
