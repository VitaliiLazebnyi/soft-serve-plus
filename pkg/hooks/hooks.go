// Package hooks generates the git hooks of a repository and defines what
// the server hands them: their environment, the config file they read, and
// the file post-receive reports the updated refs in.
package hooks

// HookArg is an argument to a git hook.
type HookArg struct {
	OldSha  string
	NewSha  string
	RefName string
}
