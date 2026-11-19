package backend

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// LFSClient downloads LFS objects from a remote: an *lfs.HTTPClient, or a
// stand-in in tests.
type LFSClient interface {
	Download(ctx context.Context, objects []lfs.Pointer, callback lfs.DownloadCallback) error
}

// ErrLFSObjectsUnavailable is returned when the remote could not provide some
// LFS objects. Every other object was still stored. The error that wraps it
// carries the number of objects; each one is logged on its own.
var ErrLFSObjectsUnavailable = errors.New("some lfs objects are unavailable")

// lfsObjectsUnavailableError is ErrLFSObjectsUnavailable with the number of
// objects that were unavailable.
type lfsObjectsUnavailableError struct {
	count int
}

func (e *lfsObjectsUnavailableError) Error() string {
	return fmt.Sprintf("%s: %d", ErrLFSObjectsUnavailable, e.count)
}

func (e *lfsObjectsUnavailableError) Unwrap() error {
	return ErrLFSObjectsUnavailable
}

// lfsScannedRefsFile is the file in a repository's LFS directory that holds
// the ref tips whose LFS objects were all stored by the last complete sync,
// one object id per line, and when the whole history was last scanned. It
// lives next to the objects, so it goes away with them if the directory is
// removed. Deleting it makes the next mirror sync scan the whole history.
const lfsScannedRefsFile = "scanned-refs"

// lfsFullScanPrefix starts the line of lfsScannedRefsFile that holds the
// Unix time of the last scan of the whole history.
const lfsFullScanPrefix = "full-scan "

// lfsFullScanInterval is how often a mirror sync scans the whole history
// anyway. Only a full scan checks the objects of the history scanned
// before, so this is how long an object file that went missing or was cut
// short stays that way. Once a day keeps the cost of the full scan on a
// large repository to one sync in 144 at the default mirror interval.
var lfsFullScanInterval = 24 * time.Hour

// searchPointerBlobs scans for pointers. Tests replace it to see what is
// scanned.
var searchPointerBlobs = lfs.SearchPointerBlobsBetween

// lfsScanError is an error of the pointer scan itself, as opposed to one
// that came up storing the objects it found.
type lfsScanError struct {
	err error
}

func (e *lfsScanError) Error() string { return e.err.Error() }
func (e *lfsScanError) Unwrap() error { return e.err }

// StoreRepoMissingLFSObjects stores missing LFS objects for a repository. It
// scans the whole history of the repository.
//
// Objects the remote can't provide (an error for the object in the batch
// response, a download that answers 404 or a redirect, content that doesn't
// match) are logged and skipped, and counted in an error wrapping
// ErrLFSObjectsUnavailable once every other object is stored. Any other
// error stops the sync, including a download that fails or breaks off for a
// reason that may pass on a later try, such as a dropped connection or a 5xx.
func (d *Backend) StoreRepoMissingLFSObjects(ctx context.Context, repo proto.Repository, lfsClient LFSClient) error {
	return d.storeRepoLFSObjects(ctx, repo, lfsClient, false)
}

// SyncRepoLFSObjects is StoreRepoMissingLFSObjects for mirror syncs. It only
// scans the history added since the last sync that stored every object, so a
// sync that fetched nothing new scans nothing. It scans the whole history
// when no sync completed yet, once lfsFullScanInterval passed since the last
// full scan, when the repository's LFS directory or its lfsScannedRefsFile
// was removed, or when the shorter scan fails. Objects that were unavailable
// keep the history they are in scanned on later syncs, until a sync stores
// them all.
func (d *Backend) SyncRepoLFSObjects(ctx context.Context, repo proto.Repository, lfsClient LFSClient) error {
	return d.storeRepoLFSObjects(ctx, repo, lfsClient, true)
}

