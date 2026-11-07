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
