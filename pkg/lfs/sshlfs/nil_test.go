package sshlfs

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
)

// Transfer and Authenticate refuse missing dependencies before they answer
// the client, rather than fail at the first command that needs them.
func TestNilDependencies(t *testing.T) {
	t.Parallel()
	_, deps := newLFSUploadTransfer(t)
	noConfig, noDB, noStore := deps, deps, deps
	noConfig.Config = nil
	noDB.DB = nil
	noStore.Store = nil
	for _, tc := range []struct {
		name string
		run  func(out io.Writer) error
		want error
	}{
		{"transfer nil config", func(out io.Writer) error {
			return Transfer(t.Context(), strings.NewReader(""), out, noConfig, lfsTestRepo{}, lfsTestUser{}, lfs.OperationUpload)
		}, config.ErrNilConfig},
		{"transfer nil database", func(out io.Writer) error {
			return Transfer(t.Context(), strings.NewReader(""), out, noDB, lfsTestRepo{}, lfsTestUser{}, lfs.OperationUpload)
		}, errNilDeps},
		{"transfer nil store", func(out io.Writer) error {
			return Transfer(t.Context(), strings.NewReader(""), out, noStore, lfsTestRepo{}, lfsTestUser{}, lfs.OperationUpload)
		}, errNilDeps},
		{"authenticate nil config", func(out io.Writer) error {
			return Authenticate(out, nil, nil, log.New(io.Discard), lfsAuthRepo{}, lfsAuthUser{}, lfs.OperationDownload)
		}, config.ErrNilConfig},
		{"authenticate nil signer", func(out io.Writer) error {
			return Authenticate(out, config.DefaultConfig(), nil, log.New(io.Discard), lfsAuthRepo{}, lfsAuthUser{}, lfs.OperationDownload)
		}, jwk.ErrHostKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			if err := tc.run(&out); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("answered the client: %q", out.String())
			}
		})
	}
}
