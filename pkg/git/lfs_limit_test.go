package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// runLFSTransfer runs an upload session over in and returns the status codes
// it answered with and the error it ended with.
func runLFSTransfer(t *testing.T, ctx context.Context, in string) ([]string, error) {
	t.Helper()
	ctx = proto.WithRepositoryContext(ctx, lfsTestRepo{})
	ctx = proto.WithUserContext(ctx, lfsTestUser{})
	var out bytes.Buffer
	err := LFSTransfer(ctx, ServiceCommand{
		Stdin:  strings.NewReader(in),
		Stdout: &out,
		Args:   []string{"repo1", lfs.OperationUpload},
	})
	var got []string
	for _, m := range regexp.MustCompile(`status (\d{3})`).FindAllStringSubmatch(out.String(), -1) {
		got = append(got, m[1])
	}
	return got, err
}

// batchRequest returns a batch command for n objects.
func batchRequest(n int) string {
	var b strings.Builder
	b.WriteString(pkt("batch\n") + pkt("transfer=basic\n") + "0001")
	for i := range n {
		b.WriteString(pkt(fmt.Sprintf("%064x %d\n", i, i+1)))
	}
	b.WriteString("0000")
	return b.String()
}

// A batch over SSH holds at most as many objects as one over HTTP. A larger
// one is refused before any object is looked up, and the session goes on.
func TestLFSTransferBatchTooManyObjects(t *testing.T) {
	_, ctx := newLFSUploadTransfer(t)
	in := pkt("version 1\n") + "0000" +
		batchRequest(lfs.MaxBatchObjects+1) +
		batchRequest(lfs.MaxBatchObjects) +
		pkt("quit\n") + "0000"

	got, err := runLFSTransfer(t, ctx, in)
	if err != nil {
		t.Fatalf("LFSTransfer: %v", err)
	}
	if want := []string{"200", "400", "200", "200"}; !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
}

// A command whose packets add up to more than the limit ends the session
// instead of being read into memory whole.
func TestLFSTransferCommandTooLarge(t *testing.T) {
	_, ctx := newLFSUploadTransfer(t)
	arg := pkt("ref=" + strings.Repeat("a", 60000) + "\n")
	var in strings.Builder
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("lock\n"))
	for in.Len() < 2*lfs.MaxBatchRequestSize {
		in.WriteString(arg)
	}
	in.WriteString("0000")
	in.WriteString(pkt("quit\n") + "0000")

	got, err := runLFSTransfer(t, ctx, in.String())
	if !errors.Is(err, errLFSCommandTooLarge) {
		t.Fatalf("LFSTransfer: got %v, want errLFSCommandTooLarge", err)
	}
	if want := []string{"200", "400"}; !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
}

// Object data sent with put-object is not a command's arguments: an object
// larger than the limit is stored.
func TestLFSTransferPutObjectLargerThanCommandLimit(t *testing.T) {
	tr, ctx := newLFSUploadTransfer(t)
	data := strings.Repeat("x", 2*lfs.MaxBatchRequestSize)
	oid := oidOf(data)

	var in strings.Builder
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("put-object "+oid+"\n") + pkt(fmt.Sprintf("size=%d\n", len(data))) + "0001")
	for rest := data; rest != ""; {
		n := min(len(rest), 65516)
		in.WriteString(pkt(rest[:n]))
		rest = rest[n:]
	}
	in.WriteString("0000")
	in.WriteString(pkt("quit\n") + "0000")

	got, err := runLFSTransfer(t, ctx, in.String())
	if err != nil {
		t.Fatalf("LFSTransfer: %v", err)
	}
	if want := []string{"200", "200", "200"}; !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
	if fi, err := os.Stat(lfsObjectPath(tr.cfg, oid)); err != nil || fi.Size() != int64(len(data)) {
		t.Errorf("object not stored: %v", err)
	}
}

// The limit counts the bytes since the last flush, so many commands under the
// limit add up to more than it, and the bytes pass through unchanged.
func TestLFSCommandLimitReader(t *testing.T) {
	cmd := pkt("verify-object x\n") + pkt("size=1\n") + "0000"
	put := pkt("put-object x\n") + pkt("size=100\n") + "0001" + pkt(strings.Repeat("d", 100)) + "0000"
	in := strings.Repeat(cmd+put, 10)

	got, err := io.ReadAll(newLFSCommandLimitReader(strings.NewReader(in), len(cmd)))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != in {
		t.Errorf("read %q, want %q", got, in)
	}

	// One byte short of the first command's packets: its last one is refused.
	got, err = io.ReadAll(newLFSCommandLimitReader(strings.NewReader(in), len(cmd)-5))
	if !errors.Is(err, errLFSCommandTooLarge) {
		t.Fatalf("ReadAll: got %v, want errLFSCommandTooLarge", err)
	}
	if want := pkt("verify-object x\n"); string(got) != want {
		t.Errorf("read %q before the error, want %q", got, want)
	}

	// A broken header is passed through for the pktline reader to report.
	got, err = io.ReadAll(newLFSCommandLimitReader(strings.NewReader("zzzzrest"), 4))
	if err != nil || string(got) != "zzzzrest" {
		t.Errorf("ReadAll = %q, %v; want the input unchanged", got, err)
	}
}

// Batch itself refuses too many objects, before touching the store.
func TestLFSTransferBatchObjectLimit(t *testing.T) {
	tr, _ := newLFSUploadTransfer(t)
	tr.store = nil
	items := make([]transfer.BatchItem, lfs.MaxBatchObjects+1)
	if _, err := tr.Batch(lfs.OperationDownload, items, nil); !errors.Is(err, transfer.ErrParseError) {
		t.Errorf("Batch: got %v, want ErrParseError", err)
	}
}
