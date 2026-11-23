package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// HTTPClient downloads objects from a Git LFS server over HTTP, with the
// batch API and the basic transfer adapter.
type HTTPClient struct {
	client   *http.Client
	endpoint Endpoint
	logger   *log.Logger
}

// stallTimeout is how long an LFS connection may go without sending or
// receiving any data before it is closed.
const stallTimeout = ssrf.ClientTimeout

// secureHTTPClient is the SSRF-protected HTTP client shared by every LFS
// client, so they all draw from one connection pool instead of each keeping
// its own idle connections. Its transfer timeouts are set once here, as
// setTransferTimeouts wraps the shared transport's dialer. Nothing changes
// it after.
var secureHTTPClient = newTransferClient(ssrf.NewSecureClient(), stallTimeout)

// newTransferClient sets up client for LFS transfers, with stall as the
// stall timeout of its connections, and returns it. Tests pass a client that
// may reach loopback, so the stall tests run through the same setup as
// secureHTTPClient.
func newTransferClient(client *http.Client, stall time.Duration) *http.Client {
	setTransferTimeouts(client, stall)
	return client
}

// newHTTPClient returns a new Git LFS client that logs to logger, or to
// log.Default() when it is nil.
func newHTTPClient(endpoint Endpoint, logger *log.Logger) *HTTPClient {
	if logger == nil {
		logger = log.Default()
	}
	return &HTTPClient{client: secureHTTPClient, endpoint: endpoint, logger: logger.WithPrefix("lfs")}
}

// setTransferTimeouts replaces the client's whole-request timeout, which also
// covers reading the response body and so cuts off any object that takes
// longer than it to transfer, with an idle timeout on each connection: a
// transfer may take as long as it needs while data keeps moving, and a stalled
// one still fails. It panics if the client's transport is not an
// *http.Transport with a DialContext, since it could not add the stall limit
// and transfers would then have no bound at all.
func setTransferTimeouts(client *http.Client, stall time.Duration) {
	t, ok := client.Transport.(*http.Transport)
	if !ok || t.DialContext == nil {
		panic(fmt.Sprintf("lfs: cannot add a stall timeout to transport %T", client.Transport))
	}
	client.Timeout = 0
	dial := t.DialContext
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &stallTimeoutConn{Conn: conn, timeout: stall}, nil
	}
}

// stallTimeoutConn pushes the connection's deadline forward on every read and
// write, so it only expires once neither direction has made progress for the
// timeout. Setting the deadline from either side also extends a read already
// waiting, such as the transport's wait for a response while a large upload
// is still being written.
type stallTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *stallTimeoutConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err //nolint:wrapcheck
	}
	return c.Conn.Read(b) //nolint:wrapcheck
}

func (c *stallTimeoutConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err //nolint:wrapcheck
	}
	return c.Conn.Write(b) //nolint:wrapcheck
}

// batch performs a batch request to the LFS server.
func (c *HTTPClient) batch(ctx context.Context, operation string, objects []Pointer) (*BatchResponse, error) {
	logger := c.logger
	url := c.endpoint.JoinPath("objects", "batch").String()

	// TODO: support ref
	request := &BatchRequest{
		Operation: operation,
		Transfers: []string{TransferBasic},
		Objects:   objects,
		HashAlgo:  HashAlgorithmSHA256,
	}

	payload := new(bytes.Buffer)
	err := json.NewEncoder(payload).Encode(request)
	if err != nil {
		logger.Errorf("Error encoding json: %v", err)
		return nil, err
	}

	logger.Debugf("Calling: %s", utils.RedactURL(url))

	req, err := http.NewRequestWithContext(ctx, "POST", url, payload)
	if err != nil {
		err = utils.RedactURLError(err, utils.RedactURL)
		logger.Errorf("Error creating request: %v", err)
		return nil, err
	}
	req.Header.Set("Content-type", MediaType)
	req.Header.Set("Accept", MediaType)

	res, err := c.client.Do(req)
	if err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		err = utils.RedactURLError(err, utils.RedactURL)
		logger.Errorf("Error while processing request: %v", err)
		return nil, err
	}
	defer res.Body.Close() //nolint: errcheck

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected server response: %s", remoteText(res.Status))
	}

	var response BatchResponse
	err = decodeJSONLimit(res.Body, maxBatchResponseSize, &response)
	if err != nil {
		logger.Errorf("Error decoding json: %v", err)
		return nil, err
	}

	for _, object := range response.Objects {
		if object == nil {
			return nil, errors.New("invalid batch response: null object")
		}
	}

	// A response may answer for fewer objects than were asked for, but
	// never for more.
	if len(response.Objects) > len(objects) {
		return nil, fmt.Errorf("lfs: batch response has %d objects, %d were requested", len(response.Objects), len(objects))
	}

	if len(response.Transfer) == 0 {
		response.Transfer = TransferBasic
	}

	// The oids were sent as SHA-256 hashes. A server that answers in another
	// hash algorithm names different objects, whose content would never
	// verify against the pointers. An empty hash_algo means sha256.
	if response.HashAlgo != "" && response.HashAlgo != HashAlgorithmSHA256 {
		return nil, fmt.Errorf("lfs: batch response uses hash algorithm %q, %q was requested", remoteText(response.HashAlgo), HashAlgorithmSHA256)
	}

	return &response, nil
}

