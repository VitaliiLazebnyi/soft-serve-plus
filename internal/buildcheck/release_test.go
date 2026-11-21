// Package buildcheck holds tests that check the repository's build, CI and
// packaging files. It has no non-test code.
package buildcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

// repoRoot is the repository root, relative to this package.
const repoRoot = "../.."

type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string         `yaml:"name"`
			Uses string         `yaml:"uses"`
			Run  string         `yaml:"run"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if len(w.Jobs) == 0 {
		t.Fatalf("%s: no jobs", name)
	}
	return w
}

// TestReleaseWorkflowDoesNotTrustSharedState checks that the tag-triggered
// release workflow builds only from what it fetches itself: the Actions cache
// can be written by any workflow run on main, so a release that restores it
// could compile and attest code that the tagged commit does not contain.
func TestReleaseWorkflowDoesNotTrustSharedState(t *testing.T) {
	t.Parallel()
	w := readWorkflow(t, "goreleaser.yml")
	var setupGo, goreleaser int
	for jobName, job := range w.Jobs {
		verified := false
		for i, step := range job.Steps {
			switch {
			case strings.HasPrefix(step.Uses, "actions/cache"):
				t.Errorf("job %s step %d: %s restores a cache other runs can write", jobName, i, step.Uses)
			case strings.HasPrefix(step.Uses, "actions/setup-go@"):
				setupGo++
				if v, ok := step.With["cache"]; !ok || v != false {
					t.Errorf("job %s step %d (setup-go): cache must be false, got %v", jobName, i, v)
				}
			case strings.HasPrefix(step.Uses, "actions/checkout@"):
				if v, ok := step.With["persist-credentials"]; !ok || v != false {
					t.Errorf("job %s step %d (checkout): persist-credentials must be false, got %v", jobName, i, v)
				}
			case strings.Contains(step.Run, "go mod verify"):
				verified = true
			case strings.HasPrefix(step.Uses, "goreleaser/goreleaser-action@"),
				strings.Contains(step.Run, "goreleaser"):
				goreleaser++
				if !verified {
					t.Errorf("job %s: GoReleaser runs before `go mod verify`", jobName)
				}
			}
		}
	}
	// If the workflow stops using these steps, the checks above check nothing.
	if setupGo == 0 {
		t.Error("no actions/setup-go step found; update this test")
	}
	if goreleaser == 0 {
		t.Error("no GoReleaser step found; update this test")
	}
}

// TestCheckoutsDoNotPersistCredentials checks that no workflow leaves the
// job's token in .git/config, where every later step, third-party actions
// and the code under test included, could read it. No workflow pushes with
// git.
func TestCheckoutsDoNotPersistCredentials(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(repoRoot, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var checkouts int
	for _, f := range files {
		name := filepath.Base(f)
		for jobName, job := range readWorkflow(t, name).Jobs {
			for i, step := range job.Steps {
				if !strings.HasPrefix(step.Uses, "actions/checkout@") {
					continue
				}
				checkouts++
				if v, ok := step.With["persist-credentials"]; !ok || v != false {
					t.Errorf("%s: job %s step %d (checkout): persist-credentials must be false, got %v", name, jobName, i, v)
				}
			}
		}
	}
	if checkouts == 0 {
		t.Error("no actions/checkout step found; update this test")
	}
}
