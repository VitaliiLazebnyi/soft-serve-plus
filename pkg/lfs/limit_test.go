package lfs

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"charm.land/log/v2"
)

func TestDecodeJSONLimit(t *testing.T) {
	t.Parallel()
	const body = `{"message":"hi"}`
	tests := map[string]struct {
		limit   int64
		wantErr error
	}{
		"under limit":   {limit: int64(len(body)) + 10},
		"exactly limit": {limit: int64(len(body))},
		"over limit":    {limit: int64(len(body)) - 1, wantErr: errResponseTooLarge},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// One byte reads exercise the limit on every call.
			var er ErrorResponse
			err := decodeJSONLimit(iotest.OneByteReader(strings.NewReader(body)), tt.limit, &er)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && er.Message != "hi" {
				t.Fatalf("message = %q, want %q", er.Message, "hi")
			}
		})
	}
}

func TestHandleErrorResponseLimit(t *testing.T) {
	t.Parallel()
	body := `{"message":"` + strings.Repeat("x", maxErrorResponseSize) + `"}`
	logger, logs := debugLogger()
	res := &http.Response{
		Status:     "500 Internal Server Error",
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	err := handleErrorResponse(logger, res)
	if want := "request failed with status 500 Internal Server Error"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if !strings.Contains(logs.String(), errResponseTooLarge.Error()) {
		t.Fatalf("logs %q do not contain %q", logs, errResponseTooLarge)
	}
}

// oversizedBatchBody is a valid batch response, padded with whitespace past
// maxBatchResponseSize.
func oversizedBatchBody() string {
	return `{"transfer":"basic","objects":[]` + strings.Repeat(" ", maxBatchResponseSize) + `}`
}

func TestBatchResponseSizeLimit(t *testing.T) {
	t.Parallel()
	tests := map[string]http.HandlerFunc{
		"plain": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", MediaType)
			io.WriteString(w, oversizedBatchBody()) //nolint: errcheck
		},
		// The transport decompresses a gzip response it asked for, so a
		// small compressed body can expand far past the limit.
		"gzip": func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				t.Error("client did not ask for gzip")
			}
			w.Header().Set("Content-Type", MediaType)
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			io.WriteString(gz, oversizedBatchBody()) //nolint: errcheck
			gz.Close()                               //nolint: errcheck
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)
			c := plainClient(t, srv.URL+"/x.git")
			_, err := c.batch(t.Context(), OperationDownload, []Pointer{testPointer})
			if !errors.Is(err, errResponseTooLarge) {
				t.Fatalf("err = %v, want %v", err, errResponseTooLarge)
			}
		})
	}
}

func TestBatchRejectsUnrequestedObjects(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		res := BatchResponse{Transfer: TransferBasic}
		for range 3 {
			res.Objects = append(res.Objects, &ObjectResponse{Pointer: testPointer})
		}
		w.Header().Set("Content-Type", MediaType)
		json.NewEncoder(w).Encode(res) //nolint: errcheck
	}))
	t.Cleanup(srv.Close)
	c := plainClient(t, srv.URL+"/x.git")

	called := 0
	err := c.Download(t.Context(), []Pointer{testPointer}, func(Pointer, io.ReadCloser, error) error {
		called++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "3 objects, 1 were requested") {
		t.Fatalf("err = %v, want a too many objects error", err)
	}
	if called != 0 {
		t.Fatalf("callback called %d times, want 0", called)
	}
}

// longRemoteText is more text than the client keeps of anything a remote
// says, by far.
var longRemoteText = strings.Repeat("remote says ", 64<<10)

// assertCapped fails unless s keeps at most maxRemoteText bytes of
// longRemoteText.
func assertCapped(t *testing.T, what, s string) {
	t.Helper()
	if n := strings.Count(s, "remote says "); n*len("remote says ") > maxRemoteText {
		t.Errorf("%s keeps %d bytes of the remote's text, want at most %d", what, n*len("remote says "), maxRemoteText)
	}
	if !strings.Contains(s, "remote says") {
		t.Errorf("%s keeps none of the remote's text: %q", what, s)
	}
}

// A per-object error message in the batch response is cut short before the
// callback, which logs it, gets it.
func TestDownloadCapsObjectErrorMessage(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		res := BatchResponse{Transfer: TransferBasic, Objects: []*ObjectResponse{{
			Pointer: testPointer,
			Error:   &ObjectError{Code: http.StatusNotFound, Message: longRemoteText},
		}}}
		w.Header().Set("Content-Type", MediaType)
		json.NewEncoder(w).Encode(res) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	c := plainClient(t, srv.URL+"/x.git")

	var objectErr error
	err := c.Download(t.Context(), []Pointer{testPointer}, func(_ Pointer, _ io.ReadCloser, e error) error {
		objectErr = e
		return nil
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if objectErr == nil {
		t.Fatal("no error for the object")
	}
	assertCapped(t, "object error", objectErr.Error())
}

// The message of a failed transfer request is cut short too.
func TestDownloadCapsErrorResponseMessage(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(ErrorResponse{Message: longRemoteText}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	c := &HTTPClient{client: srv.Client(), logger: log.New(io.Discard)}

	rc, err := c.download(t.Context(), &Link{Href: srv.URL + "/obj"})
	if err == nil {
		rc.Close() //nolint:errcheck
		t.Fatal("download succeeded")
	}
	assertCapped(t, "error", err.Error())
}

// rawStatusServer answers every request with status line "HTTP/1.1 <code>
// <reason>" and an empty body, a reason httptest would not send.
func rawStatusServer(t *testing.T, code int, reason string) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() }) //nolint:errcheck
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() //nolint: errcheck
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", code, reason) //nolint: errcheck
			}()
		}
	}()
	return "http://" + l.Addr().String()
}

