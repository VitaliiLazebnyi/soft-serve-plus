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
	for jobName, job := range w.Jobs {
		verified := false
		for i, step := range job.Steps {
			switch {
			case strings.HasPrefix(step.Uses, "actions/setup-go@"):
				if v, ok := step.With["cache"]; !ok || v != false {
					t.Errorf("job %s step %d (setup-go): cache must be false, got %v", jobName, i, v)
				}
			case strings.HasPrefix(step.Uses, "actions/checkout@"):
				if v, ok := step.With["persist-credentials"]; !ok || v != false {
					t.Errorf("job %s step %d (checkout): persist-credentials must be false, got %v", jobName, i, v)
				}
			case strings.Contains(step.Run, "go mod verify"):
				verified = true
			case strings.HasPrefix(step.Uses, "goreleaser/goreleaser-action@"):
				if !verified {
					t.Errorf("job %s: GoReleaser runs before `go mod verify`", jobName)
				}
			}
		}
	}
}
