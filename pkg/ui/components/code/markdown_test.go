package code

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// Whether the content is markdown is worked out once per SetContent, not on
// every render, and is worked out again for new content.
func TestMarkdownCachedPerContent(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	r := New(c, "", "")
	r.UseGlamour = true

	r.SetContent("# Title\n\nbody\n", "README.md")
	if r.isMarkdown == nil || !*r.isMarkdown {
		t.Fatalf("README.md not cached as markdown")
	}
	cached := r.isMarkdown
	r.SetSize(60, 20)
	r.Init()
	if r.isMarkdown != cached {
		t.Error("markdown detection ran again on a re-render")
	}

	r.SetContent("package main\n", "main.go")
	if r.isMarkdown == nil || *r.isMarkdown {
		t.Error("main.go still treated as markdown after SetContent")
	}
}

// SetMarkdownContent takes the caller's answer and does not work it out
// again, even on a re-render.
func TestSetMarkdownContentUsesCallerFlag(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	r := New(c, "", "")
	r.UseGlamour = true

	// README.md would be detected as markdown; the caller's false must win.
	r.SetMarkdownContent("# Title\n\nbody\n", "README.md", false)
	if r.markdown() {
		t.Fatal("SetMarkdownContent flag false was replaced by detection")
	}
	r.SetSize(60, 20)
	r.Init()
	if r.markdown() {
		t.Error("markdown detection ran again on a re-render")
	}

	// SetContent still detects for callers that pass no flag.
	r.SetContent("# Title\n\nbody\n", "README.md")
	if !r.markdown() {
		t.Error("SetContent did not detect README.md as markdown")
	}
}