// The reason phrase of a status line is the remote's text as well.
func TestCapsRemoteStatus(t *testing.T) {
	t.Parallel()
	reason := longRemoteText[:32<<10]

	t.Run("batch", func(t *testing.T) {
		t.Parallel()
		c := plainClient(t, rawStatusServer(t, http.StatusBadGateway, reason)+"/x.git")
		_, err := c.batch(t.Context(), OperationDownload, []Pointer{testPointer})
		if err == nil {
			t.Fatal("batch succeeded")
		}
		assertCapped(t, "batch error", err.Error())
	})
	t.Run("download", func(t *testing.T) {
		t.Parallel()
		c := plainClient(t, "http://unused.invalid/x.git")
		rc, err := c.download(t.Context(), &Link{Href: rawStatusServer(t, http.StatusNotFound, reason) + "/obj"})
		if err == nil {
			rc.Close() //nolint:errcheck
			t.Fatal("download succeeded")
		}
		assertCapped(t, "download error", err.Error())
	})
}

// remoteText cuts at a character boundary and marks the cut.
func TestRemoteText(t *testing.T) {
	t.Parallel()
	if got := remoteText("short"); got != "short" {
		t.Errorf("remoteText(short) = %q", got)
	}
	s := strings.Repeat("a", maxRemoteText-1) + "é" + "tail"
	got := remoteText(s)
	if want := strings.Repeat("a", maxRemoteText-1) + "... (truncated)"; got != want {
		t.Errorf("remoteText = %q, want %q", got, want)
	}
}

// A batch response the client never needs, past 1 MiB, is refused: the
// client asks for at most 20 objects at a time.
func TestBatchResponseLimitIsOneMiB(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		io.WriteString(w, `{"transfer":"basic","objects":[]`+strings.Repeat(" ", 1<<20)+`}`) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	c := plainClient(t, srv.URL+"/x.git")
	if _, err := c.batch(t.Context(), OperationDownload, []Pointer{testPointer}); !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("err = %v, want %v", err, errResponseTooLarge)
	}
}

// remoteText escapes what a terminal would act on: control characters, an
// escape sequence and bytes that are not UTF-8.
func TestRemoteTextEscapes(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"clear \x1b[2J screen": `clear \x1b[2J screen`,
		"bell\a and\r\n":       `bell\a and\r\n`,
		"bad \xff byte":        `bad \xff byte`,
		"c1 \u009b csi":        `c1 \u009b csi`,
		"fine: é ✓ tab\tend":   `fine: é ✓ tab\tend`,
	} {
		if got := remoteText(in); got != want {
			t.Errorf("remoteText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Text with no character boundary near the cut, such as continuation bytes
// only, is still cut at maxRemoteText, not backed off to nothing.
func TestRemoteTextNoBoundary(t *testing.T) {
	t.Parallel()
	got := remoteText(strings.Repeat("\x80", 4*maxRemoteText))
	if want := strings.Repeat(`\x80`, maxRemoteText) + "... (truncated)"; got != want {
		t.Errorf("remoteText = %.80q..., want %d escaped bytes and the mark", got, maxRemoteText)
	}
}

// The transfer adapter and the hash algorithm a batch response names are
// the remote's text too.
func TestBatchCapsRemoteNames(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"transfer", "hash_algo"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", MediaType)
				body := map[string]any{"objects": []any{}, field: longRemoteText[:64<<10] + "\x1b[2J"}
				json.NewEncoder(w).Encode(body) //nolint:errcheck
			}))
			t.Cleanup(srv.Close)
			c := plainClient(t, srv.URL+"/x.git")
			err := c.Download(t.Context(), []Pointer{testPointer}, func(Pointer, io.ReadCloser, error) error { return nil })
			if err == nil {
				t.Fatal("Download succeeded")
			}
			assertCapped(t, field+" error", err.Error())
		})
	}
}

// An object the remote answers for without being asked is logged with its
// oid, which the remote chose: capped and escaped like its other text. (The
// logger drops ANSI escape sequences itself, but not other control
// characters, nor the length.)
func TestDownloadLogsUnrequestedOidEscaped(t *testing.T) {
	t.Parallel()
	oid := "\x07" + strings.Repeat("A", 64<<10) + "\x01"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		res := BatchResponse{Transfer: TransferBasic, Objects: []*ObjectResponse{{
			Pointer: Pointer{Oid: oid, Size: 1},
		}}}
		w.Header().Set("Content-Type", MediaType)
		json.NewEncoder(w).Encode(res) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	logger, logs := debugLogger()
	c := plainClient(t, srv.URL+"/x.git")
	c.logger = logger

	if err := c.Download(t.Context(), []Pointer{testPointer}, func(Pointer, io.ReadCloser, error) error { return nil }); err != nil {
		t.Fatalf("Download: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "AAAA") {
		t.Fatalf("the ignored object was not logged: %.200q", out)
	}
	if strings.ContainsAny(out, "\x07\x01") {
		t.Errorf("log holds the remote's control characters: %.200q", out)
	}
	if n := strings.Count(out, "A"); n > maxRemoteText {
		t.Errorf("log keeps %d bytes of the remote's oid, want at most %d", n, maxRemoteText)
	}
}
