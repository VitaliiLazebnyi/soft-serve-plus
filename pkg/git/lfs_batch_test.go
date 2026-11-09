package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
)

// Batch only reads: for an object on disk but not recorded (an upload
// between storing and recording it), it used to record the size the client
// sent, in download sessions too. The size is now checked against the file,
// and an upload session reports the object missing so the upload records
// it.
func TestLFSTransferBatchDoesNotRecordObjects(t *testing.T) {
	const content = "hello lfs"
	oid := oidOf(content)
	size := int64(len(content))

	cases := []struct {
		op   string
		size int64
		want bool
	}{
		{lfs.OperationDownload, 0, false},
		{lfs.OperationDownload, size + 1, false},
		{lfs.OperationDownload, size, true},
		{lfs.OperationUpload, 0, false},
		{lfs.OperationUpload, size, false},
	}
	for _, tc := range cases {
		tr, _ := newLFSUploadTransfer(t)
		tr.op = tc.op
		p := lfsObjectPath(tr.cfg, oid)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		items, err := tr.Batch(tc.op, []transfer.BatchItem{{Pointer: transfer.Pointer{Oid: oid, Size: tc.size}}}, nil)
		if err != nil {
			t.Fatalf("%s size %d: Batch: %v", tc.op, tc.size, err)
		}
		if items[0].Present != tc.want {
			t.Errorf("%s size %d: present %v, want %v", tc.op, tc.size, items[0].Present, tc.want)
		}
		if _, err := tr.store.GetLFSObjectByOid(tr.ctx, tr.dbx, 1, oid); !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("%s size %d: object recorded by Batch (err %v)", tc.op, tc.size, err)
		}
	}

	// A recorded object is present only at its recorded size.
	tr, _ := newLFSUploadTransfer(t)
	if err := tr.Upload(oid, size, strings.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	tr.op = lfs.OperationDownload
	for _, s := range []int64{0, size} {
		items, err := tr.Batch(tr.op, []transfer.BatchItem{{Pointer: transfer.Pointer{Oid: oid, Size: s}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if items[0].Present != (s == size) {
			t.Errorf("recorded, size %d: present %v", s, items[0].Present)
		}
	}
}
