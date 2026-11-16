package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestSetMirrorConfig pins what SetMirror writes to a repository's git
// config: mirror = true on the remote with a URL, removed again when
// mirroring is switched off, and nothing else touched.
func TestSetMirrorConfig(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	_, err = seedCreate(be, ctx, "m", admin, proto.RepositoryOptions{})
	is.NoErr(err)
	rp := be.repoPath("m")

	cp := filepath.Join(rp, "config")
	f, err := os.OpenFile(cp, os.O_APPEND|os.O_WRONLY, 0)
	is.NoErr(err)
	// A remote named with a dot and capitals, with two urls (the last is
	// the one read) and a section split in two, one without a url and one
	// whose url is empty.
	_, err = f.WriteString(`[remote "Up.Per"]
	url = https://1.1.1.1/a.git
	url = https://1.1.1.1/b.git
	fetch = +refs/*:refs/*
[remote "nourl"]
	fetch = +refs/*:refs/*
[remote "empty"]
	url =
[custom]
	keep = "me ; # too"
[remote "Up.Per"]
	pushurl = https://1.1.1.1/p.git
`)
	is.NoErr(err)
	is.NoErr(f.Close())

	get := func(key string) string {
		t.Helper()
		out, err := gittestConfig(t, cp, key)
		is.NoErr(err)
		return out
	}

	is.NoErr(be.SetMirror(ctx, "m", true))
	is.Equal(get("remote.Up.Per.mirror"), "true")
	is.Equal(get("remote.nourl.mirror"), "")
	is.Equal(get("remote.empty.mirror"), "")
	is.Equal(get("custom.keep"), "me ; # too")
	is.Equal(get("remote.Up.Per.url"), "https://1.1.1.1/b.git")
	is.Equal(get("remote.Up.Per.pushurl"), "https://1.1.1.1/p.git")

	is.NoErr(be.SetMirror(ctx, "m", false))
	is.Equal(get("remote.Up.Per.mirror"), "")
	is.Equal(get("custom.keep"), "me ; # too")
	is.Equal(get("remote.Up.Per.url"), "https://1.1.1.1/b.git")
}

// A repository with two remotes can't be made a mirror: the mirror job
// fetches every ref of a remote with --prune, so each would delete and
// overwrite the other's refs. Nothing is written.
func TestSetMirrorRefusesTwoRemotes(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	_, err = seedCreate(be, ctx, "m", admin, proto.RepositoryOptions{})
	is.NoErr(err)
	cp := filepath.Join(be.repoPath("m"), "config")
	f, err := os.OpenFile(cp, os.O_APPEND|os.O_WRONLY, 0)
	is.NoErr(err)
	_, err = f.WriteString(`[remote "origin"]
	url = https://1.1.1.1/a.git
[remote "backup"]
	url = https://1.1.1.1/b.git
`)
	is.NoErr(err)
	is.NoErr(f.Close())

	err = be.SetMirror(ctx, "m", true)
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "more than one remote"))
	out, err := gittestConfig(t, cp, "remote.origin.mirror")
	is.NoErr(err)
	is.Equal(out, "")
	r, err := be.Repository(ctx, "m")
	is.NoErr(err)
	is.True(!r.IsMirror())
}

// gittestConfig returns the last value of key in the config file at path,
// or "" when it is not set.
func gittestConfig(t *testing.T, path, key string) (string, error) {
	t.Helper()
	out := gittest.Git(t, filepath.Dir(path), nil, "config", "--file", path, "--get", "--default", "", key)
	return strings.TrimSpace(out), nil
}
