package daemon

import (
	"errors"
	"slices"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/gitserve"
)

func TestParseRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		want gitRequest
		err  error
	}{
		{"git-upload-pack", gitRequest{}, gitserve.ErrInvalidRequest},
		{"git-upload-pack /r.git", gitRequest{}, gitserve.ErrInvalidRequest},
		{"git-receive-pack /r.git\x00host=h\x00", gitRequest{}, ErrPushNotSupported},
		{"git-x /r.git\x00host=h\x00", gitRequest{}, gitserve.ErrInvalidRequest},
		{"git-upload-pack /r.git\x00host=h\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h"}, nil},
		{"git-upload-pack /a/../r\x00host=h:9418\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h:9418"}, nil},
		{"git-upload-archive /r.git\x00\x00", gitRequest{service: gitserve.UploadArchiveService, name: "r"}, nil},
		{"git-upload-pack /r.git\x00host=h\x00\x00version=2\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h", version: "2"}, nil},
		{"git-upload-pack /r.git\x00host=h\x00\x00version=2:x\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h", version: "2"}, nil},
		{"git-upload-pack /r.git\x00host=h\x00\x00version=3\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h"}, nil},
	} {
		got, err := parseRequest([]byte(tc.line))
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("parseRequest(%q) = %+v, %v; want %+v, %v", tc.line, got, err, tc.want, tc.err)
		}
	}
}

// TestEnviron: the daemon runs no hooks, so git gets only the requested
// protocol version, never the hook variables or the server config with its
// database data source.
func TestEnviron(t *testing.T) {
	env := testDaemon.environ(gitRequest{service: gitserve.UploadPackService, name: "r", host: "h", version: "2"})
	if want := []string{"GIT_PROTOCOL=version=2"}; !slices.Equal(env, want) {
		t.Errorf("environ = %q, want %q", env, want)
	}
	if env := testDaemon.environ(gitRequest{service: gitserve.UploadPackService, name: "r"}); len(env) != 0 {
		t.Errorf("environ without a requested version = %q, want none", env)
	}
}
