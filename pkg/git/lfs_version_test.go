package git

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// A "version" command with no argument makes the transfer library index
// past its fields. The session ends with an error instead of the process.
func TestLFSTransferBareVersion(t *testing.T) {
	t.Parallel()

	var in strings.Builder
	in.WriteString(pkt("version\n"))
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("quit\n") + "0000")

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	var logs bytes.Buffer
	ctx := config.WithContext(context.Background(), cfg)
	ctx = log.WithContext(ctx, log.New(&logs))
	ctx = proto.WithRepositoryContext(ctx, lfsTestRepo{})
	ctx = proto.WithUserContext(ctx, lfsTestUser{})

	var out bytes.Buffer
	err := LFSTransfer(ctx, ServiceCommand{
		Stdin:  strings.NewReader(in.String()),
		Stdout: &out,
		Args:   []string{"repo1", lfs.OperationDownload},
	})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("LFSTransfer => %v, want the processor's panic as an error", err)
	}
	if strings.Contains(out.String(), "status 200") {
		t.Errorf("commands after the bare version were served\noutput: %q", out.String())
	}
	// The log has where it panicked, which the error does not.
	if !strings.Contains(logs.String(), "processCommands") {
		t.Errorf("panic logged without its stack:\n%s", logs.String())
	}
}
