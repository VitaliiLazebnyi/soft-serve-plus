package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/gorilla/mux"
)

func (s *server) serviceLfsLocks(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	// gitRoute has already refused any other method.
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.serviceLfsLocksGet(w, r, gr)
	case http.MethodPost:
		s.serviceLfsLocksCreate(w, r, gr)
	}
}

// POST: /<repo>.git/info/lfs/locks
func (s *server) serviceLfsLocksCreate(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	if !lfsMediaTypes(w, r, gr.logger) {
		return
	}

	ctx := r.Context()
	logger := gr.logger.WithPrefix("http.lfs-locks")

	var req lfs.LockCreateRequest
	body := http.MaxBytesReader(w, r.Body, maxLfsJSONBodySize)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		if lfsBodyTooLarge(w, r, logger, err) {
			return
		}
		logger.Info("error decoding json", "err", err)
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request: " + err.Error(),
		})
		return
	}

	// A lock names one file in the working tree. Refuse a path that cannot,
	// so "a.bin", "./a.bin" and "dir/../a.bin" do not lock one file thrice.
	if err := lfs.ValidateLockPath(req.Path); err != nil {
		renderJSON(w, gr.logger, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "invalid lock path",
		})
		return
	}

	repo := gr.repo

	// A lock has an owner, so even with anonymous write access the caller
	// must sign in. git-lfs only asks for credentials after a 401.
	user := gr.user
	if user == nil {
		logger.Info("anonymous caller cannot create a lock")
		askCredentials(w, r)
		renderJSON(w, gr.logger, http.StatusUnauthorized, lfs.ErrorResponse{
			Message: "credentials needed",
		})
		return
	}

	dbx := s.dbx
	datastore := s.datastore
	lock, err := datastore.CreateLFSLockForUser(ctx, dbx, repo.ID(), user.ID(), req.Path, req.Ref.Name)
	if err != nil {
		// A repository deleted meanwhile fails the insert on its foreign
		// key; report it as gone.
		if errors.Is(store.RepoGoneError(ctx, datastore, dbx, repo.ID(), err), proto.ErrRepoNotFound) {
			renderJSON(w, gr.logger, http.StatusNotFound, lfs.ErrorResponse{Message: "repository not found"})
			return
		}
		if errors.Is(err, db.ErrDuplicateKey) {
			errResp := lfs.LockResponse{
				ErrorResponse: lfs.ErrorResponse{
					Message: "lock already exists",
				},
			}
			// The conflicting lock may be anyone's; the spec returns it.
			lock, err := datastore.GetLFSLockForPath(ctx, dbx, repo.ID(), req.Path)
			if err == nil {
				errResp.Lock = lfs.Lock{
					ID:       strconv.FormatInt(lock.ID, 10),
					Path:     lock.Path,
					LockedAt: lock.CreatedAt,
				}
				errResp.Lock.Owner = lfs.Owner{Name: lock.OwnerName}
			}
			renderJSON(w, gr.logger, http.StatusConflict, errResp)
			return
		}
		logger.Error("error creating lock", "err", err)
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	renderJSON(w, gr.logger, http.StatusCreated, lfs.LockResponse{
		Lock: lfs.Lock{
			ID:       strconv.FormatInt(lock.ID, 10),
			Path:     lock.Path,
			LockedAt: lock.CreatedAt,
			Owner: lfs.Owner{
				Name: user.Username(),
			},
		},
	})
}

// locksLimit checks the limit of a lock list or verify request. A negative
// limit is refused; otherwise lfs.ClampLocksLimit applies.
func locksLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, errors.New("invalid limit")
	}
	return lfs.ClampLocksLimit(limit), nil
}

