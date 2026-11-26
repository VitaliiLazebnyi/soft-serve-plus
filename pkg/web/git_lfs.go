package web

import (
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
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
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

// lfsActionAuth is the Authorization header a batch response puts on its
// actions, and how long it stays valid.
type lfsActionAuth struct {
	header    string
	expiresIn *int64
}

// lfsActionAuthFor picks the credential to forward to the batch actions. Only
// a bearer JWT is forwarded: it is short-lived, scoped to the repository, and
// is how a client that authenticated with git-lfs-authenticate over SSH
// reaches the object URLs. Its remaining lifetime goes into expires_in so the
// client asks for a new batch before it runs out. An access token, in
// HTTP Basic or not, is not echoed back in the response body; git-lfs sends the
// credentials it used for the batch to the object URLs on the same server.
// When the client reached the server under a different host than the one in
// the HTTP public URL, git-lfs has to find credentials for the public-URL
// host itself (for example from a credential helper, or by prompting).
//
// The token is verified again for repo, the repository of the batch request,
// so a credential is only ever forwarded when it is a valid token for repo.
// logger is the request's logger.
func (s *server) lfsActionAuthFor(r *http.Request, logger *log.Logger, repo string, now time.Time) lfsActionAuth {
	scheme, credential, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || credential == "" {
		return lfsActionAuth{}
	}

	claims, err := s.parseJWT(logger, credential, []string{proto.SanitizeRepo(repo)})
	if err != nil {
		return lfsActionAuth{}
	}
	auth := lfsActionAuth{header: "Bearer " + credential}
	if claims.ExpiresAt != nil {
		left := int64(claims.ExpiresAt.Sub(now) / time.Second)
		if left <= 0 {
			return lfsActionAuth{}
		}
		auth.expiresIn = &left
	}
	return auth
}

// link returns an action for href with header, adding the forwarded
// credential and its lifetime.
func (a lfsActionAuth) link(href string, header map[string]string) *lfs.Link {
	l := &lfs.Link{Href: href, Header: header}
	if a.header != "" {
		if l.Header == nil {
			l.Header = map[string]string{}
		}
		l.Header["Authorization"] = a.header
		l.ExpiresIn = a.expiresIn
	}
	return l
}

// lfsBodyTooLarge answers 413 when err comes from a body limited with
// http.MaxBytesReader, and reports whether it did.
func lfsBodyTooLarge(w http.ResponseWriter, r *http.Request, logger *log.Logger, err error) bool {
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		return false
	}
	logger.Warn("request body too large", "limit", maxErr.Limit)
	renderJSON(w, logger, http.StatusRequestEntityTooLarge, lfs.ErrorResponse{
		Message: "request too large",
	})
	return true
}

// serviceLfsBatch handles a Git LFS batch request. The request's ref is
// ignored: objects belong to the repository, not to a ref.
// https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md
// POST: /<repo>.git/info/lfs/objects/batch
func (s *server) serviceLfsBatch(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	ctx := r.Context()
	logger := gr.logger.WithPrefix("http.lfs")

	defer r.Body.Close() //nolint: errcheck
	batchRequest, ok := decodeLfsBatch(w, r, gr, logger)
	if !ok {
		return
	}

	name := gr.name
	repo := gr.repo

	cfg := s.cfg
	dbx := s.dbx
	datastore := s.datastore
	// Objects are stored on the local file system only.
	repoID := strconv.FormatInt(repo.ID(), 10)
	strg := storage.NewLocal(filepath.Join(cfg.DataPath, "lfs", repoID))

	baseHref := fmt.Sprintf("%s/%s/info/lfs/objects/basic", cfg.HTTP.PublicURL, name+".git")

	var batchResponse lfs.BatchResponse
	batchResponse.Transfer = lfs.TransferBasic
	batchResponse.HashAlgo = lfs.HashAlgorithmSHA256

	// The batch's objects are looked up in one query, not one each.
	oids := make([]string, 0, len(batchRequest.Objects))
	for _, o := range batchRequest.Objects {
		if o.IsValid() {
			oids = append(oids, o.Oid)
		}
	}
	found, err := datastore.GetLFSObjectsByOids(ctx, dbx, repo.ID(), oids)
	if err != nil {
		logger.Error("error getting objects from database", "repo", name, "err", err)
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}
	recorded := make(map[string]models.LFSObject, len(found))
	for _, obj := range found {
		recorded[obj.Oid] = obj
	}

	batch := lfsBatch{
		logger:   logger,
		name:     name,
		strg:     strg,
		recorded: recorded,
		baseHref: baseHref,
		auth:     s.lfsActionAuthFor(r, gr.logger, repo.Name(), time.Now()),
	}
	var objects []*lfs.ObjectResponse
	// Actions carry no expiry (expires_at, expires_in).
	switch batchRequest.Operation {
	case lfs.OperationDownload:
		objects, err = batch.download(batchRequest.Objects)
	case lfs.OperationUpload:
		// Check authorization
		if gr.level < access.ReadWriteAccess {
			denyLFSWrite(w, r, gr)
			return
		}
		objects, err = batch.upload(batchRequest.Objects)
	default:
		renderJSON(w, gr.logger, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "unsupported operation",
		})
		return
	}
	if err != nil {
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	batchResponse.Objects = objects
	renderJSON(w, gr.logger, http.StatusOK, batchResponse)
}