func (d *Backend) storeRepoLFSObjects(ctx context.Context, repo proto.Repository, lfsClient LFSClient, incremental bool) error {
	repoID := strconv.FormatInt(repo.ID(), 10)
	lfsRoot := filepath.Join(d.cfg.DataPath, "lfs", repoID)
	logger := log.FromContext(ctx).WithPrefix("lfs")

	r, err := repo.Open()
	if err != nil {
		return err
	}

	// The tips are taken before the scan, so refs that move during it are
	// scanned on the next sync.
	tips, err := refTips(ctx, r)
	if err != nil {
		return err
	}

	markerPath := filepath.Join(lfsRoot, lfsScannedRefsFile)
	var since []string
	var fullScan time.Time
	if incremental {
		since, fullScan = readScannedRefs(markerPath)
		// A last full scan in the future means the clock went back, so
		// don't count on it either.
		if age := time.Since(fullScan); since != nil && (age < 0 || age >= lfsFullScanInterval) {
			logger.Debug("scanning the whole repository for lfs objects, the last full scan is too old", "repo", repo.Name(), "full_scan", fullScan)
			since = nil
		}
	}

	start := time.Now()
	switch {
	case since == nil:
		err = d.storeMissingLFSObjects(ctx, repo, r, lfsClient, lfsRoot, tips, nil)
		fullScan = start
	case slices.Equal(since, tips):
		// Nothing changed since the last complete sync.
		return nil
	default:
		err = d.storeMissingLFSObjects(ctx, repo, r, lfsClient, lfsRoot, tips, since)
		var scanErr *lfsScanError
		if errors.As(err, &scanErr) && ctx.Err() == nil {
			// For example, an old tip was pruned since the last sync.
			logger.Warn("scanning the whole repository for lfs objects, scanning the new history failed", "repo", repo.Name(), "err", err)
			err = d.storeMissingLFSObjects(ctx, repo, r, lfsClient, lfsRoot, tips, nil)
			fullScan = start
		}
	}
	if err != nil {
		// Objects that weren't stored stay in the history the next sync
		// scans, as the tips it starts from don't move.
		return err
	}

	if err := writeScannedRefs(markerPath, tips, fullScan); err != nil {
		// The next sync scans more history than it needs to.
		logger.Warn("failed to save the scanned refs", "repo", repo.Name(), "err", err)
	}
	return nil
}

// refTips returns the sorted, distinct object ids the refs and HEAD of r
// point to, the tips git rev-list --all starts from.
func refTips(ctx context.Context, r *git.Repository) ([]string, error) {
	out, err := gitm.NewCommandWithContext(ctx, "for-each-ref", "--format=%(objectname)").RunInDir(r.Path)
	if err != nil {
		return nil, fmt.Errorf("listing refs: %w", err)
	}
	tips := strings.Fields(string(out))
	// A detached HEAD, which git clone --mirror copies, can reach commits
	// no ref does. An unborn HEAD doesn't resolve and adds nothing.
	if head, err := gitm.NewCommandWithContext(ctx, "rev-parse", "-q", "--verify", "HEAD").RunInDir(r.Path); err == nil {
		if id := strings.TrimSpace(string(head)); isObjectID(id) {
			tips = append(tips, id)
		}
	}
	slices.Sort(tips)
	return slices.Compact(tips), nil
}

// readScannedRefs returns the tips saved in the file at path, sorted and
// distinct, and when the whole history was last scanned, or nil tips if
// there is no usable file. A file without the time of the full scan is
// usable, with a zero time.
func readScannedRefs(path string) ([]string, time.Time) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}
	}
	tips := []string{}
	var fullScan time.Time
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, lfsFullScanPrefix):
			sec, err := strconv.ParseInt(strings.TrimPrefix(line, lfsFullScanPrefix), 10, 64)
			if err != nil {
				return nil, time.Time{}
			}
			fullScan = time.Unix(sec, 0)
		case isObjectID(line):
			tips = append(tips, line)
		default:
			return nil, time.Time{}
		}
	}
	slices.Sort(tips)
	return slices.Compact(tips), fullScan
}

