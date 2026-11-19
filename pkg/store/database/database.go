package database

import "github.com/charmbracelet/soft-serve/pkg/store"

// Store implements store.Store on a SQL database. It holds no state: each
// method runs on the db.Handler it is given, the pool or a transaction.
type Store struct{}

var _ store.Store = (*Store)(nil)

// New returns a Store.
func New() *Store {
	return &Store{}
}
