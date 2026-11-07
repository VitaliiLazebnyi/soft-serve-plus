package models

import (
	"database/sql"
	"time"
)

// LFSObject is a Git LFS object.
type LFSObject struct {
	ID        int64     `db:"id"`
	Oid       string    `db:"oid"`
	Size      int64     `db:"size"`
	RepoID    int64     `db:"repo_id"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// LFSLock is a Git LFS lock.
type LFSLock struct {
	ID        int64          `db:"id"`
	Path      string         `db:"path"`
	UserID    int64          `db:"user_id"`
	RepoID    int64          `db:"repo_id"`
	Refname   sql.NullString `db:"refname"`
	CreatedAt time.Time      `db:"created_at"`
	UpdatedAt time.Time      `db:"updated_at"`
}
