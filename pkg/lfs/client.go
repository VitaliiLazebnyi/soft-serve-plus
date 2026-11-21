package lfs

import (
	"io"

	"charm.land/log/v2"
)

// DownloadCallback gets called for every requested LFS object to process its content
type DownloadCallback func(p Pointer, content io.ReadCloser, objectError error) error

// NewClient returns a Git LFS client for e, an endpoint from NewEndpoint,
// that logs to logger, or to log.Default() when it is nil.
func NewClient(e Endpoint, logger *log.Logger) *HTTPClient {
	return newHTTPClient(e, logger)
}
