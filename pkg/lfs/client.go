package lfs

import (
	"io"
)

// DownloadCallback gets called for every requested LFS object to process its content
type DownloadCallback func(p Pointer, content io.ReadCloser, objectError error) error

// NewClient returns a Git LFS client for e, an endpoint from NewEndpoint.
func NewClient(e Endpoint) *HTTPClient {
	return newHTTPClient(e)
}
