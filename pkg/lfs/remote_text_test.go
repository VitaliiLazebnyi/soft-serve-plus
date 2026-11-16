package lfs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		res := BatchResponse{Transfer: TransferBasic, Objects: []*ObjectResponse{{
			Pointer: testPointer,
			Error:   &ObjectError{Code: http.StatusNotFound, Message: longRemoteText},
		}}}
		w.Header().Set("Content-Type", MediaType)
		json.NewEncoder(w).Encode(res) //nolint: errcheck,gosec
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(ErrorResponse{Message: longRemoteText}) //nolint: errcheck,gosec
	}))
	t.Cleanup(srv.Close)
	c := &HTTPClient{client: srv.Client()}

	rc, err := c.download(t.Context(), &Link{Href: srv.URL + "/obj"})
	if err == nil {
		rc.Close() //nolint: errcheck,gosec
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
	t.Cleanup(func() { l.Close() }) //nolint: errcheck,gosec
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
	reason := longRemoteText[:32<<10]

	t.Run("batch", func(t *testing.T) {
		c := plainClient(t, rawStatusServer(t, http.StatusBadGateway, reason)+"/x.git")
		_, err := c.batch(t.Context(), OperationDownload, []Pointer{testPointer})
		if err == nil {
			t.Fatal("batch succeeded")
		}
		assertCapped(t, "batch error", err.Error())
	})
	t.Run("download", func(t *testing.T) {
		c := plainClient(t, "http://unused.invalid/x.git")
		rc, err := c.download(t.Context(), &Link{Href: rawStatusServer(t, http.StatusNotFound, reason) + "/obj"})
		if err == nil {
			rc.Close() //nolint: errcheck,gosec
			t.Fatal("download succeeded")
		}
		assertCapped(t, "download error", err.Error())
	})
}

// remoteText cuts at a character boundary and marks the cut.
func TestRemoteText(t *testing.T) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		io.WriteString(w, `{"transfer":"basic","objects":[]`+strings.Repeat(" ", 1<<20)+`}`) //nolint: errcheck,gosec
	}))
	t.Cleanup(srv.Close)
	c := plainClient(t, srv.URL+"/x.git")
	if _, err := c.batch(t.Context(), OperationDownload, []Pointer{testPointer}); !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("err = %v, want %v", err, errResponseTooLarge)
	}
}
