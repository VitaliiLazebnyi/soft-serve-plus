package git

import (
	"bytes"
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// A "version" command with no argument is a bad request, not a crash, and
// the session goes on to serve the commands after it.
func TestLFSTransferBareVersion(t *testing.T) {
	t.Parallel()

	var in strings.Builder
	in.WriteString(pkt("version\n"))
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("quit\n") + "0000")

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := config.WithContext(context.Background(), cfg)
	ctx = proto.WithRepositoryContext(ctx, lfsTestRepo{})
	ctx = proto.WithUserContext(ctx, lfsTestUser{})

	var out bytes.Buffer
	if err := LFSTransfer(ctx, ServiceCommand{
		Stdin:  strings.NewReader(in.String()),
		Stdout: &out,
		Args:   []string{"repo1", lfs.OperationDownload},
	}); err != nil {
		t.Fatalf("LFSTransfer: %v", err)
	}

	var got []string
	for _, m := range regexp.MustCompile(`status (\d{3})`).FindAllStringSubmatch(out.String(), -1) {
		got = append(got, m[1])
	}
	// bare version, version 1, quit.
	want := []string{"400", "200", "200"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v\noutput: %q", got, want, out.String())
	}
	if !strings.Contains(out.String(), "unknown version") {
		t.Errorf("bare version not answered with \"unknown version\"\noutput: %q", out.String())
	}
}

// Only the processor's own bare-version index is recovered: the same index
// error raised anywhere else is not.
func TestIsBareVersionPanicOnlyMatchesProcessor(t *testing.T) {
	t.Parallel()

	matched := func() (ok bool) {
		defer func() { ok = isBareVersionPanic(recover()) }()
		msgs := strings.Split("version", " ")
		_ = msgs[len(msgs)]
		return false
	}()
	if matched {
		t.Error("an index panic outside the processor was taken for a bare version")
	}
}
