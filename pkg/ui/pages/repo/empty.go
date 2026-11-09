package repo

import (
	"fmt"

	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// emptyLocalRepoMsg is shown for an empty repository browsed locally, which
// has no server URL to clone from or push to.
const emptyLocalRepoMsg = `# Empty repository

This repository has no commits yet.
`

func defaultEmptyRepoMsg(c common.Common, repo string) string {
	if c.HideCloneCmd {
		return emptyLocalRepoMsg
	}
	return fmt.Sprintf(`# Quick Start

Get started by cloning this repository, add your files, commit, and push.

## Clone this repository.

`+"```"+`sh
git clone %[1]s
`+"```"+`

## Creating a new repository on the command line

`+"```"+`sh
touch README.md
git init
git add README.md
git branch -M main
git commit -m "first commit"
git remote add origin %[1]s
git push -u origin main
`+"```"+`

## Pushing an existing repository from the command line

`+"```"+`sh
git remote add origin %[1]s
git push -u origin main
`+"```"+`
`, common.RepoURL(c.Config().SSH.PublicURL, repo))
}
