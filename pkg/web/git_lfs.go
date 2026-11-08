package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/gorilla/mux"
)

// Limits on LFS JSON request bodies. The batch limits are shared with the
// SSH transfer protocol. Verify and lock requests carry a single object or
// path.
const (
	maxLfsBatchObjects  = lfs.MaxBatchObjects
	maxLfsBatchBodySize = lfs.MaxBatchRequestSize
	maxLfsJSONBodySize  = 64 << 10 // 64 KiB
)

// lfsBodyTooLarge answers 413 when err comes from a body limited with
// http.MaxBytesReader, and reports whether it did.
func lfsBodyTooLarge(w http.ResponseWriter, r *http.Request, logger *log.Logger, err error) bool {
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		return false
	}
	logger.Warn("request body too large", "limit", maxErr.Limit)
	renderJSON(w, r, http.StatusRequestEntityTooLarge, lfs.ErrorResponse{
		Message: "request too large",
	})
	return true
}

// serviceLfsBatch handles a Git LFS batch requests.
// https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md
// TODO: support refname
// POST: /<repo>.git/info/lfs/objects/batch
func serviceLfsBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.FromContext(ctx).WithPrefix("http.lfs")

	if !lfsMediaTypes(w, r) {
		return
	}

	var batchRequest lfs.BatchRequest
	defer r.Body.Close() //nolint: errcheck
	body := http.MaxBytesReader(w, r.Body, maxLfsBatchBodySize)
	if err := json.NewDecoder(body).Decode(&batchRequest); err != nil {
		if lfsBodyTooLarge(w, r, logger, err) {
			return
		}
		logger.Info("error decoding json", "err", err)
		renderJSON(w, r, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "validation error in request: " + err.Error(),
		})
		return
	}

	// We only accept basic transfers for now
	// Default to basic if no transfer is specified
	if len(batchRequest.Transfers) > 0 {
		var isBasic bool
		for _, t := range batchRequest.Transfers {
			if t == lfs.TransferBasic {
				isBasic = true
				break
			}
		}

		if !isBasic {
			renderJSON(w, r, http.StatusUnprocessableEntity, lfs.ErrorResponse{
				Message: "unsupported transfer",
			})
			return
		}
	}

	// Object IDs are SHA-256 hashes; the batch API answers 409 to any other
	// hash algorithm.
	if batchRequest.HashAlgo != "" && batchRequest.HashAlgo != lfs.HashAlgorithmSHA256 {
		renderJSON(w, r, http.StatusConflict, lfs.ErrorResponse{
			Message: "unsupported hash algorithm: " + batchRequest.HashAlgo,
		})
		return
	}

	if len(batchRequest.Objects) == 0 {
		renderJSON(w, r, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "no objects found",
		})
		return
	}

	// The batch API answers 413 when a request has too many objects.
	if len(batchRequest.Objects) > maxLfsBatchObjects {
		logger.Warn("too many objects in batch request", "objects", len(batchRequest.Objects))
		renderJSON(w, r, http.StatusRequestEntityTooLarge, lfs.ErrorResponse{
			Message: fmt.Sprintf("too many objects in batch request: %d, the maximum is %d", len(batchRequest.Objects), maxLfsBatchObjects),
		})
		return
	}

	name := mux.Vars(r)["repo"]
	repo, ok := lfsRepository(w, r)
	if !ok {
		return
	}

	cfg := config.FromContext(ctx)
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	// TODO: support S3 storage
	repoID := strconv.FormatInt(repo.ID(), 10)
	strg := storage.NewLocalStorage(filepath.Join(cfg.DataPath, "lfs", repoID))

	baseHref := fmt.Sprintf("%s/%s/info/lfs/objects/basic", cfg.HTTP.PublicURL, name+".git")

	var batchResponse lfs.BatchResponse
	batchResponse.Transfer = lfs.TransferBasic
	batchResponse.HashAlgo = lfs.HashAlgorithmSHA256

	objects := make([]*lfs.ObjectResponse, 0, len(batchRequest.Objects))
	// XXX: We don't support objects TTL for now, probably implement that with
	// S3 using object "expires_at" & "expires_in"
	switch batchRequest.Operation {
	case lfs.OperationDownload:
		for _, o := range batchRequest.Objects {
			// Object IDs become storage paths, so validate before touching the
			// filesystem.
			if !o.IsValid() {
				logger.Info("invalid object", "oid", o.Oid, "repo", name)
				objects = append(objects, &lfs.ObjectResponse{
					Pointer: o,
					Error: &lfs.ObjectError{
						Code:    http.StatusUnprocessableEntity,
						Message: "invalid object",
					},
				})
				continue
			}

			exist, err := strg.Exists(path.Join("objects", o.RelativePath()))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				logger.Error("error getting object stat", "oid", o.Oid, "repo", name, "err", err)
				renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
					Message: "internal server error",
				})
				return
			}

			obj, err := datastore.GetLFSObjectByOid(ctx, dbx, repo.ID(), o.Oid)
			if err != nil && !errors.Is(err, db.ErrRecordNotFound) {
				logger.Error("error getting object from database", "oid", o.Oid, "repo", name, "err", err)
				renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
					Message: "internal server error",
				})
				return
			}

			if !exist {
				objects = append(objects, &lfs.ObjectResponse{
					Pointer: o,
					Error: &lfs.ObjectError{
						Code:    http.StatusNotFound,
						Message: "object not found",
					},
				})
			} else if obj.Size != o.Size {
				objects = append(objects, &lfs.ObjectResponse{
					Pointer: o,
					Error: &lfs.ObjectError{
						Code:    http.StatusUnprocessableEntity,
						Message: "size mismatch",
					},
				})
			} else {
				download := &lfs.Link{
					Href: fmt.Sprintf("%s/%s", baseHref, o.Oid),
				}
				if auth := r.Header.Get("Authorization"); auth != "" {
					download.Header = map[string]string{
						"Authorization": auth,
					}
				}

				objects = append(objects, &lfs.ObjectResponse{
					Pointer: o,
					Actions: map[string]*lfs.Link{
						lfs.ActionDownload: download,
					},
				})

				// If the object doesn't exist in the database, create it
				if exist && obj.ID == 0 {
					if err := datastore.CreateLFSObject(ctx, dbx, repo.ID(), o.Oid, o.Size); err != nil {
						logger.Error("error creating object in datastore", "oid", o.Oid, "repo", name, "err", err)
						renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
							Message: "internal server error",
						})
						return
					}
				}
			}
		}
	case lfs.OperationUpload:
		// Check authorization
		accessLevel := access.FromContext(ctx)
		if accessLevel < access.ReadWriteAccess {
			denyLFSWrite(w, r)
			return
		}

		// Object upload logic happens in the "basic" API route
		for _, o := range batchRequest.Objects {
			if !o.IsValid() {
				objects = append(objects, &lfs.ObjectResponse{
					Pointer: o,
					Error: &lfs.ObjectError{
						Code:    http.StatusUnprocessableEntity,
						Message: "invalid object",
					},
				})
				continue
			}

			// The batch API omits actions for objects the server already
			// has, so the client does not send them again.
			// https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md#successful-responses
			present, err := lfsObjectPresent(ctx, strg, dbx, datastore, repo.ID(), o)
			if err != nil {
				logger.Error("error checking object", "oid", o.Oid, "repo", name, "err", err)
				renderJSON(w, http.StatusInternalServerError, lfs.ErrorResponse{
					Message: "internal server error",
				})
				return
			}
			if present {
				objects = append(objects, &lfs.ObjectResponse{Pointer: o})
			} else {
				upload := &lfs.Link{
					Href: fmt.Sprintf("%s/%s", baseHref, o.Oid),
					Header: map[string]string{
						// NOTE: git-lfs v2.5.0 sets the Content-Type based on the uploaded file.
						// This ensures that the client always uses the designated value for the header.
						"Content-Type": "application/octet-stream",
					},
				}
				verify := &lfs.Link{
					Href: fmt.Sprintf("%s/verify", baseHref),
				}
				if auth := r.Header.Get("Authorization"); auth != "" {
					upload.Header["Authorization"] = auth
					verify.Header = map[string]string{
						"Authorization": auth,
					}
				}

				objects = append(objects, &lfs.ObjectResponse{
					Pointer: o,
					Actions: map[string]*lfs.Link{
						lfs.ActionUpload: upload,
						// Verify uploaded objects
						// https://github.com/git-lfs/git-lfs/blob/main/docs/api/basic-transfers.md#verification
						lfs.ActionVerify: verify,
					},
				})
			}
		}
	default:
		renderJSON(w, r, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "unsupported operation",
		})
		return
	}

	batchResponse.Objects = objects
	renderJSON(w, r, http.StatusOK, batchResponse)
}

