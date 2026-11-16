package lfs

import (
	"errors"
	"testing"
)

func TestNewEndpoint(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		in, want string
	}{
		"https remote":           {"https://host/org/repo.git", "https://host/org/repo.git/info/lfs"},
		"https remote no suffix": {"https://host/org/repo", "https://host/org/repo.git/info/lfs"},
		"trailing slash":         {"https://host/org/repo.git/", "https://host/org/repo.git/info/lfs"},
		"lfs endpoint":           {"https://host/org/repo.git/info/lfs", "https://host/org/repo.git/info/lfs"},
		"lfs endpoint slash":     {"https://host/org/repo.git/info/lfs/", "https://host/org/repo.git/info/lfs"},
		"custom lfs endpoint":    {"https://lfs.host/api/info/lfs", "https://lfs.host/api/info/lfs"},
		"query":                  {"https://host/org/repo.git?token=x", "https://host/org/repo.git/info/lfs?token=x"},
		"git":                    {"git://host:9418/r.git", "https://host/r.git/info/lfs"},
		"git ipv6":               {"git://[::1]:9418/r", "https://[::1]/r.git/info/lfs"},
		"git ipv6 no port":       {"git://[::1]/r", "https://[::1]/r.git/info/lfs"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ep, err := NewEndpoint(tt.in)
			if err != nil {
				t.Fatalf("NewEndpoint(%q): %v", tt.in, err)
			}
			if got := ep.String(); got != tt.want {
				t.Errorf("NewEndpoint(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if ep.Hostname() == "" {
				t.Errorf("NewEndpoint(%q) has no host: %q", tt.in, ep.Host)
			}
		})
	}
}

// Remotes objects can't be fetched from are refused, and strings that are
// not URLs are reported as such, without quoting either.
func TestNewEndpointUnsupported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want error
	}{
		{"ssh://git:TOKEN@host/r.git", ErrUnsupportedEndpoint},
		{"git+ssh://host/r.git", ErrUnsupportedEndpoint},
		{"host:r.git", ErrUnsupportedEndpoint},
		{"file:///srv/r.git", ErrUnsupportedEndpoint},
		{"/srv/r.git", ErrUnsupportedEndpoint},
		{"git@host:r.git", ErrInvalidEndpoint},
		{"[git@host:22]:r.git", ErrInvalidEndpoint},
		{"%zz://TOKEN@host", ErrInvalidEndpoint},
	} {
		ep, err := NewEndpoint(tc.in)
		if !errors.Is(err, tc.want) {
			t.Errorf("NewEndpoint(%q) = %v, %v, want %v", tc.in, ep, err, tc.want)
		}
		if err != nil && err.Error() != tc.want.Error() {
			t.Errorf("NewEndpoint(%q) error %q says more than %q", tc.in, err, tc.want)
		}
	}
}
