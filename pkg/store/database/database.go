// Package database implements store.Store with SQL that runs on both SQLite
// and Postgres.
package database

import (
	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// Store implements store.Store on a SQL database. It holds only its logger:
// each method runs on the db.Handler it is given, the pool or a
// transaction.
type Store struct {
	logger *log.Logger
}

var _ store.Store = (*Store)(nil)

// New returns a Store that logs on logger, with the prefix "store". A nil
// logger is log.Default().
func New(logger *log.Logger) *Store {
	if logger == nil {
		logger = log.Default()
	}
	return &Store{logger: logger.WithPrefix("store")}
}

// log returns s's logger, the default one with the prefix "store" for a
// Store not made by New.
func (s *Store) log() *log.Logger {
	if s.logger == nil {
		return log.Default().WithPrefix("store")
	}
	return s.logger
}
