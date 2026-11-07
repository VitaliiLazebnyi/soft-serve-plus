package webhook

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/google/go-querystring/query"
)

// PushEvent is a push event.
type PushEvent struct {
	Common

	// Ref is the branch or tag name.
	Ref string `json:"ref" url:"ref"`
	// Before is the previous commit SHA.
	Before string `json:"before" url:"before"`
	// After is the current commit SHA.
	After string `json:"after" url:"after"`
	// Commits is the list of commits.
	Commits Commits `json:"commits" url:"commits"`
}

// Commits is a list of commits.
type Commits []Commit

var _ query.Encoder = Commits(nil)

// EncodeValues implements query.Encoder. Each commit is form-encoded with its
// index, as key[0][id], key[0][author][name] and so on, the same bracket
// notation used for the other nested fields. Without it each commit would be
// written as a Go struct dump that receivers cannot parse.
func (cs Commits) EncodeValues(key string, v *url.Values) error {
	for i, c := range cs {
		cv, err := query.Values(c)
		if err != nil {
			return err
		}
		for k, vals := range cv {
			// k is a field name, possibly with its own nested keys, such as
			// author[name]: only the leading name needs brackets.
			field, nested, _ := strings.Cut(k, "[")
			name := fmt.Sprintf("%s[%d][%s]", key, i, field)
			if nested != "" {
				name += "[" + nested
			}
			for _, val := range vals {
				v.Add(name, val)
			}
		}
	}
	return nil
}

// NewPushEvent sends a push event.
func NewPushEvent(ctx context.Context, user proto.User, repo proto.Repository, ref, before, after string) (PushEvent, error) {
	event := EventPush

	payload := PushEvent{
		Ref:    ref,
		Before: before,
		After:  after,
		Common: Common{
			EventType: event,
			Repository: Repository{
				ID:          repo.ID(),
				Name:        repo.Name(),
				Description: repo.Description(),
				ProjectName: repo.ProjectName(),
				Private:     repo.IsPrivate(),
				CreatedAt:   repo.CreatedAt(),
				UpdatedAt:   repo.UpdatedAt(),
			},
			Sender: senderOf(user),
		},
	}

	cfg := config.FromContext(ctx)
	payload.Repository.HTTPURL = repoURL(cfg.HTTP.PublicURL, repo.Name())
	payload.Repository.SSHURL = repoURL(cfg.SSH.PublicURL, repo.Name())
	payload.Repository.GitURL = repoURL(cfg.Git.PublicURL, repo.Name())

	// Find repo owner.
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	owner, err := datastore.GetUserByID(ctx, dbx, repo.UserID())
	if err != nil {
		return PushEvent{}, db.WrapError(err)
	}

	payload.Repository.Owner.ID = owner.ID
	payload.Repository.Owner.Username = owner.Username

	// Find commits.
	r, err := repo.Open()
	if err != nil {
		return PushEvent{}, err
	}

	payload.Repository.DefaultBranch, _ = getDefaultBranch(repo)

	rev := after
	if !git.IsZeroHash(before) {
		rev = fmt.Sprintf("%s..%s", before, after)
	}

	commits, err := r.Log(rev, gitm.LogOptions{
		// XXX: limit to 20 commits for now
		// TODO: implement a commits api
		MaxCount: 20,
	})
	if err != nil {
		return PushEvent{}, err
	}

	payload.Commits = make([]Commit, len(commits))
	for i, c := range commits {
		payload.Commits[i] = Commit{
			ID:      c.ID.String(),
			Message: c.Message,
			Title:   c.Summary(),
			Author: Author{
				Name:  c.Author.Name,
				Email: c.Author.Email,
				Date:  c.Author.When,
			},
			Committer: Author{
				Name:  c.Committer.Name,
				Email: c.Committer.Email,
				Date:  c.Committer.When,
			},
			Timestamp: c.Committer.When,
		}
	}

	return payload, nil
}
