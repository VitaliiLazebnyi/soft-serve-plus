package lfs

import "testing"

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
		"ssh":                    {"ssh://git@host/r.git", "ssh://git@host/r.git"},
		"bare ssh":               {"git@host:r.git", "ssh://git@host/r.git"},
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