// decodeLfsBatch reads the batch request of r and checks what the batch API
// requires of it before any object is looked up. When the request is
// refused it answers it and reports false. logger is the LFS logger of the
// request.
func decodeLfsBatch(w http.ResponseWriter, r *http.Request, gr *gitRequest, logger *log.Logger) (lfs.BatchRequest, bool) {
	var batchRequest lfs.BatchRequest
	if !lfsMediaTypes(w, r, gr.logger) {
		return batchRequest, false
	}

	body := http.MaxBytesReader(w, r.Body, maxLfsBatchBodySize)
	if err := json.NewDecoder(body).Decode(&batchRequest); err != nil {
		if lfsBodyTooLarge(w, r, logger, err) {
			return batchRequest, false
		}
		logger.Info("error decoding json", "err", err)
		renderJSON(w, gr.logger, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "validation error in request: " + err.Error(),
		})
		return batchRequest, false
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
			renderJSON(w, gr.logger, http.StatusUnprocessableEntity, lfs.ErrorResponse{
				Message: "unsupported transfer",
			})
			return batchRequest, false
		}
	}

	// Object IDs are SHA-256 hashes; the batch API answers 409 to any other
	// hash algorithm.
	if batchRequest.HashAlgo != "" && batchRequest.HashAlgo != lfs.HashAlgorithmSHA256 {
		renderJSON(w, gr.logger, http.StatusConflict, lfs.ErrorResponse{
			Message: "unsupported hash algorithm: " + batchRequest.HashAlgo,
		})
		return batchRequest, false
	}

	if len(batchRequest.Objects) == 0 {
		renderJSON(w, gr.logger, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "no objects found",
		})
		return batchRequest, false
	}

	// The batch API answers 413 when a request has too many objects.
	if len(batchRequest.Objects) > maxLfsBatchObjects {
		logger.Warn("too many objects in batch request", "objects", len(batchRequest.Objects))
		renderJSON(w, gr.logger, http.StatusRequestEntityTooLarge, lfs.ErrorResponse{
			Message: fmt.Sprintf("too many objects in batch request: %d, the maximum is %d", len(batchRequest.Objects), maxLfsBatchObjects),
		})
		return batchRequest, false
	}

	return batchRequest, true
}

// lfsBatch is what answering the objects of one batch request takes.
type lfsBatch struct {
	// logger is the LFS logger of the request.
	logger *log.Logger
	// name is the repository's name, for the log.
	name string
	// strg holds the repository's objects.
	strg storage.Storage
	// recorded holds the database rows of the batch's valid objects, by
	// object ID.
	recorded map[string]models.LFSObject
	// baseHref is the URL the actions' URLs start with.
	baseHref string
	// auth is the credential the actions forward.
	auth lfsActionAuth
}

// lfsObjectError returns the answer for o that is an error.
func lfsObjectError(o lfs.Pointer, code int, msg string) *lfs.ObjectResponse {
	return &lfs.ObjectResponse{
		Pointer: o,
		Error: &lfs.ObjectError{
			Code:    code,
			Message: msg,
		},
	}
}

