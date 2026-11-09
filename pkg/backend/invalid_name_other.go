//go:build !windows

package backend

func isInvalidNamePlatform(error) bool { return false }
