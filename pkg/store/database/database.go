package database

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

type datastore struct {
	*settingsStore
	*repoStore
	*userStore
	*collabStore
	*lfsStore
	*accessTokenStore
	*webhookStore
}

// New returns a new store.Store database.
func New(context.Context, *db.DB) store.Store {
	s := &datastore{
		settingsStore:    &settingsStore{},
		repoStore:        &repoStore{},
		userStore:        &userStore{},
		collabStore:      &collabStore{},
		lfsStore:         &lfsStore{},
		accessTokenStore: &accessTokenStore{},
		webhookStore:     &webhookStore{},
	}

	return s
}
