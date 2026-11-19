package backend

import (
	"context"
)

// cloneImport clones imp.remote into imp.path as a bare repository, a
// mirror when asked for. ctx stops the clone (see RunRemoteGit), so a remote
// that stalls cannot keep git and its helpers running once the import is
// given up.
func cloneImport(ctx context.Context, imp importSpec) error {
	args := []string{"clone", "--bare", "--quiet"}
	if imp.opts.Mirror {
		args = append(args, "--mirror")
	}
	args = append(args, "--end-of-options", imp.remote, imp.path)
	return RunRemoteGit(ctx, "", imp.env, args...)
}
