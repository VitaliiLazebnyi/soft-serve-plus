package lfs

import (
	"context"
	"io"
)

// DownloadCallback gets called for every requested LFS object to process its content
type DownloadCallback func(p Pointer, content io.ReadCloser, objectError error) error

// Client is a Git LFS client to communicate with a LFS source API.
type Client interface {
	Download(ctx context.Context, objects []Pointer, callback DownloadCallback) error
}

// NewClient returns a new Git LFS client.
func NewClient(e Endpoint) Client {
	if e.Scheme == "http" || e.Scheme == "https" {
		return newHTTPClient(e)
	}
	// TODO: support ssh client
	return nil
}