// GET: /<repo>.git/info/lfs/locks
func (s *server) serviceLfsLocksGet(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	if !lfsAccepts(w, r, gr.logger) {
		return
	}

	ctx := r.Context()
	// TODO: respect refspec
	values := r.URL.Query()
	path := values.Get("path")
	var id int64
	if idStr := values.Get("id"); idStr != "" {
		var err error
		id, err = strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
				Message: "invalid lock id",
			})
			return
		}
	}
	cursor, err := lfs.ParseLocksCursor(values.Get("cursor"))
	if err != nil {
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: err.Error(),
		})
		return
	}
	var limit int
	if limitStr := values.Get("limit"); limitStr != "" {
		limit, err = strconv.Atoi(limitStr)
		if err != nil {
			renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
				Message: "invalid limit",
			})
			return
		}
	}
	limit, err = locksLimit(limit)
	if err != nil {
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	logger := gr.logger.WithPrefix("http.lfs-locks")
	dbx := s.dbx
	datastore := s.datastore
	repo := gr.repo

	if id > 0 {
		lock, err := datastore.GetLFSLockByID(ctx, dbx, repo.ID(), id)
		if err != nil {
			if errors.Is(err, db.ErrRecordNotFound) {
				// A filter that matches nothing is an empty list.
				renderJSON(w, gr.logger, http.StatusOK, lfs.LockListResponse{Locks: []lfs.Lock{}})
				return
			}
			logger.Error("error getting lock", "err", err)
			renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		renderJSON(w, gr.logger, http.StatusOK, lfs.LockListResponse{
			Locks: []lfs.Lock{
				{
					ID:       strconv.FormatInt(lock.ID, 10),
					Path:     lock.Path,
					LockedAt: lock.CreatedAt,
					Owner: lfs.Owner{
						Name: lock.OwnerName,
					},
				},
			},
		})
		return
	} else if path != "" {
		lock, err := datastore.GetLFSLockForPath(ctx, dbx, repo.ID(), path)
		if err != nil {
			if errors.Is(err, db.ErrRecordNotFound) {
				// A filter that matches nothing is an empty list.
				renderJSON(w, gr.logger, http.StatusOK, lfs.LockListResponse{Locks: []lfs.Lock{}})
				return
			}
			logger.Error("error getting lock", "err", err)
			renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		renderJSON(w, gr.logger, http.StatusOK, lfs.LockListResponse{
			Locks: []lfs.Lock{
				{
					ID:       strconv.FormatInt(lock.ID, 10),
					Path:     lock.Path,
					LockedAt: lock.CreatedAt,
					Owner: lfs.Owner{
						Name: lock.OwnerName,
					},
				},
			},
		})
		return
	}

	locks, err := datastore.GetLFSLocks(ctx, dbx, repo.ID(), cursor, limit)
	if err != nil {
		logger.Error("error getting locks", "err", err)
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	lockList := make([]lfs.Lock, len(locks))
	for i, lock := range locks {
		lockList[i] = lfs.Lock{
			ID:       strconv.FormatInt(lock.ID, 10),
			Path:     lock.Path,
			LockedAt: lock.CreatedAt,
			Owner: lfs.Owner{
				Name: lock.OwnerName,
			},
		}
	}

	resp := lfs.LockListResponse{
		Locks: lockList,
	}
	resp.NextCursor = lfs.NextLocksCursor(limit, locks)

	renderJSON(w, gr.logger, http.StatusOK, resp)
}

// POST: /<repo>.git/info/lfs/locks/verify
func (s *server) serviceLfsLocksVerify(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	if !lfsMediaTypes(w, r, gr.logger) {
		return
	}

	ctx := r.Context()
	logger := gr.logger.WithPrefix("http.lfs-locks")
	repo := gr.repo

	var req lfs.LockVerifyRequest
	body := http.MaxBytesReader(w, r.Body, maxLfsJSONBodySize)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		if lfsBodyTooLarge(w, r, logger, err) {
			return
		}
		logger.Info("error decoding request", "err", err)
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request: " + err.Error(),
		})
		return
	}

	// TODO: refspec
	cursor, err := lfs.ParseLocksCursor(req.Cursor)
	if err != nil {
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: err.Error(),
		})
		return
	}
	limit, err := locksLimit(req.Limit)
	if err != nil {
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	dbx := s.dbx
	datastore := s.datastore
	user := gr.user
	ours := make([]lfs.Lock, 0)
	theirs := make([]lfs.Lock, 0)

	var resp lfs.LockVerifyResponse
	locks, err := datastore.GetLFSLocks(ctx, dbx, repo.ID(), cursor, limit)
	if err != nil {
		logger.Error("error getting locks", "err", err)
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	for _, lock := range locks {
		l := lfs.Lock{
			ID:       strconv.FormatInt(lock.ID, 10),
			Path:     lock.Path,
			LockedAt: lock.CreatedAt,
			Owner: lfs.Owner{
				Name: lock.OwnerName,
			},
		}

		if user != nil && user.ID() == lock.UserID {
			ours = append(ours, l)
		} else {
			theirs = append(theirs, l)
		}
	}

	resp.Ours = ours
	resp.Theirs = theirs

	resp.NextCursor = lfs.NextLocksCursor(limit, locks)

	renderJSON(w, gr.logger, http.StatusOK, resp)
}

// POST: /<repo>.git/info/lfs/locks/:lockID/unlock
func (s *server) serviceLfsLocksDelete(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	if !lfsMediaTypes(w, r, gr.logger) {
		return
	}

	ctx := r.Context()
	logger := gr.logger.WithPrefix("http.lfs-locks")
	lockIDStr := mux.Vars(r)["lock_id"]
	if lockIDStr == "" {
		logger.Info("missing lock id")
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request",
		})
		return
	}

	lockID, err := strconv.ParseInt(lockIDStr, 10, 64)
	if err != nil {
		logger.Info("error parsing lock id", "err", err)
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request",
		})
		return
	}

	var req lfs.LockDeleteRequest
	body := http.MaxBytesReader(w, r.Body, maxLfsJSONBodySize)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		if lfsBodyTooLarge(w, r, logger, err) {
			return
		}
		logger.Info("error decoding request", "err", err)
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request: " + err.Error(),
		})
		return
	}

	// Only a lock's owner or an admin may unlock, so even with anonymous
	// write access the caller must sign in. git-lfs only asks for
	// credentials after a 401.
	user := gr.user
	if user == nil {
		logger.Info("anonymous caller cannot delete a lock")
		askCredentials(w, r)
		renderJSON(w, gr.logger, http.StatusUnauthorized, lfs.ErrorResponse{
			Message: "credentials needed",
		})
		return
	}

	dbx := s.dbx
	datastore := s.datastore
	repo := gr.repo

	// The lock being deleted. Looked up scoped to this repository so that a
	// lock ID belonging to another repository cannot be read through it.
	lock, err := datastore.GetLFSLockByID(ctx, dbx, repo.ID(), lockID)
	if err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			logger.Info("lock not found", "id", lockID)
		} else {
			logger.Error("error getting lock", "err", err)
		}
		renderJSON(w, gr.logger, http.StatusNotFound, lfs.ErrorResponse{
			Message: "lock not found",
		})
		return
	}

	l := lfs.Lock{
		ID:       strconv.FormatInt(lock.ID, 10),
		Path:     lock.Path,
		LockedAt: lock.CreatedAt,
		Owner: lfs.Owner{
			Name: lock.OwnerName,
		},
	}

	// Force delete another user's lock (requires admin privileges)
	if req.Force {
		if !user.IsAdmin() {
			logger.Info("non-admin user attempted force delete", "user", user.Username())
			renderJSON(w, gr.logger, http.StatusForbidden, lfs.ErrorResponse{
				Message: "admin access required for force delete",
			})
			return
		}

		if err := datastore.DeleteLFSLock(ctx, dbx, repo.ID(), lockID); err != nil {
			logger.Error("error deleting lock", "err", err)
			renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		renderJSON(w, gr.logger, http.StatusOK, lfs.LockResponse{Lock: l})
		return
	}

	// Delete our own lock - verify ownership
	if lock.UserID != user.ID() {
		logger.Info("refusing to delete another user's lock", "user", user.Username(), "id", lockID)
		renderJSON(w, gr.logger, http.StatusForbidden, lfs.ErrorResponse{
			Message: "lock belongs to another user",
		})
		return
	}

	if err := datastore.DeleteLFSLock(ctx, dbx, repo.ID(), lockID); err != nil {
		logger.Error("error deleting lock", "err", err)
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	renderJSON(w, gr.logger, http.StatusOK, lfs.LockResponse{Lock: l})
}