// download answers the objects of a download batch. An error is a storage
// fault, which it has logged; the request is then answered 500.
func (b lfsBatch) download(objs []lfs.Pointer) ([]*lfs.ObjectResponse, error) {
	objects := make([]*lfs.ObjectResponse, 0, len(objs))
	for _, o := range objs {
		// Object IDs become storage paths, so validate before touching the
		// filesystem.
		if !o.IsValid() {
			b.logger.Info("invalid object", "oid", o.Oid, "repo", b.name)
			objects = append(objects, lfsObjectError(o, http.StatusUnprocessableEntity, "invalid object"))
			continue
		}

		stat, err := b.strg.Stat(path.Join("objects", o.RelativePath()))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			b.logger.Error("error getting object stat", "oid", o.Oid, "repo", b.name, "err", err)
			return nil, err
		}
		exist := err == nil && stat.Mode().IsRegular()

		obj := b.recorded[o.Oid]

		// A download is read-only: the client's size is checked against
		// the file and its record, never recorded. A file without a
		// record (an upload that has not recorded it yet) is offered at
		// the size it has on disk.
		if !exist {
			objects = append(objects, lfsObjectError(o, http.StatusNotFound, "object not found"))
		} else if stat.Size() != o.Size || (obj.ID != 0 && obj.Size != o.Size) {
			objects = append(objects, lfsObjectError(o, http.StatusUnprocessableEntity, "size mismatch"))
		} else {
			download := b.auth.link(fmt.Sprintf("%s/%s", b.baseHref, o.Oid), nil)

			objects = append(objects, &lfs.ObjectResponse{
				Pointer: o,
				Actions: map[string]*lfs.Link{
					lfs.ActionDownload: download,
				},
			})
		}
	}
	return objects, nil
}

// upload answers the objects of an upload batch; the objects themselves
// arrive through the "basic" API route. An error is a storage fault, which
// it has logged; the request is then answered 500.
func (b lfsBatch) upload(objs []lfs.Pointer) ([]*lfs.ObjectResponse, error) {
	objects := make([]*lfs.ObjectResponse, 0, len(objs))
	for _, o := range objs {
		if !o.IsValid() {
			objects = append(objects, lfsObjectError(o, http.StatusUnprocessableEntity, "invalid object"))
			continue
		}

		// The batch API omits actions for objects the server already
		// has, so the client does not send them again.
		// https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md#successful-responses
		obj, ok := b.recorded[o.Oid]
		present, err := lfsObjectPresent(b.strg, obj, ok, o)
		if err != nil {
			b.logger.Error("error checking object", "oid", o.Oid, "repo", b.name, "err", err)
			return nil, err
		}
		if present {
			objects = append(objects, &lfs.ObjectResponse{Pointer: o})
		} else {
			upload := b.auth.link(fmt.Sprintf("%s/%s", b.baseHref, o.Oid), map[string]string{
				// NOTE: git-lfs v2.5.0 sets the Content-Type based on the uploaded file.
				// This ensures that the client always uses the designated value for the header.
				"Content-Type": "application/octet-stream",
			})
			verify := b.auth.link(fmt.Sprintf("%s/verify", b.baseHref), nil)

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
	return objects, nil
}

// lfsObjectPresent reports whether the repository already stores the object
// with the given size: it has a database row of that size and its file is a
// regular file of that size in storage. A missing, truncated or overgrown file
// is not present, so the client sends it again and the upload handler restores
// it. The content is not hashed here, to keep batch requests cheap.
// obj is the object's row, if recorded says it has one.
func lfsObjectPresent(strg storage.Storage, obj models.LFSObject, recorded bool, p lfs.Pointer) (bool, error) {
	if !recorded || obj.Size != p.Size {
		return false, nil
	}
	stat, err := strg.Stat(path.Join("objects", p.RelativePath()))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return stat.Mode().IsRegular() && stat.Size() == p.Size, nil
}

// serviceLfsBasic serves the Git LFS basic transfer API.
// https://github.com/git-lfs/git-lfs/blob/main/docs/api/basic-transfers.md
func (s *server) serviceLfsBasic(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.serviceLfsBasicDownload(w, r, gr)
	case http.MethodPut:
		s.serviceLfsBasicUpload(w, r, gr)
	}
}

// serviceLfsBasicDownload sends an object.
// GET: /<repo>.git/info/lfs/objects/basic/<oid>
func (s *server) serviceLfsBasicDownload(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	oid := mux.Vars(r)["oid"]
	repo := gr.repo

	cfg := s.cfg
	logger := gr.logger.WithPrefix("http.lfs-basic")
	repoID := strconv.FormatInt(repo.ID(), 10)
	strg := storage.NewLocal(filepath.Join(cfg.DataPath, "lfs", repoID))

	// The object's database row may be missing (the batch API creates it
	// lazily), so the stored file, not the row, is the source of truth for
	// the size. http.ServeContent takes the length from the file and
	// handles Range and HEAD requests, so interrupted downloads can resume.
	pointer := lfs.Pointer{Oid: oid}
	f, err := strg.Open(path.Join("objects", pointer.RelativePath()))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			logger.Debug("object not found", "oid", oid, "repo", repo.Name())
			renderJSON(w, gr.logger, http.StatusNotFound, lfs.ErrorResponse{
				Message: "object not found",
			})
			return
		}
		logger.Error("error opening object", "oid", oid, "repo", repo.Name(), "err", err)
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}
	defer f.Close() //nolint: errcheck

	w.Header().Set("Content-Type", "application/octet-stream")
	// Objects are content addressed, so the oid is a strong validator.
	w.Header().Set("ETag", `"`+oid+`"`)
	http.ServeContent(w, r, "", time.Time{}, f)
}

