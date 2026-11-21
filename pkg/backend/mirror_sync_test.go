package backend

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// The recorded error is cut to maxMirrorErrorLen bytes between characters,
// also when a character straddles the limit, and is valid UTF-8 without NUL
// bytes whatever git wrote.
func TestRecordMirrorSyncCapsError(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m, err := seedCreate(be, ctx, "m", admin, proto.RepositoryOptions{Mirror: true})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, msg string
	}{
		// "é" is two bytes, at maxMirrorErrorLen-1 and maxMirrorErrorLen.
		{"straddling", strings.Repeat("a", maxMirrorErrorLen-1) + strings.Repeat("é", 100)},
		{"not utf-8", "\x00" + strings.Repeat("\xffa", maxMirrorErrorLen)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := be.RecordMirrorSync(ctx, m, errors.New(tc.msg)); err != nil {
				t.Fatal(err)
			}
			st, err := be.MirrorSyncStatus(ctx, "m")
			if err != nil {
				t.Fatal(err)
			}
			kept, ok := strings.CutSuffix(st.Error, "... (truncated)")
			if !ok {
				t.Fatalf("error %.40q... is not marked as cut", st.Error)
			}
			if len(kept) > maxMirrorErrorLen || !utf8.ValidString(st.Error) || strings.ContainsRune(st.Error, 0) {
				t.Errorf("kept %d bytes, valid UTF-8 %v: %.80q", len(kept), utf8.ValidString(st.Error), st.Error)
			}
		})
	}
}
