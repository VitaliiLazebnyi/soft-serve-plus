package lfs

import (
	"context"
	"io"
)

// TransferBasic is the name of the Git LFS basic transfer protocol.
const TransferBasic = "basic"

// TransferAdapter represents an adapter for downloading LFS objects. Objects
// are only ever fetched from a remote LFS server (on mirror sync), never sent
// to one, so it has no upload or verify side.
type TransferAdapter interface {
	Name() string
	Download(ctx context.Context, p Pointer, l *Link) (io.ReadCloser, error)
}
