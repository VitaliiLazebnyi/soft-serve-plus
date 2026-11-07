package lfs

import (
	"context"
	"encoding/json"
	"errors"
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
	resp, err := a.performRequest(ctx, http.MethodGet, l)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (a *BasicTransferAdapter) performRequest(ctx context.Context, method string, l *Link) (*http.Response, error) {
	logger := log.FromContext(ctx).WithPrefix("lfs")
	logger.Debugf("Calling: %s %s", method, utils.RedactURL(l.Href))

	req, err := http.NewRequestWithContext(ctx, method, l.Href, nil)
	if err != nil {
		err = redactURLError(err)
		logger.Errorf("Error creating request: %v", err)
		return nil, err
	}
	for key, value := range l.Header {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", MediaType)

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
		return res, handleErrorResponse(res)
	}

	return res, nil
}

func handleErrorResponse(resp *http.Response) error {
	defer resp.Body.Close() //nolint: errcheck

	er, err := decodeResponseError(resp.Body)
	if err != nil {
		return fmt.Errorf("request failed with status %s", resp.Status)
	}
	return errors.New(er.Message)
}

func decodeResponseError(r io.Reader) (ErrorResponse, error) {
	var er ErrorResponse
	err := json.NewDecoder(r).Decode(&er)
	if err != nil {
		log.Error("Error decoding json: %v", err)
	}
	return er, err
}