// actionLink returns the link of the named action of an object. ok is false
// when the server left the action out; a link that is null or has no href is
// an error, as there is nothing to transfer to or from.
func actionLink(object *ObjectResponse, action string) (link *Link, ok bool, err error) {
	link, ok = object.Actions[action]
	if !ok {
		return nil, false, nil
	}
	if link == nil || link.Href == "" {
		return nil, false, fmt.Errorf("invalid action '%s': no href", action)
	}
	return link, true, nil
}

// Download asks the server for objects in one batch
// request and calls dc with each one it answers for.
func (c *HTTPClient) Download(ctx context.Context, objects []Pointer, dc DownloadCallback) error {
	logger := c.logger
	if len(objects) == 0 {
		return nil
	}

	result, err := c.batch(ctx, OperationDownload, objects)
	if err != nil {
		return err
	}

	if result.Transfer != TransferBasic {
		return fmt.Errorf("transfer adapter not supported: %s", remoteText(result.Transfer))
	}

	// Only act on objects that were asked for, once each, so a response
	// listing other objects or the same one twice transfers nothing more.
	requested := make(map[string]bool, len(objects))
	for _, p := range objects {
		requested[p.Oid] = true
	}

	for _, object := range result.Objects {
		if !requested[object.Oid] {
			// The oid is the remote's choice, unchecked here.
			logger.Debug("ignoring an object that was not requested or was already handled", "oid", remoteText(object.Oid))
			continue
		}
		delete(requested, object.Oid)

		if object.Error != nil {
			objectError := errors.New(remoteText(object.Error.Message))
			logger.Debugf("Error on object %v: %v", object.Pointer, objectError)
			if err := dc(object.Pointer, nil, objectError); err != nil {
				return err
			}
			continue
		}

		// A download that fails because of the object, not the
		// connection or the server, only fails its own object, like an
		// error the server reports for it in the batch response. A
		// download link that is null or has no href is a malformed
		// response and still fails the whole transfer.
		link, ok, err := actionLink(object, ActionDownload)
		if err != nil {
			return err
		}
		var content io.ReadCloser
		if ok {
			content, err = c.download(ctx, link)
		} else {
			logger.Debugf("%v has no usable action", object.Pointer)
			err = errMissingDownload
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !isObjectError(err) {
				return err
			}
			logger.Debugf("Error on object %v: %v", object.Pointer, err)
			if err := dc(object.Pointer, nil, err); err != nil {
				return err
			}
			continue
		}

		if err := dc(object.Pointer, content, nil); err != nil {
			return err
		}
	}

	return nil
}

// errMissingDownload is the error of an object the batch response gives no
// download action for.
var errMissingDownload = errors.New("missing action 'download'")

// isObjectError reports whether err, the error of downloading one object,
// is about that object rather than the connection or the server: the object
// has no download action or an href that doesn't parse, or its href answers
// 404 Not Found, 410 Gone or a redirect, which is refused. Trying again
// won't change the outcome of those, so they only fail their own object.
// Any other error, such as a connection that fails or times out, a 5xx, or
// a 401 or 403 from an expired href, may well pass on a later try, so it
// stops the download: a plain import that is told an object is unavailable
// never fetches it again.
func isObjectError(err error) bool {
	if errors.Is(err, errMissingDownload) || errors.Is(err, utils.ErrInvalidURL) {
		return true
	}
	var serr *statusError
	if !errors.As(err, &serr) {
		return false
	}
	switch {
	case serr.code == http.StatusNotFound, serr.code == http.StatusGone:
		return true
	case serr.code >= 300 && serr.code < 400:
		return true
	}
	return false
}

// download fetches an object from its download link, as the basic transfer
// adapter does, and returns its content.
func (c *HTTPClient) download(ctx context.Context, l *Link) (io.ReadCloser, error) {
	logger := c.logger
	logger.Debugf("Calling: %s %s", http.MethodGet, utils.RedactURL(l.Href))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.Href, nil)
	if err != nil {
		err = utils.RedactURLError(err, utils.RedactURL)
		logger.Errorf("Error creating request: %v", err)
		return nil, err
	}
	for key, value := range l.Header {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", MediaType)

	res, err := c.client.Do(req)
	if err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		err = utils.RedactURLError(err, utils.RedactURL)
		logger.Errorf("Error while processing request: %v", err)
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, &statusError{code: res.StatusCode, err: handleErrorResponse(logger, res)}
	}

	return res.Body, nil
}

// statusError is the error of a transfer request the server answered with a
// status other than 200 OK. It reads as err, the error handleErrorResponse
// built from the response.
type statusError struct {
	code int
	err  error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

// handleErrorResponse closes resp's body and returns an error carrying its
// status and, when the body is an LFS error object with a message, that
// message.
func handleErrorResponse(logger *log.Logger, resp *http.Response) error {
	defer resp.Body.Close() //nolint: errcheck

	var er ErrorResponse
	if err := decodeJSONLimit(resp.Body, maxErrorResponseSize, &er); err != nil {
		logger.Debug("error decoding error response", "status", remoteText(resp.Status), "err", err)
	}
	if er.Message == "" {
		return fmt.Errorf("request failed with status %s", remoteText(resp.Status))
	}
	return fmt.Errorf("request failed with status %s: %s", remoteText(resp.Status), remoteText(er.Message))
}
