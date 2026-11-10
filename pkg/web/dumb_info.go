package web

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/gorilla/mux"
)

// dumbGitWaitDelay is how long after the request ends git is killed if
// it still holds its output open.
const dumbGitWaitDelay = 5 * time.Second

// getDumbInfoRefs answers a dumb HTTP info/refs request with the
// repository's refs, one "<oid>\t<ref>" line each and a "<ref>^{}" line with
// the peeled object of each annotated tag, as git update-server-info would
// write them. Like git http-backend, it lists the refs on every request
// instead of running update-server-info, which writes to the repository and
// would let any anonymous reader make the server write.
func getDumbInfoRefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.FromContext(ctx)
	dir := mux.Vars(r)["dir"]

	// show-ref leaves out HEAD and peels tags fully, as update-server-info
	// does.
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--dereference")
	cmd.Dir = dir
	cmd.WaitDelay = dumbGitWaitDelay
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logger.Errorf("failed to list refs: %v", err)
		renderInternalServerError(w, r)
		return
	}
	if err := cmd.Start(); err != nil {
		logger.Errorf("failed to list refs: %v", err)
		renderInternalServerError(w, r)
		return
	}

	out := &advertisement{w: w, start: func() {
		hdrNocache(w)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	}}
	br := bufio.NewReader(stdout)
	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			// show-ref separates the object and the ref with a space; refs
			// cannot contain one.
			if oid, ref, ok := strings.Cut(line, " "); ok {
				line = oid + "\t" + ref
			}
			if _, werr := io.WriteString(out, line); werr != nil {
				// The client is gone. Let git finish rather than block on
				// a full pipe.
				io.Copy(io.Discard, br) //nolint: errcheck
				break
			}
		}
		if rerr != nil {
			break
		}
	}

	err = cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && !out.started && stderr.Len() == 0:
		// show-ref exits 1 when there are no refs.
	case ctx.Err() != nil:
		logger.Debugf("listing refs cancelled: %v", err)
		return
	case out.started:
		logger.Errorf("failed to list refs: %v: %s", err, bytes.TrimSpace(stderr.Bytes()))
		return
	default:
		logger.Errorf("failed to list refs: %v: %s", err, bytes.TrimSpace(stderr.Bytes()))
		renderInternalServerError(w, r)
		return
	}
	out.begin()
}

// packName matches the name of a pack in objects/pack, for SHA-1 and
// SHA-256 repositories.
var packName = regexp.MustCompile(`^pack-(?:[0-9a-f]{40}|[0-9a-f]{64})\.pack$`)

// getInfoPacks answers a dumb HTTP objects/info/packs request with the
// repository's packs, one "P <pack>" line each and a blank line at the end,
// as git update-server-info would write them. Like git http-backend, it
// lists objects/pack on every request rather than serving a file that only
// update-server-info, a write, keeps current.
func getInfoPacks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dir := mux.Vars(r)["dir"]
	packDir := filepath.Join(dir, "objects", "pack")

	entries, err := os.ReadDir(packDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.FromContext(ctx).Errorf("failed to list packs: %v", err)
		renderInternalServerError(w, r)
		return
	}

	var packs []string
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !packName.MatchString(name) {
			continue
		}
		// git writes the index last, so a pack without one is still being
		// written, or is not usable.
		idx := strings.TrimSuffix(name, ".pack") + ".idx"
		if fi, err := os.Stat(filepath.Join(packDir, idx)); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		packs = append(packs, name)
	}
	sort.Strings(packs)

	var b strings.Builder
	for _, p := range packs {
		b.WriteString("P " + p + "\n")
	}
	b.WriteString("\n")

	hdrNocache(w)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, b.String()) //nolint: errcheck
}