// serviceLfsBasicUpload receives and stores an object.
// PUT: /<repo>.git/info/lfs/objects/basic/<oid>
func (s *server) serviceLfsBasicUpload(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	if !hasMediaType(r, "application/octet-stream") {
		renderJSON(w, gr.logger, http.StatusUnsupportedMediaType, lfs.ErrorResponse{
			Message: "invalid content type",
		})
		return
	}

	ctx := r.Context()

	oid := mux.Vars(r)["oid"]
	cfg := s.cfg
	dbx := s.dbx
	datastore := s.datastore
	logger := gr.logger.WithPrefix("http.lfs-basic")
	repo := gr.repo

	repoID := strconv.FormatInt(repo.ID(), 10)
	strg := storage.NewLocal(filepath.Join(cfg.DataPath, "lfs", repoID))

	defer r.Body.Close() //nolint: errcheck

	// NOTE: Git LFS client will retry uploading the same object if there was a
	// partial error, so we need to skip existing objects. The object exists
	// only if it is recorded and its file is in place: a recorded object
	// whose file is gone or damaged is stored again, which restores it.
	if obj, err := datastore.GetLFSObjectByOid(ctx, dbx, repo.ID(), oid); err == nil {
		if lfs.HasObject(strg, oid, obj.Size) {
			// Object exists, skip request
			io.Copy(io.Discard, r.Body) //nolint: errcheck
			renderStatus(w, http.StatusOK)
			return
		}
	} else if !errors.Is(err, db.ErrRecordNotFound) {
		logger.Error("error getting object", "oid", oid, "err", err)
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
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
		renderJSON(w, gr.logger, http.StatusLengthRequired, lfs.ErrorResponse{
			Message: "content length required",
		})
		return
	}

	err := lfs.StoreObject(ctx, strg, dbx, datastore, repo.ID(), oid, size, r.Body)
	switch {
	case errors.Is(err, lfs.ErrObjectSizeMismatch):
		logger.Info("object size mismatch", "oid", oid, "err", err)
		renderJSON(w, gr.logger, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "object size does not match content length",
		})
		return
	case errors.Is(err, lfs.ErrObjectOidMismatch):
		logger.Info("object hash mismatch", "oid", oid)
		renderJSON(w, gr.logger, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "object content does not match oid",
		})
		return
	case errors.Is(err, lfs.ErrObjectRead):
		// The client aborted or truncated the upload.
		logger.Info("error reading object", "oid", oid, "err", err)
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "error reading object",
		})
		return
	case err != nil:
		logger.Error("error storing object", "oid", oid, "err", err)
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}

	renderStatus(w, http.StatusOK)
}

