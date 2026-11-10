package git

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// RefInfos lists branches and tags with the commit each points to, peeling
// annotated tags, and leaves the commit empty for tags of other objects.
func TestRefInfos(t *testing.T) {
	repo, head := setupTestRepo(t)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo.Path
		cmd.Env = append(cmd.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b",
			"GIT_COMMITTER_DATE=2001-02-03T04:05:06+07:00")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("branch", "dev")
	run("tag", "light")
	run("tag", "-a", "-m", "release one\n\nbody line", "annotated")
	run("tag", "tree", "HEAD^{tree}")
	run("tag", "-a", "-m", "a tree", "annotated-tree", "HEAD^{tree}")
	// Git prints a UTC date as "Z" or "+00:00" depending on its version, so
	// the dates are compared as instants rather than as text.
	parseDate := func(s string) time.Time {
		t.Helper()
		d, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	authorDate := parseDate(run("log", "-1", "--format=%aI"))
	committerDate := parseDate(run("log", "-1", "--format=%cI"))

	tags, err := repo.RefInfos(RefsTags)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*RefInfo{}
	for _, info := range tags {
		got[info.Name().Short()] = info
	}
	if len(got) != 4 {
		t.Fatalf("RefInfos(RefsTags) = %d references; want 4", len(tags))
	}
	for name, want := range map[string]struct {
		commit, subject string
	}{
		"light":          {commit: head.ID},
		"annotated":      {commit: head.ID, subject: "release one"},
		"tree":           {},
		"annotated-tree": {subject: "a tree"},
	} {
		info := got[name]
		if info == nil {
			t.Fatalf("RefInfos(RefsTags) lacks tag %q", name)
		}
		if !info.IsTag() || info.Name().String() != RefsTags+name {
			t.Errorf("tag %q has reference %q", name, info.Name())
		}
		if info.CommitID != want.commit || info.TagSubject != want.subject {
			t.Errorf("tag %q = commit %q, subject %q; want %q, %q",
				name, info.CommitID, info.TagSubject, want.commit, want.subject)
		}
		if want.commit == "" {
			if !info.AuthorWhen.IsZero() || !info.CommitterWhen.IsZero() {
				t.Errorf("tag %q without a commit has dates %v, %v", name, info.AuthorWhen, info.CommitterWhen)
			}
			continue
		}
		if !info.AuthorWhen.Equal(authorDate) || !info.CommitterWhen.Equal(committerDate) {
			t.Errorf("tag %q has dates %v, %v; want the commit's %v, %v",
				name, info.AuthorWhen, info.CommitterWhen, authorDate, committerDate)
		}
	}
	if got["annotated"].ID == head.ID {
		t.Error("annotated tag reference ID is the commit, not the tag object")
	}

	branches, err := repo.RefInfos(RefsHeads)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 {
		t.Fatalf("RefInfos(RefsHeads) = %d references; want 2", len(branches))
	}
	for _, info := range branches {
		if !info.IsBranch() || info.CommitID != head.ID || info.ID != head.ID ||
			info.TagSubject != "" || info.CommitterWhen.IsZero() {
			t.Errorf("branch %q = %+v; want commit %s", info.Name(), info, head.ID)
		}
	}
}
