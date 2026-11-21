package sshlfs

import (
	"bytes"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
)

// A "version" command with no argument makes the transfer library index
// past its fields. The session ends with an error instead of the process.
func TestLFSTransferBareVersion(t *testing.T) {
	t.Parallel()

	var in strings.Builder
	in.WriteString(pkt("version\n"))
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("quit\n") + "0000")

	_, deps := newLFSUploadTransfer(t)
	deps.Store = untouchedStore{}
	var logs bytes.Buffer
	deps.Logger = log.New(&logs)
	var out bytes.Buffer
	err := Transfer(t.Context(), strings.NewReader(in.String()), &out, deps, lfsTestRepo{}, lfsTestUser{}, lfs.OperationDownload)
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("Transfer => %v, want the processor's panic as an error", err)
	}
	if strings.Contains(out.String(), "status 200") {
		t.Errorf("commands after the bare version were served\noutput: %q", out.String())
	}
	// The log has where it panicked, which the error does not.
	if !strings.Contains(logs.String(), "processCommands") {
		t.Errorf("panic logged without its stack:\n%s", logs.String())
	}
}
