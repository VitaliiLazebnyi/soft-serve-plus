package ssh

import (
	"slices"
	"strings"
	"testing"
)

func TestRedactCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			name: "git",
			in:   []string{"git-upload-pack", "repo"},
			want: []string{"git-upload-pack", "repo"},
		},
		{
			name: "webhook create secret",
			in:   []string{"repo", "webhook", "create", "r", "https://hooks.example.com/services/T0/B0/TOKEN", "--secret", "S3CRET", "-e", "push"},
			want: []string{"repo", "webhook", "create", "r", "https://hooks.example.com/[redacted]", "--secret", "[redacted]", "-e", "push"},
		},
		{
			name: "secret forms",
			in:   []string{"repo", "webhook", "update", "r", "1", "--secret=S3CRET", "-s", "S3CRET", "-sS3CRET", "-s=S3CRET"},
			want: []string{"repo", "webhook", "update", "r", "1", "--secret=[redacted]", "-s", "[redacted]", "-s[redacted]", "-s[redacted]"},
		},
		{
			name: "webhook update url flag forms",
			in:   []string{"repo", "webhook", "update", "r", "1", "--url", "https://u:p@example.com/x?k=TOKEN", "--url=https://example.com/TOKEN", "-uhttps://example.com/TOKEN", "-u=https://example.com/TOKEN"},
			want: []string{"repo", "webhook", "update", "r", "1", "--url", "https://example.com/[redacted]", "--url=https://example.com/[redacted]", "-uhttps://example.com/[redacted]", "-u=https://example.com/[redacted]"},
		},
		{
			name: "import token",
			in:   []string{"repo", "import", "r", "https://TOKEN@github.com/x/y.git", "--lfs-endpoint", "https://u:TOKEN@lfs.example.com", "-m"},
			want: []string{"repo", "import", "r", "https://github.com/[redacted]", "--lfs-endpoint", "https://lfs.example.com", "-m"},
		},
		{
			name: "unparsable url",
			in:   []string{"repo", "import", "r", "https://TOKEN@example.com/%zz"},
			want: []string{"repo", "import", "r", "[redacted]"},
		},
		{
			name: "scp remote and plain args",
			in:   []string{"repo", "import", "r", "git@github.com:x/y.git", "-d", "a description", "--name=Proj"},
			want: []string{"repo", "import", "r", "git@github.com:x/y.git", "-d", "a description", "--name=Proj"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := slices.Clone(tt.in)
			got := redactCommand(in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("redactCommand(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !slices.Equal(in, tt.in) {
				t.Errorf("redactCommand modified its input: %q", in)
			}
			for _, a := range got {
				if strings.Contains(a, "TOKEN") || strings.Contains(a, "S3CRET") {
					t.Errorf("redactCommand(%q) leaked a secret: %q", tt.in, got)
				}
			}
		})
	}
}
