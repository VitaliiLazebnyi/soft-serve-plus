//go:build !windows

package git

// isRenameRetryable reports whether a failed rename is worth retrying. Only
// Windows refuses to replace a file another process has open.
func isRenameRetryable(error) bool {
	return false
}
