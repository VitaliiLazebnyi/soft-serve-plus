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
//
// It implements backend.Backend.
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

	if err := db.WrapError(
		d.db.TransactionContext(ctx, func(tx *db.Tx) error {
			return d.store.AddCollabByUsernameAndRepo(ctx, tx, username, repo, level)
		}),
	); err != nil {
		// A repository deleted meanwhile fails the insert on a
		// constraint too; report it as gone, not as a duplicate.
		if err := store.RepoGoneError(ctx, d.store, d.db, r.ID(), err); errors.Is(err, proto.ErrRepoNotFound) {
			return err
		}
		if errors.Is(err, db.ErrDuplicateKey) {
			return proto.ErrCollaboratorExist
		}

		return err
	}

	// The collaborator is committed, so an event problem is logged, not returned.
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
//
// It implements backend.Backend.
func (d *Backend) Collaborators(ctx context.Context, repo string) ([]Collaborator, error) {
	repo = utils.SanitizeRepo(repo)
	var users []models.User
	var collabs []models.Collab
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
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
//
// It implements backend.Backend.
func (d *Backend) IsCollaborator(ctx context.Context, repo string, username string) (access.AccessLevel, bool, error) {
	if username == "" {
		return -1, false, nil
	}

	repo = utils.SanitizeRepo(repo)
	var m models.Collab
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		m, err = d.store.GetCollabByUsernameAndRepo(ctx, tx, username, repo)
		return err
	}); err != nil {
		return -1, false, db.WrapError(err)
	}

	return m.AccessLevel, m.ID > 0, nil
}

// RemoveCollaborator removes a collaborator from a repository.
//
// It implements backend.Backend.
func (d *Backend) RemoveCollaborator(ctx context.Context, repo string, username string) error {
	repo = utils.SanitizeRepo(repo)
	r, err := d.Repository(ctx, repo)
	if err != nil {
		return err
	}

	wh, err := webhook.NewCollaboratorEvent(ctx, proto.UserFromContext(ctx), r, username, webhook.CollaboratorEventRemoved)
	if err != nil {
		// The event looks up the collaborator row, so a missing row shows up here.
		if errors.Is(db.WrapError(err), db.ErrRecordNotFound) {
			return proto.ErrCollaboratorNotFound
		}
		return err
	}

	if err := db.WrapError(
		d.db.TransactionContext(ctx, func(tx *db.Tx) error {
			return d.store.RemoveCollabByUsernameAndRepo(ctx, tx, username, repo)
		}),
	); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return proto.ErrCollaboratorNotFound
		}

		return err
	}

	// The removal is committed, so the event is sent without affecting the result.
	d.sendAfterChange(ctx, wh)
	return nil
}
