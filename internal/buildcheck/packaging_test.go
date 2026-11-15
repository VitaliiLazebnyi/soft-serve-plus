package buildcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

type goreleaserConfig struct {
	Before struct {
		// A hook is a command string or a mapping with a cmd key.
		Hooks []any `yaml:"hooks"`
	} `yaml:"before"`
	Builds []struct {
		Binary       string   `yaml:"binary"`
		Flags        []string `yaml:"flags"`
		ModTimestamp string   `yaml:"mod_timestamp"`
	} `yaml:"builds"`
	NFPMs []struct {
		Dependencies []string `yaml:"dependencies"`
	} `yaml:"nfpms"`
}

func readGoreleaser(t *testing.T) goreleaserConfig {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, ".goreleaser.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var c goreleaserConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse .goreleaser.yml: %v", err)
	}
	return c
}

// dockerfileInstructions returns the Dockerfile's instructions, one string per
// instruction with continuation lines joined and comments dropped.
func dockerfileInstructions(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var cur strings.Builder
	for line := range strings.SplitSeq(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || (trimmed == "" && cur.Len() == 0) {
			continue
		}
		if s, ok := strings.CutSuffix(trimmed, "\\"); ok {
			cur.WriteString(s + " ")
			continue
		}
		cur.WriteString(trimmed)
		if cur.Len() > 0 {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	return out
}

var (
	apkAdd = regexp.MustCompile(`apk add\s+((?:-\S+\s+)*)([^&;|]+)`)
	envCI  = regexp.MustCompile(`^ENV\s+(?:.*\s)?CI(?:=|\s)`)
)

// TestImageInstallsOnlyWhatTheServerUses checks that the image installs git,
// the OpenSSH client (for ssh:// mirrors) and tini, but not bash (the hooks
// the server generates are #!/bin/sh), the OpenSSH server or its SFTP server,
// and that it does not set CI, a workaround for a terminal library the binary
// no longer links.
func TestImageInstallsOnlyWhatTheServerUses(t *testing.T) {
	t.Parallel()
	var pkgs []string
	for _, ins := range dockerfileInstructions(t) {
		if envCI.MatchString(ins) {
			t.Errorf("Dockerfile sets CI: %s", ins)
		}
		if !strings.HasPrefix(ins, "RUN ") {
			continue
		}
		for _, m := range apkAdd.FindAllStringSubmatch(ins, -1) {
			pkgs = append(pkgs, strings.Fields(m[2])...)
		}
	}
	if len(pkgs) == 0 {
		t.Fatal("no `apk add` found in the Dockerfile; update this test")
	}
	for _, want := range []string{"git", "openssh-client-default", "tini"} {
		if !slices.Contains(pkgs, want) {
			t.Errorf("Dockerfile does not install %s; installs %v", want, pkgs)
		}
	}
	for _, unwanted := range []string{"bash", "openssh", "openssh-server", "openssh-sftp-server"} {
		if slices.Contains(pkgs, unwanted) {
			t.Errorf("Dockerfile installs %s; installs %v", unwanted, pkgs)
		}
	}
}

// TestPackagesDoNotDependOnBash checks that the deb, rpm and apk packages do
// not pull in bash, which nothing the server runs needs.
func TestPackagesDoNotDependOnBash(t *testing.T) {
	t.Parallel()
	c := readGoreleaser(t)
	if len(c.NFPMs) == 0 {
		t.Fatal("no nfpms in .goreleaser.yml; update this test")
	}
	for i, n := range c.NFPMs {
		if !slices.Contains(n.Dependencies, "git") {
			t.Errorf("nfpms[%d]: git is not a dependency: %v", i, n.Dependencies)
		}
		if slices.Contains(n.Dependencies, "bash") {
			t.Errorf("nfpms[%d]: bash is a dependency: %v", i, n.Dependencies)
		}
	}
}

// TestReleaseBinariesAreReproducible checks that every release build strips
// local paths (-trimpath) and dates the binary by the commit, so a rebuild of
// a tag elsewhere gives the same bytes.
func TestReleaseBinariesAreReproducible(t *testing.T) {
	t.Parallel()
	c := readGoreleaser(t)
	if len(c.Builds) == 0 {
		t.Fatal("no builds in .goreleaser.yml; update this test")
	}
	for i, b := range c.Builds {
		if !slices.Contains(b.Flags, "-trimpath") {
			t.Errorf("builds[%d] (%s): flags %v lack -trimpath", i, b.Binary, b.Flags)
		}
		if b.ModTimestamp != "{{ .CommitTimestamp }}" {
			t.Errorf("builds[%d] (%s): mod_timestamp is %q, want {{ .CommitTimestamp }}", i, b.Binary, b.ModTimestamp)
		}
	}
}

// TestReleaseHooksDoNotRewriteModules checks that the release does not run a
// plain `go mod tidy`, which could change go.mod or go.sum during the build
// and so release code that differs from the tagged commit. `go mod tidy -diff`
// only reports.
func TestReleaseHooksDoNotRewriteModules(t *testing.T) {
	t.Parallel()
	c := readGoreleaser(t)
	for i, h := range c.Before.Hooks {
		var cmd string
		switch h := h.(type) {
		case string:
			cmd = h
		case map[string]any:
			cmd, _ = h["cmd"].(string)
		}
		f := strings.Fields(cmd)
		if len(f) < 3 || f[0] != "go" || f[1] != "mod" || f[2] != "tidy" {
			continue
		}
		if !slices.Contains(f[3:], "-diff") {
			t.Errorf("before.hooks[%d]: %q rewrites go.mod and go.sum; use `go mod tidy -diff`", i, cmd)
		}
	}
}
