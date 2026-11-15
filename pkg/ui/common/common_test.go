package common_test

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

func TestIsFileMarkdown(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		content  string // XXX: chroma doesn't correctly analyze mk files
		isMkd    bool
	}{
		{"simple", "README.md", "", true},
		{"empty", "", "", false},
		{"no extension", "README", "", false},
		{"weird extension", "README.foo", "", false},
		{"long ext", "README.markdown", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := common.IsFileMarkdown(c.content, c.filename); got != c.isMkd {
				t.Errorf("IsFileMarkdown(%q, %q) = %v, want %v", c.content, c.filename, got, c.isMkd)
			}
		})
	}
}

// The UI's size comes from the SSH client; NewCommon and SetSize keep it to
// at most 500x200 cells.
func TestCommonSizeClamped(t *testing.T) {
	for _, tc := range []struct {
		width, height         int
		wantWidth, wantHeight int
	}{
		{80, 24, 80, 24},
		{500, 200, 500, 200},
		{501, 201, 500, 200},
		{30000, 30000, 500, 200},
		{30000, 10, 500, 10},
		{10, 30000, 10, 200},
		{0, 0, 0, 0},
	} {
		c := common.NewCommon(context.Background(), tc.width, tc.height)
		if c.Width != tc.wantWidth || c.Height != tc.wantHeight {
			t.Errorf("NewCommon(%dx%d) size %dx%d, want %dx%d", tc.width, tc.height, c.Width, c.Height, tc.wantWidth, tc.wantHeight)
		}
		c.SetSize(1, 1)
		c.SetSize(tc.width, tc.height)
		if c.Width != tc.wantWidth || c.Height != tc.wantHeight {
			t.Errorf("SetSize(%dx%d) size %dx%d, want %dx%d", tc.width, tc.height, c.Width, c.Height, tc.wantWidth, tc.wantHeight)
		}
		c.Zone.Close()
	}
}
