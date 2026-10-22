package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Shorten production deadlines so real socket timeout tests finish promptly.
type shortWriteDeadline struct{ http.ResponseWriter }

func (w shortWriteDeadline) SetWriteDeadline(time.Time) error {
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
}
func (w shortWriteDeadline) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func TestStalledSSEClient(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done <- writeSSE(shortWriteDeadline{w}, "epoch", 1, []UIEvent{{T: "delta", S: strings.Repeat("x", 16<<20)}})
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
	// Deliberately do not read the response.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected stalled socket write to fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled writer did not stop")
	}
}

func TestSSEDeadlineRefresh(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream := shortWriteDeadline{w}
		if err := writeSSE(stream, "epoch", 1, nil); err != nil {
			done <- err
			return
		}
		time.Sleep(200 * time.Millisecond)
		done <- writeSSE(stream, "epoch", 2, nil)
	}))
	defer srv.Close()
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scanner := bufio.NewScanner(res.Body)
	count := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "id:") {
			count++
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("got %d events, want 2", count)
	}
}
