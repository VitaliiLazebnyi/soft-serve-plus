package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/gorilla/mux"
)

func serviceLfsLocks(w http.ResponseWriter, r *http.Request) {
	// GitRoute has already refused any other method.
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serviceLfsLocksGet(w, r)
	case http.MethodPost:
		serviceLfsLocksCreate(w, r)
	}
}

// POST: /<repo>.git/info/lfs/locks
func serviceLfsLocksCreate(w http.ResponseWriter, r *http.Request) {
	if !lfsMediaTypes(w, r) {
		return
	}

	ctx := r.Context()
	logger := log.FromContext(ctx).WithPrefix("http.lfs-locks")

	// Checked in withAccess too. Repeated here so a routing or middleware
	// mistake cannot turn into an unauthorized lock.
	if access.FromContext(ctx) < access.ReadWriteAccess {
		denyLFSWrite(w, r)
		return
	}

	var req lfs.LockCreateRequest
	body := http.MaxBytesReader(w, r.Body, maxLfsJSONBodySize)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		if lfsBodyTooLarge(w, r, logger, err) {
			return
		}
		logger.Info("error decoding json", "err", err)
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request: " + err.Error(),
		})
		return
	}

	repo, ok := lfsRepository(w, r)
	if !ok {
		return
	}

	// A lock has an owner, so even with anonymous write access the caller
	// must sign in. git-lfs only asks for credentials after a 401.
	user := proto.UserFromContext(ctx)
	if user == nil {
		logger.Info("anonymous caller cannot create a lock")
		askCredentials(w, r)
		renderJSON(w, r, http.StatusUnauthorized, lfs.ErrorResponse{
			Message: "credentials needed",
		})
		return
	}

	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	if err := datastore.CreateLFSLockForUser(ctx, dbx, repo.ID(), user.ID(), req.Path, req.Ref.Name); err != nil {
		err = db.WrapError(err)
		// A repository deleted meanwhile fails the insert on its foreign
		// key too; that is not an existing lock.
		if errors.Is(store.RepoGoneError(ctx, datastore, dbx, repo.ID(), err), proto.ErrRepoNotFound) {
			renderJSON(w, r, http.StatusNotFound, lfs.ErrorResponse{Message: "repository not found"})
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
				lockOwner := lfs.Owner{
					Name: user.Username(),
				}
				if lock.UserID != user.ID() {
					owner, err := datastore.GetUserByID(ctx, dbx, lock.UserID)
					if err != nil {
						logger.Error("error getting lock owner", "err", err)
						renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
							Message: "internal server error",
						})
						return
					}
					lockOwner.Name = owner.Username
				}
				errResp.Lock.Owner = lockOwner
			}
			renderJSON(w, r, http.StatusConflict, errResp)
			return
		}
		logger.Error("error creating lock", "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	lock, err := datastore.GetLFSLockForUserPath(ctx, dbx, repo.ID(), user.ID(), req.Path)
	if err != nil {
		logger.Error("error getting lock", "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	renderJSON(w, r, http.StatusCreated, lfs.LockResponse{
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
func serviceLfsLocksGet(w http.ResponseWriter, r *http.Request) {
	if !lfsAccepts(w, r) {
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
			renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
				Message: "invalid lock id",
			})
			return
		}
	}
	cursor, err := lfs.ParseLocksCursor(values.Get("cursor"))
	if err != nil {
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: err.Error(),
		})
		return
	}
	var limit int
	if limitStr := values.Get("limit"); limitStr != "" {
		limit, err = strconv.Atoi(limitStr)
		if err != nil {
			renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
				Message: "invalid limit",
			})
			return
		}
	}
	limit, err = locksLimit(limit)
	if err != nil {
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	logger := log.FromContext(ctx).WithPrefix("http.lfs-locks")
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	repo, ok := lfsRepository(w, r)
	if !ok {
		return
	}

	if id > 0 {
		lock, err := datastore.GetLFSLockByID(ctx, dbx, repo.ID(), id)
		if err != nil {
			if errors.Is(err, db.ErrRecordNotFound) {
				// A filter that matches nothing is an empty list.
				renderJSON(w, r, http.StatusOK, lfs.LockListResponse{Locks: []lfs.Lock{}})
				return
			}
			logger.Error("error getting lock", "err", err)
			renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		owner, err := datastore.GetUserByID(ctx, dbx, lock.UserID)
		if err != nil {
			logger.Error("error getting lock owner", "err", err)
			renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		renderJSON(w, r, http.StatusOK, lfs.LockListResponse{
			Locks: []lfs.Lock{
				{
					ID:       strconv.FormatInt(lock.ID, 10),
					Path:     lock.Path,
					LockedAt: lock.CreatedAt,
					Owner: lfs.Owner{
						Name: owner.Username,
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
				renderJSON(w, r, http.StatusOK, lfs.LockListResponse{Locks: []lfs.Lock{}})
				return
			}
			logger.Error("error getting lock", "err", err)
			renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		owner, err := datastore.GetUserByID(ctx, dbx, lock.UserID)
		if err != nil {
			logger.Error("error getting lock owner", "err", err)
			renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		renderJSON(w, r, http.StatusOK, lfs.LockListResponse{
			Locks: []lfs.Lock{
				{
					ID:       strconv.FormatInt(lock.ID, 10),
					Path:     lock.Path,
					LockedAt: lock.CreatedAt,
					Owner: lfs.Owner{
						Name: owner.Username,
					},
				},
			},
		})
		return
	}

	locks, err := datastore.GetLFSLocks(ctx, dbx, repo.ID(), cursor, limit)
	if err != nil {
		logger.Error("error getting locks", "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	lockList := make([]lfs.Lock, len(locks))
	users := map[int64]models.User{}
	for i, lock := range locks {
		owner, ok := users[lock.UserID]
		if !ok {
			owner, err = datastore.GetUserByID(ctx, dbx, lock.UserID)
			if err != nil {
				logger.Error("error getting lock owner", "err", err)
				renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
					Message: "internal server error",
				})
				return
			}
			users[lock.UserID] = owner
		}

		lockList[i] = lfs.Lock{
			ID:       strconv.FormatInt(lock.ID, 10),
			Path:     lock.Path,
			LockedAt: lock.CreatedAt,
			Owner: lfs.Owner{
				Name: owner.Username,
			},
		}
	}

	resp := lfs.LockListResponse{
		Locks: lockList,
	}
	resp.NextCursor = lfs.NextLocksCursor(cursor, limit, len(locks))

	renderJSON(w, r, http.StatusOK, resp)
}

// POST: /<repo>.git/info/lfs/locks/verify
func serviceLfsLocksVerify(w http.ResponseWriter, r *http.Request) {
	if !lfsMediaTypes(w, r) {
		return
	}

	ctx := r.Context()
	logger := log.FromContext(ctx).WithPrefix("http.lfs-locks")
	repo, ok := lfsRepository(w, r)
	if !ok {
		return
	}

	var req lfs.LockVerifyRequest
	body := http.MaxBytesReader(w, r.Body, maxLfsJSONBodySize)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		if lfsBodyTooLarge(w, r, logger, err) {
			return
		}
		logger.Info("error decoding request", "err", err)
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request: " + err.Error(),
		})
		return
	}

	// TODO: refspec
	cursor, err := lfs.ParseLocksCursor(req.Cursor)
	if err != nil {
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: err.Error(),
		})
		return
	}
	limit, err := locksLimit(req.Limit)
	if err != nil {
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	user := proto.UserFromContext(ctx)
	ours := make([]lfs.Lock, 0)
	theirs := make([]lfs.Lock, 0)

	var resp lfs.LockVerifyResponse
	locks, err := datastore.GetLFSLocks(ctx, dbx, repo.ID(), cursor, limit)
	if err != nil {
		logger.Error("error getting locks", "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	users := map[int64]models.User{}
	for _, lock := range locks {
		owner, ok := users[lock.UserID]
		if !ok {
			owner, err = datastore.GetUserByID(ctx, dbx, lock.UserID)
			if err != nil {
				logger.Error("error getting lock owner", "err", err)
				renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
					Message: "internal server error",
				})
				return
			}
			users[lock.UserID] = owner
		}

		l := lfs.Lock{
			ID:       strconv.FormatInt(lock.ID, 10),
			Path:     lock.Path,
			LockedAt: lock.CreatedAt,
			Owner: lfs.Owner{
				Name: owner.Username,
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

	resp.NextCursor = lfs.NextLocksCursor(cursor, limit, len(locks))

	renderJSON(w, r, http.StatusOK, resp)
}

// POST: /<repo>.git/info/lfs/locks/:lockID/unlock
func serviceLfsLocksDelete(w http.ResponseWriter, r *http.Request) {
	if !lfsMediaTypes(w, r) {
		return
	}

	ctx := r.Context()
	logger := log.FromContext(ctx).WithPrefix("http.lfs-locks")
	lockIDStr := mux.Vars(r)["lock_id"]
	if lockIDStr == "" {
		logger.Info("missing lock id")
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request",
		})
		return
	}

	lockID, err := strconv.ParseInt(lockIDStr, 10, 64)
	if err != nil {
		logger.Info("error parsing lock id", "err", err)
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
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
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request: " + err.Error(),
		})
		return
	}

	// Only a lock's owner or an admin may unlock, so even with anonymous
	// write access the caller must sign in. git-lfs only asks for
	// credentials after a 401.
	user := proto.UserFromContext(ctx)
	if user == nil {
		logger.Info("anonymous caller cannot delete a lock")
		askCredentials(w, r)
		renderJSON(w, r, http.StatusUnauthorized, lfs.ErrorResponse{
			Message: "credentials needed",
		})
		return
	}

	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	repo, ok := lfsRepository(w, r)
	if !ok {
		return
	}

	// The lock being deleted. Looked up scoped to this repository so that a
	// lock ID belonging to another repository cannot be read through it.
	lock, err := datastore.GetLFSLockByID(ctx, dbx, repo.ID(), lockID)
	if err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			logger.Info("lock not found", "id", lockID)
		} else {
			logger.Error("error getting lock", "err", err)
		}
		renderJSON(w, r, http.StatusNotFound, lfs.ErrorResponse{
			Message: "lock not found",
		})
		return
	}

	owner, err := datastore.GetUserByID(ctx, dbx, lock.UserID)
	if err != nil {
		logger.Error("error getting lock owner", "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	l := lfs.Lock{
		ID:       strconv.FormatInt(lock.ID, 10),
		Path:     lock.Path,
		LockedAt: lock.CreatedAt,
		Owner: lfs.Owner{
			Name: owner.Username,
		},
	}

	// Force delete another user's lock (requires admin privileges)
	if req.Force {
		if !user.IsAdmin() {
			logger.Info("non-admin user attempted force delete", "user", user.Username())
			renderJSON(w, r, http.StatusForbidden, lfs.ErrorResponse{
				Message: "admin access required for force delete",
			})
			return
		}

		if err := datastore.DeleteLFSLock(ctx, dbx, repo.ID(), lockID); err != nil {
			logger.Error("error deleting lock", "err", err)
			renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		renderJSON(w, r, http.StatusOK, lfs.LockResponse{Lock: l})
		return
	}

	// Delete our own lock - verify ownership
	if owner.ID != user.ID() {
		logger.Info("refusing to delete another user's lock", "user", user.Username(), "id", lockID)
		renderJSON(w, r, http.StatusForbidden, lfs.ErrorResponse{
			Message: "lock belongs to another user",
		})
		return
	}

	if err := datastore.DeleteLFSLock(ctx, dbx, repo.ID(), lockID); err != nil {
		logger.Error("error deleting lock", "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	renderJSON(w, r, http.StatusOK, lfs.LockResponse{Lock: l})
}
