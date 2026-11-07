package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// BasicTransferAdapter implements the "basic" adapter
type BasicTransferAdapter struct {
	client *http.Client
}

// Name returns the name of the adapter
func (a *BasicTransferAdapter) Name() string {
	return "basic"
}

// Download reads the download location and downloads the data
func (a *BasicTransferAdapter) Download(ctx context.Context, _ Pointer, l *Link) (io.ReadCloser, error) {
	resp, err := a.performRequest(ctx, "GET", l, nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Upload sends the content to the LFS server
func (a *BasicTransferAdapter) Upload(ctx context.Context, p Pointer, r io.Reader, l *Link) error {
	res, err := a.performRequest(ctx, "PUT", l, r, func(req *http.Request) {
		if len(req.Header.Get("Content-Type")) == 0 {
			req.Header.Set("Content-Type", "application/octet-stream")
		}

		if req.Header.Get("Transfer-Encoding") == "chunked" {
			req.TransferEncoding = []string{"chunked"}
		}

		req.ContentLength = p.Size
	})
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// Verify calls the verify handler on the LFS server
func (a *BasicTransferAdapter) Verify(ctx context.Context, p Pointer, l *Link) error {
	logger := log.FromContext(ctx).WithPrefix("lfs")
	b, err := json.Marshal(p)
	if err != nil {
		logger.Errorf("Error encoding json: %v", err)
		return err
	}

	res, err := a.performRequest(ctx, "POST", l, bytes.NewReader(b), func(req *http.Request) {
		req.Header.Set("Content-Type", MediaType)
	})
	if err != nil {
		return err
	}
	return res.Body.Close()
}

func (a *BasicTransferAdapter) performRequest(ctx context.Context, method string, l *Link, body io.Reader, callback func(*http.Request)) (*http.Response, error) {
	logger := log.FromContext(ctx).WithPrefix("lfs")
	logger.Debugf("Calling: %s %s", method, utils.RedactURL(l.Href))

	req, err := http.NewRequestWithContext(ctx, method, l.Href, body)
	if err != nil {
		err = redactURLError(err)
		logger.Errorf("Error creating request: %v", err)
		return nil, err
	}
	for key, value := range l.Header {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", MediaType)

	if callback != nil {
		callback(req)
	}

	res, err := a.client.Do(req)
	if err != nil {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		err = redactURLError(err)
		logger.Errorf("Error while processing request: %v", err)
		return res, err
	}

	if res.StatusCode != http.StatusOK {
		return res, handleErrorResponse(logger, res)
	}

	return res, nil
}

// handleErrorResponse closes resp's body and returns an error carrying its
// status and, when the body is an LFS error object with a message, that
// message.
func handleErrorResponse(logger *log.Logger, resp *http.Response) error {
	defer resp.Body.Close() //nolint: errcheck

	var er ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		logger.Debug("error decoding error response", "status", resp.Status, "err", err)
	}
	if er.Message == "" {
		return fmt.Errorf("request failed with status %s", resp.Status)
	}
	return fmt.Errorf("request failed with status %s: %s", resp.Status, er.Message)
}