// lfsObjectPresent reports whether the repository already stores the object
// with the given size: it has a database row of that size and its file is in
// storage.
func lfsObjectPresent(ctx context.Context, strg storage.Storage, dbx *db.DB, datastore store.Store, repoID int64, p lfs.Pointer) (bool, error) {
	obj, err := datastore.GetLFSObjectByOid(ctx, dbx, repoID, p.Oid)
	if errors.Is(err, db.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if obj.Size != p.Size {
		return false, nil
	}
	exist, err := strg.Exists(path.Join("objects", p.RelativePath()))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return exist, nil
}

// serviceLfsBasic implements Git LFS basic transfer API
// https://github.com/git-lfs/git-lfs/blob/main/docs/api/basic-transfers.md
func serviceLfsBasic(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serviceLfsBasicDownload(w, r)
	case http.MethodPut:
		serviceLfsBasicUpload(w, r)
	}
}

// GET: /<repo>.git/info/lfs/objects/basic/<oid>
func serviceLfsBasicDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	oid := mux.Vars(r)["oid"]
	repo, ok := lfsRepository(w, r)
	if !ok {
		return
	}

	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("http.lfs-basic")
	datastore := store.FromContext(ctx)
	dbx := db.FromContext(ctx)
	repoID := strconv.FormatInt(repo.ID(), 10)
	strg := storage.NewLocalStorage(filepath.Join(cfg.DataPath, "lfs", repoID))

	obj, err := datastore.GetLFSObjectByOid(ctx, dbx, repo.ID(), oid)
	if err != nil && !errors.Is(err, db.ErrRecordNotFound) {
		logger.Error("error getting object from database", "oid", oid, "repo", repo.Name(), "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	pointer := lfs.Pointer{Oid: oid}
	f, err := strg.Open(path.Join("objects", pointer.RelativePath()))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			logger.Info("object not found", "oid", oid)
		} else {
			logger.Error("error opening object", "oid", oid, "err", err)
		}
		renderJSON(w, r, http.StatusNotFound, lfs.ErrorResponse{
			Message: "object not found",
		})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	defer f.Close() //nolint: errcheck
	if _, err := io.Copy(w, f); err != nil {
		logger.Error("error copying object to response", "oid", oid, "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}
}

// PUT: /<repo>.git/info/lfs/objects/basic/<oid>
func serviceLfsBasicUpload(w http.ResponseWriter, r *http.Request) {
	if !hasMediaType(r, "application/octet-stream") {
		renderJSON(w, r, http.StatusUnsupportedMediaType, lfs.ErrorResponse{
			Message: "invalid content type",
		})
		return
	}

	ctx := r.Context()

	// Checked in withAccess too. Repeated here so a routing or middleware
	// mistake cannot turn into an unauthenticated write.
	if access.FromContext(ctx) < access.ReadWriteAccess {
		denyLFSWrite(w, r)
		return
	}

	oid := mux.Vars(r)["oid"]
	cfg := config.FromContext(ctx)
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("http.lfs-basic")
	repo, ok := lfsRepository(w, r)
	if !ok {
		return
	}

	repoID := strconv.FormatInt(repo.ID(), 10)
	strg := storage.NewLocalStorage(filepath.Join(cfg.DataPath, "lfs", repoID))

	defer r.Body.Close() //nolint: errcheck

	// NOTE: Git LFS client will retry uploading the same object if there was a
	// partial error, so we need to skip existing objects.
	if _, err := datastore.GetLFSObjectByOid(ctx, dbx, repo.ID(), oid); err == nil {
		// Object exists, skip request
		io.Copy(io.Discard, r.Body) //nolint: errcheck
		renderStatus(http.StatusOK)(w, nil)
		return
	} else if !errors.Is(err, db.ErrRecordNotFound) {
		logger.Error("error getting object", "oid", oid, "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	// The declared size is the content length. net/http has already rejected
	// an invalid Content-Length header; -1 means the length is unknown, as
	// with a chunked body, and the object cannot be checked against it.
	size := r.ContentLength
	if size < 0 {
		logger.Info("missing content length", "oid", oid)
		renderJSON(w, r, http.StatusLengthRequired, lfs.ErrorResponse{
			Message: "content length required",
		})
		return
	}

	err := lfs.StoreObject(ctx, strg, dbx, datastore, repo.ID(), oid, size, r.Body)
	switch {
	case errors.Is(err, lfs.ErrObjectSizeMismatch):
		logger.Info("object size mismatch", "oid", oid, "err", err)
		renderJSON(w, r, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "object size does not match content length",
		})
		return
	case errors.Is(err, lfs.ErrObjectOidMismatch):
		logger.Info("object hash mismatch", "oid", oid)
		renderJSON(w, r, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "object content does not match oid",
		})
		return
	case errors.Is(err, lfs.ErrObjectRead):
		// The client aborted or truncated the upload.
		logger.Info("error reading object", "oid", oid, "err", err)
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "error reading object",
		})
		return
	case err != nil:
		logger.Error("error storing object", "oid", oid, "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	renderStatus(http.StatusOK)(w, nil)
}

// POST: /<repo>.git/info/lfs/objects/basic/verify
func serviceLfsBasicVerify(w http.ResponseWriter, r *http.Request) {
	if !lfsMediaTypes(w, r) {
		return
	}

	var pointer lfs.Pointer
	ctx := r.Context()
	logger := log.FromContext(ctx).WithPrefix("http.lfs-basic")
	repo, ok := lfsRepository(w, r)
	if !ok {
		return
	}

	defer r.Body.Close() //nolint: errcheck
	body := http.MaxBytesReader(w, r.Body, maxLfsJSONBodySize)
	if err := json.NewDecoder(body).Decode(&pointer); err != nil {
		if lfsBodyTooLarge(w, r, logger, err) {
			return
		}
		logger.Info("error decoding json", "err", err)
		renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request: " + err.Error(),
		})
		return
	}

	// Object IDs become storage paths, so validate before touching the
	// filesystem.
	if !pointer.IsValid() {
		logger.Info("invalid object", "oid", pointer.Oid)
		renderJSON(w, r, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "invalid object",
		})
		return
	}

	cfg := config.FromContext(ctx)
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	repoID := strconv.FormatInt(repo.ID(), 10)
	strg := storage.NewLocalStorage(filepath.Join(cfg.DataPath, "lfs", repoID))
	if stat, err := strg.Stat(path.Join("objects", pointer.RelativePath())); err == nil {
		// Verify object is in the database.
		obj, err := datastore.GetLFSObjectByOid(ctx, dbx, repo.ID(), pointer.Oid)
		if err != nil {
			if errors.Is(err, db.ErrRecordNotFound) {
				logger.Info("object not found", "oid", pointer.Oid)
				renderJSON(w, r, http.StatusNotFound, lfs.ErrorResponse{
					Message: "object not found",
				})
				return
			}
			logger.Error("error getting object", "oid", pointer.Oid, "err", err)
			renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		if obj.Size != pointer.Size {
			renderJSON(w, r, http.StatusBadRequest, lfs.ErrorResponse{
				Message: "object size mismatch",
			})
			return
		}

		if stat.Size() == pointer.Size {
			renderStatus(http.StatusOK)(w, nil)
			return
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		logger.Info("object not found", "oid", pointer.Oid)
		renderJSON(w, r, http.StatusNotFound, lfs.ErrorResponse{
			Message: "object not found",
		})
		return
	} else {
		logger.Error("error getting object", "oid", pointer.Oid, "err", err)
		renderJSON(w, r, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}
}

// renderJSON renders a JSON response with the given status code and value. It
// also sets the Content-Type header to the JSON LFS media type (application/vnd.git-lfs+json).
func renderJSON(w http.ResponseWriter, r *http.Request, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", lfs.MediaType)
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The client has most likely gone away.
		log.FromContext(r.Context()).Debug("error encoding json", "err", err)
	}
}

// lfsAccepts answers 406 unless the client accepts the LFS media type, and
// reports whether it does.
func lfsAccepts(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Accept"), lfs.MediaType) {
		renderJSON(w, r, http.StatusNotAcceptable, lfs.ErrorResponse{
			Message: "Not Acceptable: the Accept header must be " + lfs.MediaType,
		})
		return false
	}
	return true
}

// lfsMediaTypes answers 406 unless the client accepts the LFS media type,
// and 415 unless it sends a body of that type, and reports whether both hold.
func lfsMediaTypes(w http.ResponseWriter, r *http.Request) bool {
	if !lfsAccepts(w, r) {
		return false
	}
	if !hasMediaType(r, lfs.MediaType) {
		renderJSON(w, r, http.StatusUnsupportedMediaType, lfs.ErrorResponse{
			Message: "Unsupported Media Type: the Content-Type header must be " + lfs.MediaType,
		})
		return false
	}
	return true
}

// lfsRepository returns the repository that withAccess stored in the request
// context, answering 404 if there is none.
func lfsRepository(w http.ResponseWriter, r *http.Request) (proto.Repository, bool) {
	repo := proto.RepositoryFromContext(r.Context())
	if repo == nil {
		log.FromContext(r.Context()).Error("error getting repository from context")
		renderJSON(w, r, http.StatusNotFound, lfs.ErrorResponse{
			Message: "repository not found",
		})
		return nil, false
	}
	return repo, true
}
