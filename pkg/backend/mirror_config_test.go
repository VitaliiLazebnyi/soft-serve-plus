package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// TestSetMirrorConfig pins what SetMirror writes to a repository's git
// config: mirror = true on the remote with a URL, removed again when
// mirroring is switched off, and nothing else touched.
func TestSetMirrorConfig(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = seedCreate(be, ctx, "m", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rp := be.repoPath("m")

	cp := filepath.Join(rp, "config")
	f, err := os.OpenFile(cp, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	get := func(key string) string {
		t.Helper()
		out, err := gittestConfig(t, cp, key)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if err := be.SetMirror(ctx, "m", true); err != nil {
		t.Fatal(err)
	}
	if got := get("remote.Up.Per.mirror"); got != "true" {
		t.Fatalf("get(\"remote.Up.Per.mirror\") = %v, want %v", got, "true")
	}
	if got := get("remote.nourl.mirror"); got != "" {
		t.Fatalf("get(\"remote.nourl.mirror\") = %v, want %v", got, "")
	}
	if got := get("remote.empty.mirror"); got != "" {
		t.Fatalf("get(\"remote.empty.mirror\") = %v, want %v", got, "")
	}
	if got := get("custom.keep"); got != "me ; # too" {
		t.Fatalf("get(\"custom.keep\") = %v, want %v", got, "me ; # too")
	}
	if got := get("remote.Up.Per.url"); got != "https://1.1.1.1/b.git" {
		t.Fatalf("get(\"remote.Up.Per.url\") = %v, want %v", got, "https://1.1.1.1/b.git")
	}
	if got := get("remote.Up.Per.pushurl"); got != "https://1.1.1.1/p.git" {
		t.Fatalf("get(\"remote.Up.Per.pushurl\") = %v, want %v", got, "https://1.1.1.1/p.git")
	}

	if err := be.SetMirror(ctx, "m", false); err != nil {
		t.Fatal(err)
	}
	if got := get("remote.Up.Per.mirror"); got != "" {
		t.Fatalf("get(\"remote.Up.Per.mirror\") = %v, want %v", got, "")
	}
	if got := get("custom.keep"); got != "me ; # too" {
		t.Fatalf("get(\"custom.keep\") = %v, want %v", got, "me ; # too")
	}
	if got := get("remote.Up.Per.url"); got != "https://1.1.1.1/b.git" {
		t.Fatalf("get(\"remote.Up.Per.url\") = %v, want %v", got, "https://1.1.1.1/b.git")
	}
}

// A repository with two remotes can't be made a mirror: the mirror job
// fetches every ref of a remote with --prune, so each would delete and
// overwrite the other's refs. Nothing is written.
func TestSetMirrorRefusesTwoRemotes(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = seedCreate(be, ctx, "m", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(be.repoPath("m"), "config")
	f, err := os.OpenFile(cp, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`[remote "origin"]
	url = https://1.1.1.1/a.git
[remote "backup"]
	url = https://1.1.1.1/b.git
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = be.SetMirror(ctx, "m", true)
	if !(err != nil) {
		t.Fatal("not true: err != nil")
	}
	if !strings.Contains(err.Error(), "more than one remote") {
		t.Fatal("not true: strings.Contains(err.Error(), \"more than one remote\")")
	}
	out, err := gittestConfig(t, cp, "remote.origin.mirror")
	if err != nil {
		t.Fatal(err)
	}
	if got := out; got != "" {
		t.Fatalf("out = %v, want %v", got, "")
	}
	r, err := be.Repository(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if r.IsMirror() {
		t.Fatal("not true: !r.IsMirror()")
	}
}

// gittestConfig returns the last value of key in the config file at path,
// or "" when it is not set.
func gittestConfig(t *testing.T, path, key string) (string, error) {
	t.Helper()
	out := gittest.Git(t, filepath.Dir(path), nil, "config", "--file", path, "--get", "--default", "", key)
	return strings.TrimSpace(out), nil
}
