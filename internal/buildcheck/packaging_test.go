package buildcheck

import (
	"fmt"
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
	Archives []struct {
		BuildsInfo fileInfo `yaml:"builds_info"`
		// A file is a glob string or a mapping with src and info.
		Files []any `yaml:"files"`
	} `yaml:"archives"`
	DockersV2 []struct {
		Labels map[string]string `yaml:"labels"`
	} `yaml:"dockers_v2"`
	NFPMs []struct {
		Dependencies []string `yaml:"dependencies"`
		MTime        string   `yaml:"mtime"`
		Contents     []struct {
			Src      string   `yaml:"src"`
			Dst      string   `yaml:"dst"`
			FileInfo fileInfo `yaml:"file_info"`
		} `yaml:"contents"`
		Scripts nfpmScripts `yaml:"scripts"`
		APK     struct {
			Scripts nfpmScripts `yaml:"scripts"`
		} `yaml:"apk"`
	} `yaml:"nfpms"`
}

type fileInfo struct {
	Owner string `yaml:"owner"`
	Group string `yaml:"group"`
	Mode  int    `yaml:"mode"`
	MTime string `yaml:"mtime"`
}

type nfpmScripts struct {
	PreInstall  string `yaml:"preinstall"`
	PostInstall string `yaml:"postinstall"`
	PreRemove   string `yaml:"preremove"`
	PostRemove  string `yaml:"postremove"`
	PostUpgrade string `yaml:"postupgrade"`
}

func (s nfpmScripts) files() []string {
	var out []string
	for _, f := range []string{s.PreInstall, s.PostInstall, s.PreRemove, s.PostRemove, s.PostUpgrade} {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// hookCommand returns the command of a before hook.
func hookCommand(h any) string {
	switch h := h.(type) {
	case string:
		return h
	case map[string]any:
		cmd, _ := h["cmd"].(string)
		return cmd
	}
	return ""
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
	// gzip -n leaves the input's name and time out of the header.
	gzipNoName = regexp.MustCompile(`gzip -\w*n`)
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
		cmd := hookCommand(h)
		f := strings.Fields(cmd)
		if len(f) < 3 || f[0] != "go" || f[1] != "mod" || f[2] != "tidy" {
			continue
		}
		if !slices.Contains(f[3:], "-diff") {
			t.Errorf("before.hooks[%d]: %q rewrites go.mod and go.sum; use `go mod tidy -diff`", i, cmd)
		}
	}
}

// commitDate is the template that dates a file by the commit. GoReleaser
// parses these mtime fields as RFC 3339, which CommitTimestamp is not.
const commitDate = "{{ .CommitDate }}"

// buildTime matches the templates that give the time of the build.
var buildTime = regexp.MustCompile(`{{[^}]*\.(Date|Now|Timestamp)\b`)

// TestImageLabelsAreReproducible checks that the image labels carry the commit
// date rather than the build time, so a rebuilt image has the same config.
func TestImageLabelsAreReproducible(t *testing.T) {
	t.Parallel()
	c := readGoreleaser(t)
	if len(c.DockersV2) == 0 {
		t.Fatal("no dockers_v2 in .goreleaser.yml; update this test")
	}
	for i, d := range c.DockersV2 {
		if got := d.Labels["org.opencontainers.image.created"]; got != commitDate {
			t.Errorf("dockers_v2[%d]: org.opencontainers.image.created is %q, want %s", i, got, commitDate)
		}
		for k, v := range d.Labels {
			if buildTime.MatchString(v) {
				t.Errorf("dockers_v2[%d]: label %s is %q, the build time", i, k, v)
			}
		}
	}
}

// TestReleaseArtifactsAreReproducible checks that the archives and packages,
// which checksums.txt lists and the provenance attests, do not depend on when,
// where or by whom they are built: every entry is dated by the commit and has
// a fixed owner and mode, and the generated man page is dated by the commit
// too. Otherwise the checkout's file times, the umask, the build user and the
// day of the build all end up in the hashes.
func TestReleaseArtifactsAreReproducible(t *testing.T) {
	t.Parallel()
	c := readGoreleaser(t)
	if len(c.Archives) == 0 || len(c.NFPMs) == 0 {
		t.Fatal("no archives or nfpms in .goreleaser.yml; update this test")
	}
	fixed := func(what string, fi fileInfo, wantOwner bool) {
		t.Helper()
		if fi.MTime != commitDate {
			t.Errorf("%s: mtime is %q, want %s", what, fi.MTime, commitDate)
		}
		if wantOwner && (fi.Owner == "" || fi.Group == "") {
			t.Errorf("%s: owner %q and group %q must be set", what, fi.Owner, fi.Group)
		}
	}
	for i, a := range c.Archives {
		fixed(fmt.Sprintf("archives[%d].builds_info", i), a.BuildsInfo, true)
		for j, f := range a.Files {
			m, ok := f.(map[string]any)
			if !ok {
				t.Errorf("archives[%d].files[%d]: %v has no info; give it src and info", i, j, f)
				continue
			}
			var fi fileInfo
			b, err := yaml.Marshal(m["info"])
			if err == nil {
				err = yaml.Unmarshal(b, &fi)
			}
			if err != nil {
				t.Fatalf("archives[%d].files[%d].info: %v", i, j, err)
			}
			what := fmt.Sprintf("archives[%d].files[%d] (%v)", i, j, m["src"])
			fixed(what, fi, true)
			if fi.Mode == 0 {
				t.Errorf("%s: mode must be set", what)
			}
		}
	}

	var scripts []string
	for i, n := range c.NFPMs {
		if n.MTime != commitDate {
			t.Errorf("nfpms[%d]: mtime is %q, want %s", i, n.MTime, commitDate)
		}
		for _, f := range n.Contents {
			what := fmt.Sprintf("nfpms[%d] %s", i, f.Dst)
			fixed(what, f.FileInfo, false)
			if f.FileInfo.Mode == 0 {
				t.Errorf("%s: mode must be set", what)
			}
		}
		scripts = append(scripts, n.Scripts.files()...)
		scripts = append(scripts, n.APK.Scripts.files()...)
	}

	// nfpm dates the apk's scripts by their files, so a hook must date them.
	var touched []string
	var man bool
	for _, h := range c.Before.Hooks {
		cmd := hookCommand(h)
		if strings.HasPrefix(cmd, "touch ") && strings.Contains(cmd, commitDate) {
			for _, f := range strings.Fields(cmd) {
				touched = append(touched, filepath.Clean(f))
			}
		}
		if strings.Contains(cmd, "./cmd/soft man") {
			man = true
			if !strings.Contains(cmd, "SOURCE_DATE_EPOCH={{ .CommitTimestamp }}") {
				t.Errorf("man page hook %q does not set SOURCE_DATE_EPOCH to the commit time", cmd)
			}
			if !gzipNoName.MatchString(cmd) {
				t.Errorf("man page hook %q does not run gzip -n, so the header carries the build time", cmd)
			}
		}
	}
	if !man {
		t.Error("no hook generates the man page; update this test")
	}
	for _, f := range scripts {
		if !slices.Contains(touched, filepath.Clean(f)) {
			t.Errorf("package script %s is not dated by a `touch -d %q` before hook", f, commitDate)
		}
	}
}