// serviceLfsBasicVerify checks that an uploaded object is stored.
// POST: /<repo>.git/info/lfs/objects/basic/verify
func (s *server) serviceLfsBasicVerify(w http.ResponseWriter, r *http.Request, gr *gitRequest) {
	if !lfsMediaTypes(w, r, gr.logger) {
		return
	}

	var pointer lfs.Pointer
	ctx := r.Context()
	logger := gr.logger.WithPrefix("http.lfs-basic")
	repo := gr.repo

	defer r.Body.Close() //nolint: errcheck
	body := http.MaxBytesReader(w, r.Body, maxLfsJSONBodySize)
	if err := json.NewDecoder(body).Decode(&pointer); err != nil {
		if lfsBodyTooLarge(w, r, logger, err) {
			return
		}
		logger.Info("error decoding json", "err", err)
		renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
			Message: "invalid request: " + err.Error(),
		})
		return
	}

	// Object IDs become storage paths, so validate before touching the
	// filesystem.
	if !pointer.IsValid() {
		logger.Info("invalid object", "oid", pointer.Oid)
		renderJSON(w, gr.logger, http.StatusUnprocessableEntity, lfs.ErrorResponse{
			Message: "invalid object",
		})
		return
	}

	cfg := s.cfg
	dbx := s.dbx
	datastore := s.datastore
	repoID := strconv.FormatInt(repo.ID(), 10)
	strg := storage.NewLocal(filepath.Join(cfg.DataPath, "lfs", repoID))
	if stat, err := strg.Stat(path.Join("objects", pointer.RelativePath())); err == nil {
		// Verify object is in the database.
		obj, err := datastore.GetLFSObjectByOid(ctx, dbx, repo.ID(), pointer.Oid)
		if err != nil {
			if errors.Is(err, db.ErrRecordNotFound) {
				logger.Info("object not found", "oid", pointer.Oid)
				renderJSON(w, gr.logger, http.StatusNotFound, lfs.ErrorResponse{
					Message: "object not found",
				})
				return
			}
			logger.Error("error getting object", "oid", pointer.Oid, "err", err)
			renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
				Message: "internal server error",
			})
			return
		}

		if obj.Size != pointer.Size {
			renderJSON(w, gr.logger, http.StatusBadRequest, lfs.ErrorResponse{
				Message: "object size mismatch",
			})
			return
		}

		if stat.Size() != pointer.Size {
			// The recorded size matches but the stored file does not: the
			// object on disk is truncated or corrupt and must not verify.
			logger.Error("stored object size mismatch", "oid", pointer.Oid, "size", pointer.Size, "stored", stat.Size())
			renderJSON(w, gr.logger, http.StatusUnprocessableEntity, lfs.ErrorResponse{
				Message: "object size mismatch",
			})
			return
		}

		renderStatus(w, http.StatusOK)
	} else if errors.Is(err, fs.ErrNotExist) {
		logger.Info("object not found", "oid", pointer.Oid)
		renderJSON(w, gr.logger, http.StatusNotFound, lfs.ErrorResponse{
			Message: "object not found",
		})
		return
	} else {
		logger.Error("error getting object", "oid", pointer.Oid, "err", err)
		renderJSON(w, gr.logger, http.StatusInternalServerError, lfs.ErrorResponse{
			Message: "internal server error",
		})
		return
	}
}

// renderJSON renders a JSON response with the given status code and value. It
// also sets the Content-Type header to the JSON LFS media type
// (application/vnd.git-lfs+json). A failure to send it is logged on logger,
// the request's.
func renderJSON(w http.ResponseWriter, logger *log.Logger, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", lfs.MediaType)
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The client has most likely gone away.
		logger.Debug("error encoding json", "err", err)
	}
}

// lfsAccepts answers 406 unless the client accepts the LFS media type, and
// reports whether it does. logger is the request's logger.
func lfsAccepts(w http.ResponseWriter, r *http.Request, logger *log.Logger) bool {
	if !strings.HasPrefix(r.Header.Get("Accept"), lfs.MediaType) {
		renderJSON(w, logger, http.StatusNotAcceptable, lfs.ErrorResponse{
			Message: "Not Acceptable: the Accept header must be " + lfs.MediaType,
		})
		return false
	}
	return true
}

// lfsMediaTypes answers 406 unless the client accepts the LFS media type,
// and 415 unless it sends a body of that type, and reports whether both hold.
// logger is the request's logger.
func lfsMediaTypes(w http.ResponseWriter, r *http.Request, logger *log.Logger) bool {
	if !lfsAccepts(w, r, logger) {
		return false
	}
	if !hasMediaType(r, lfs.MediaType) {
		renderJSON(w, logger, http.StatusUnsupportedMediaType, lfs.ErrorResponse{
			Message: "Unsupported Media Type: the Content-Type header must be " + lfs.MediaType,
		})
		return false
	}
	return true
}
