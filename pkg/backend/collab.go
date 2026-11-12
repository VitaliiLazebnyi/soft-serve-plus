package backend

import (
	"context"
	"errors"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

// AddCollaborator adds a collaborator to a repository.
func (d *Backend) AddCollaborator(ctx context.Context, repo string, username string, level access.AccessLevel) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	repo = utils.SanitizeRepo(repo)
	r, err := d.Repository(ctx, repo)
	if err != nil {
		return err
	}

	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// Without the user, the insert fails on a NOT NULL constraint. The
		// row lock keeps the user from being deleted before the insert.
		if _, err := d.store.FindUserByUsernameForUpdate(ctx, tx, username); err != nil {
			return userNotFound(err)
		}
		return d.store.AddCollabByUsernameAndRepo(ctx, tx, username, repo, level)
	}); err != nil {
		// A repository deleted meanwhile fails the insert on a
		// constraint; report it as gone.
		if err := store.RepoGoneError(ctx, d.store, d.db, r.ID(), err); errors.Is(err, proto.ErrRepoNotFound) {
			return err
		}
		if errors.Is(err, db.ErrDuplicateKey) {
			return proto.ErrCollaboratorExist
		}

		return err
	}

	// The collaborator is committed, so an event problem is logged, not returned.
	ctx, cancel := AfterChangeContext(ctx)
	defer cancel()
	wh, err := webhook.NewCollaboratorEvent(ctx, proto.UserFromContext(ctx), r, username, webhook.CollaboratorEventAdded)
	if err != nil {
		d.logger.Error("collaborator added but webhook event skipped", "repo", repo, "user", username, "err", err)
		return nil
	}

	d.sendAfterChange(ctx, wh)
	return nil
}

// Collaborator is a repository collaborator and their access level.
type Collaborator struct {
	Username    string
	AccessLevel access.AccessLevel
}

// Collaborators returns the collaborators of a repository with their access
// levels.
func (d *Backend) Collaborators(ctx context.Context, repo string) ([]Collaborator, error) {
	repo = utils.SanitizeRepo(repo)
	var users []models.User
	var collabs []models.Collab
	if err := d.db.ReadTransactionContext(ctx, func(tx *db.Tx) error {
		// The join returns an empty list for a missing repository, so
		// look it up first.
		if _, err := d.store.GetRepoByName(ctx, tx, repo); err != nil {
			return err
		}

		var err error
		users, err = d.store.ListCollabsByRepoAsUsers(ctx, tx, repo)
		if err != nil {
			return err
		}

		collabs, err = d.store.ListCollabsByRepo(ctx, tx, repo)
		return err
	}); err != nil {
		return nil, wrapRepoError(err)
	}

	levels := make(map[int64]access.AccessLevel, len(collabs))
	for _, c := range collabs {
		levels[c.UserID] = c.AccessLevel
	}

	list := make([]Collaborator, 0, len(users))
	for _, u := range users {
		// The two reads can see different rows when another transaction
		// commits in between; list only collaborators whose level is known.
		level, ok := levels[u.ID]
		if !ok {
			continue
		}
		list = append(list, Collaborator{Username: u.Username, AccessLevel: level})
	}

	return list, nil
}

// IsCollaborator returns the access level and true if the user is a collaborator of the repository.
// A user who is not a collaborator gives (-1, false, nil); the error is only
// set when the lookup itself fails.
func (d *Backend) IsCollaborator(ctx context.Context, repo string, username string) (access.AccessLevel, bool, error) {
	if username == "" {
		return -1, false, nil
	}

	repo = utils.SanitizeRepo(repo)
	var m models.Collab
	if err := d.db.ReadTransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		m, err = d.store.GetCollabByUsernameAndRepo(ctx, tx, username, repo)
		return err
	}); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return -1, false, nil
		}
		return -1, false, err
	}

	return m.AccessLevel, m.ID > 0, nil
}

// RemoveCollaborator removes a collaborator from a repository.
func (d *Backend) RemoveCollaborator(ctx context.Context, repo string, username string) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	repo = utils.SanitizeRepo(repo)
	r, err := d.Repository(ctx, repo)
	if err != nil {
		return err
	}

	// The event reads the collaborator row, so it is built before the
	// removal. Failing to build it does not stop the removal: a missing
	// collaborator is reported by the removal itself, and any other failure
	// is logged once the removal is done.
	wh, whErr := webhook.NewCollaboratorEvent(ctx, proto.UserFromContext(ctx), r, username, webhook.CollaboratorEventRemoved)

	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return d.store.RemoveCollabByUsernameAndRepo(ctx, tx, username, repo)
	}); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return proto.ErrCollaboratorNotFound
		}

		return err
	}

	// The removal is committed, so the event is sent without affecting the result.
	if whErr != nil {
		d.logger.Error("collaborator removed but webhook event skipped", "repo", repo, "user", username, "err", whErr)
		return nil
	}
	d.sendAfterChange(ctx, wh)
	return nil
}
