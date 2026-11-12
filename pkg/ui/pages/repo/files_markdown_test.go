package repo

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/key"

	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// The Files pane decides glamour rendering and the preview key from the
// markdown flag worked out when the file was loaded.
func TestFilesUsesLoadedMarkdownFlag(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	f := NewFiles(c)

	f.Update(FileContentMsg{content: "# Title\n", ext: "README.md", isMarkdown: true, load: f.load})
	if !f.code.UseGlamour {
		t.Error("markdown file not rendered with glamour")
	}
	if !helpHasDesc(f.FullHelp(), preview.Help().Desc) {
		t.Error("preview key missing from help for a markdown file")
	}

	f.Update(FileContentMsg{content: "package main\n", ext: "main.go", load: f.load})
	if f.code.UseGlamour {
		t.Error("non-markdown file rendered with glamour")
	}
	if helpHasDesc(f.FullHelp(), preview.Help().Desc) {
		t.Error("preview key shown for a non-markdown file")
	}
}

func helpHasDesc(groups [][]key.Binding, desc string) bool {
	for _, g := range groups {
		for _, k := range g {
			if k.Help().Desc == desc {
				return true
			}
		}
	}
	return false
}