// writeScannedRefs saves tips and the time of the last full scan to the
// file at path, replacing it at once so a sync never reads half of it.
// The directory is the LFS root of the repository, which pkg/storage keeps
// 0700 like the object directories under it.
func writeScannedRefs(path string, tips []string, fullScan time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), lfsScannedRefsFile+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint: errcheck
	var data strings.Builder
	data.WriteString(lfsFullScanPrefix + strconv.FormatInt(fullScan.Unix(), 10) + "\n")
	for _, id := range tips {
		data.WriteString(id + "\n")
	}
	if _, err := f.WriteString(data.String()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// isObjectID reports whether s is a sha1 or sha256 object id.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// lfsBatchSize is how many objects one download batch request asks for.
const lfsBatchSize = 20

// storeMissingLFSObjects stores the missing LFS objects of the pointers
// reachable from include but not from exclude. Errors of the scan itself are
// returned as an *lfsScanError.
func (d *Backend) storeMissingLFSObjects(ctx context.Context, repo proto.Repository, r *git.Repository, lfsClient LFSClient, lfsRoot string, include, exclude []string) error {
	// Cancel the scan when we return early so its goroutines and git
	// processes don't stay blocked on pointerChan.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pointerChan := make(chan lfs.PointerBlob)
	errChan := make(chan error, 1)
	go searchPointerBlobs(ctx, r, include, exclude, pointerChan, errChan)

	s := &lfsSync{
		ctx:       ctx,
		repo:      repo,
		dbx:       d.db,
		datastore: d.store,
		client:    lfsClient,
		// TODO: support S3 storage
		strg:     storage.NewLocalStorage(lfsRoot),
		logger:   log.FromContext(ctx).WithPrefix("lfs"),
		recorded: make(map[string]int64),
		verified: make(map[string]bool),
		queued:   make(map[lfsPointerKey]bool),
		batched:  make(map[string]bool),
	}
	return s.run(pointerChan, errChan)
}

// lfsPointerKey is an LFS pointer's oid and size. Different pointer blobs can
// share both, for example when one has a trailing line, so pointers are
// queued once per key. Pointers can also share an oid but not the size, when
// one of them is malformed; each one is tried, as the first one scanned may
// be the bad one.
type lfsPointerKey struct {
	oid  string
	size int64
}

// lfsSync is one run of storeMissingLFSObjects: it checks each pointer the
// scan finds against storage and the database, and downloads the objects
// that are missing in batches.
type lfsSync struct {
	ctx       context.Context
	repo      proto.Repository
	dbx       *db.DB
	datastore store.Store
	client    LFSClient
	strg      storage.Storage
	logger    *log.Logger

	// recorded holds the recorded sizes of the objects that are already in
	// the database but to be downloaded. Those only need their content
	// written, and their record corrected if the size is wrong.
	recorded map[string]int64
	// verified caches whether the file on disk of an oid holds the object,
	// so each one is hashed at most once per sync.
	verified map[string]bool
	// unavailable counts the objects the remote could not provide.
	unavailable int

	// queued holds the pointers already queued for download.
	queued map[lfsPointerKey]bool
	// batch is the next download batch, and batched the oids in it. The
	// download callback expects each oid once per batch, so a pointer whose
	// oid is already in the batch waits in deferred for a later one.
	batch, deferred []lfs.Pointer
	batched         map[string]bool
}

// run checks each pointer from pointers, the scan's output, downloads the
// missing objects, and returns the scan's error from errs as an
// *lfsScanError, or an *lfsObjectsUnavailableError if some objects could
// not be had.
func (s *lfsSync) run(pointers <-chan lfs.PointerBlob, errs <-chan error) error {
	for pointer := range pointers {
		key := lfsPointerKey{pointer.Oid, pointer.Size}
		if s.queued[key] {
			continue
		}

		need, err := s.missing(pointer.Pointer)
		if err != nil {
			return err
		}
		if !need {
			continue
		}

		s.queued[key] = true
		if err := s.queue(pointer.Pointer); err != nil {
			return err
		}
	}

	if err, ok := <-errs; ok {
		return &lfsScanError{err}
	}

	// Download whatever is left over from the last, partial batch, then the
	// pointers that shared an oid with another one in their batch. Those
	// are checked again, as the other pointer may have stored the object.
	for len(s.batch) > 0 || len(s.deferred) > 0 {
		if err := s.flush(); err != nil {
			return err
		}

		deferred := s.deferred
		s.deferred = nil
		for _, pointer := range deferred {
			need, err := s.missing(pointer)
			if err != nil {
				return err
			}
			if need {
				if err := s.queue(pointer); err != nil {
					return err
				}
			}
		}
	}

	if s.unavailable > 0 {
		return &lfsObjectsUnavailableError{count: s.unavailable}
	}
	return nil
}

// isObject reports whether the file on disk of oid is a regular file of
// size bytes that holds the object; one that can't be read is not. It is
// only checked when its size or its record disagrees with a pointer, as
// hashing every object on every sync would read all of them.
func (s *lfsSync) isObject(oid string, size int64) bool {
	if ok, checked := s.verified[oid]; checked {
		return ok
	}
	ok := lfs.HasObject(s.strg, oid, size)
	s.verified[oid] = ok
	return ok
}

// objectFailed logs and counts an object the remote could not provide.
func (s *lfsSync) objectFailed(p lfs.Pointer, err error) {
	s.logger.Warn("skipping lfs object", "repo", s.repo.Name(), "oid", p.Oid, "err", err)
	s.unavailable++
}

// missing reports whether the object of pointer has to be downloaded. An
// object already on disk is recorded if it isn't yet.
func (s *lfsSync) missing(pointer lfs.Pointer) (bool, error) {
	ctx, repo := s.ctx, s.repo
	obj, err := s.datastore.GetLFSObjectByOid(ctx, s.dbx, repo.ID(), pointer.Oid)
	if err != nil && !errors.Is(err, db.ErrRecordNotFound) {
		return false, err
	}

	// A file of the wrong size, such as one an interrupted download left
	// behind, counts as missing and is downloaded again.
	name := path.Join("objects", pointer.RelativePath())
	fi, err := s.strg.Stat(name)
	switch {
	case err == nil && fi.Size() == pointer.Size && (obj.ID == 0 || obj.Size == pointer.Size):
		// The object is on disk, make sure it's recorded.
		if obj.ID == 0 {
			if err := recordLFSObject(ctx, s.dbx, s.datastore, repo.ID(), pointer); err != nil {
				return false, err
			}
		}
		return false, nil
	case err == nil && fi.Size() == pointer.Size:
		// The object is on disk, but recorded with another size, which
		// older versions could take from a malformed pointer. If the file
		// is the object, its size is the right one.
		if s.isObject(pointer.Oid, fi.Size()) {
			s.logger.Info("correcting the recorded size of lfs object", "repo", repo.Name(), "oid", pointer.Oid, "size", pointer.Size, "recorded", obj.Size)
			return false, s.datastore.UpdateLFSObjectSize(ctx, s.dbx, repo.ID(), pointer.Oid, pointer.Size)
		}
	case err == nil && obj.ID != 0 && obj.Size == fi.Size():
		// The file and its record agree on another size. If the file is the
		// object, this pointer is malformed, and downloading it would only
		// fail its verification on every sync. Older versions could store
		// and record a file cut short, though, which this pointer may be
		// the only right one for.
		if s.isObject(pointer.Oid, fi.Size()) {
			s.logger.Debug("ignoring lfs pointer with the wrong size", "repo", repo.Name(), "oid", pointer.Oid, "size", pointer.Size, "stored", obj.Size)
			return false, nil
		}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return false, err
	}

	if obj.ID != 0 {
		s.recorded[pointer.Oid] = obj.Size
	}
	return true, nil
}

// queue adds pointer to the next batch, or defers it when its oid is in the
// batch already, and downloads the batch once it is full.
func (s *lfsSync) queue(pointer lfs.Pointer) error {
	if s.batched[pointer.Oid] {
		s.deferred = append(s.deferred, pointer)
		return nil
	}
	s.batched[pointer.Oid] = true
	s.batch = append(s.batch, pointer)
	if len(s.batch) >= lfsBatchSize {
		return s.flush()
	}
	return nil
}

// flush downloads the batch, if any, and starts a new one.
func (s *lfsSync) flush() error {
	if len(s.batch) == 0 {
		return nil
	}
	if err := s.download(s.batch); err != nil {
		return err
	}
	s.batch = nil
	clear(s.batched)
	return nil
}

// download downloads and stores the objects of pointers, one batch.
func (s *lfsSync) download(pointers []lfs.Pointer) error {
	pending := make(map[string]lfs.Pointer, len(pointers))
	for _, p := range pointers {
		pending[p.Oid] = p
	}

	if err := s.client.Download(s.ctx, pointers, func(got lfs.Pointer, content io.ReadCloser, objectError error) error {
		if content != nil {
			defer content.Close() //nolint: errcheck
		}

		// The pointer comes from the remote's response. Only take objects
		// we asked for, and go by our own pointer from then on.
		p, ok := pending[got.Oid]
		if !got.IsValid() || !ok {
			s.logger.Warn("ignoring lfs object that was not requested", "repo", s.repo.Name(), "oid", got.Oid)
			return nil
		}
		delete(pending, got.Oid)

		if objectError != nil {
			s.objectFailed(p, objectError)
			return nil
		}
		return s.store(p, content)
	}); err != nil {
		return err
	}

	for _, p := range pending {
		s.objectFailed(p, errors.New("not returned by the remote"))
	}
	return nil
}

// store stores the downloaded object of p from content. StoreObject streams
// the object to a temporary file outside of any database transaction, so a
// slow download doesn't hold the write lock, verifies it, and records it. If
// recording fails, the object stays on disk and the next sync records it.
func (s *lfsSync) store(p lfs.Pointer, content io.Reader) error {
	err := lfs.StoreObject(s.ctx, s.strg, s.dbx, s.datastore, s.repo.ID(), p.Oid, p.Size, content)
	if errors.Is(err, lfs.ErrObjectSizeMismatch) || errors.Is(err, lfs.ErrObjectOidMismatch) {
		s.objectFailed(p, fmt.Errorf("content does not match: %w", err))
		return nil
	}
	if err != nil {
		return err
	}

	s.verified[p.Oid] = true

	if size, ok := s.recorded[p.Oid]; ok && size != p.Size {
		// Older versions could record the size of a malformed pointer; the
		// content now proves this one right.
		return s.datastore.UpdateLFSObjectSize(s.ctx, s.dbx, s.repo.ID(), p.Oid, p.Size)
	}
	return nil
}

// recordLFSObject records an LFS object of a repository. An object someone
// else recorded in the meantime is not an error.
func recordLFSObject(ctx context.Context, dbx *db.DB, datastore store.Store, repoID int64, p lfs.Pointer) error {
	err := datastore.CreateLFSObject(ctx, dbx, repoID, p.Oid, p.Size)
	if errors.Is(err, db.ErrDuplicateKey) {
		return nil
	}
	return err
}
