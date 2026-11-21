//go:build windows

package git

import (
	"syscall"
	"testing"
)

// The Windows error codes clientGoneOn checks, spelled out as numbers, are
// the ones syscall defines on Windows, and ClientGone uses them there.
func TestWindowsErrnosMatchSyscall(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want syscall.Errno
	}{
		{"ERROR_BROKEN_PIPE", windowsErrorBrokenPipe, syscall.ERROR_BROKEN_PIPE},
		{"WSAECONNABORTED", windowsWSAEConnAborted, syscall.WSAECONNABORTED},
		{"WSAECONNRESET", windowsWSAEConnReset, syscall.WSAECONNRESET},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
		if !ClientGone(tc.want) {
			t.Errorf("ClientGone(%s) = false, want true", tc.name)
		}
	}
	if !ClientGone(windowsErrorNoData) {
		t.Error("ClientGone(ERROR_NO_DATA) = false, want true")
	}
}
