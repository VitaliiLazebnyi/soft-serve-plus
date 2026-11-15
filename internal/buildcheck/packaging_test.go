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
// the server generates are #!/bin/sh) or the OpenSSH server, and that it does
// not set CI, a workaround for a terminal library the binary no longer links.
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
	for _, unwanted := range []string{"bash", "openssh", "openssh-server"} {
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
