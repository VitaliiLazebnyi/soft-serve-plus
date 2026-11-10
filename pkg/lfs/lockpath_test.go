package lfs

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateLockPath(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		"a.bin",
		"dir/a.bin",
		"dir/sub/a b.bin",
		".hidden/a.bin",
		"a..b",
		"..a",
		`win\style.bin`,
		"ünïcode/文件.psd",
		strings.Repeat("a", MaxLockPathLength),
	} {
		if err := ValidateLockPath(p); err != nil {
			t.Errorf("ValidateLockPath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{
		"",
		".",
		"..",
		"/",
		"/a.bin",
		"../a.bin",
		"dir/../../a.bin",
		"dir/../a.bin",
		"./a.bin",
		"dir/./a.bin",
		"dir//a.bin",
		"dir/",
		"a\x00b",
		strings.Repeat("a", MaxLockPathLength+1),
	} {
		if err := ValidateLockPath(p); !errors.Is(err, ErrInvalidLockPath) {
			t.Errorf("ValidateLockPath(%q) = %v, want %v", p, err, ErrInvalidLockPath)
		}
	}
}
