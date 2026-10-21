package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// hangingServer never answers: each request blocks until the client gives up
// or the test ends. hits counts the requests that reached it.
func hangingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	// cleanups run last-in first-out: unblock handlers, then close the server
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })
	return srv, hits
}

// testClients builds clients with short timeouts; anything not set is long
// enough that hitting it would make the test fail on elapsed time.
func testClients(api, download, health time.Duration) *Clients {
	return newClients(&Config{
		APITimeout:         Duration(api),
		DownloadTimeout:    Duration(download),
		HealthCheckTimeout: Duration(health),
	})
}

func assertFailsWithin(t *testing.T, limit time.Duration, hits *atomic.Int32, call func() error) {
	t.Helper()
	start := time.Now()
	err := call()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a server that never answers")
	}
	if hits.Load() == 0 {
		t.Fatalf("request never reached the hanging server: %v", err)
	}
	if elapsed > limit {
		t.Errorf("gave up after %s, want under %s (err: %v)", elapsed, limit, err)
	}
}

func TestFindLatestArtifactTimesOut(t *testing.T) {
	srv, hits := hangingServer(t)
	clients := testClients(50*time.Millisecond, time.Minute, time.Minute)

	assertFailsWithin(t, time.Second, hits, func() error {
		_, err := FindLatestArtifact(clients.API, srv.URL, "owner/repo", "token", "", "")
		return err
	})
}

func TestDownloadArtifactTimesOutWaitingForHeaders(t *testing.T) {
	srv, hits := hangingServer(t)
	// the overall download timeout is long; the API timeout bounds the wait for headers
	clients := testClients(50*time.Millisecond, time.Minute, time.Minute)
	target := Artifact{ID: 7, Name: "app", ArchiveDownloadURL: srv.URL}

	assertFailsWithin(t, time.Second, hits, func() error {
		_, err := DownloadArtifact(clients.Download, target, "token", t.TempDir())
		return err
	})
}

func TestCheckNewVersionIsUpTimesOut(t *testing.T) {
	srv, hits := hangingServer(t)
	clients := testClients(time.Minute, time.Minute, 20*time.Millisecond)
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	// 5 attempts of 1ms interval + 20ms timeout each
	assertFailsWithin(t, 2*time.Second, hits, func() error {
		return checkNewVersionIsUp(clients.Health, "test", port, time.Millisecond)
	})
	if got := hits.Load(); got != 5 {
		t.Errorf("health check reached the server %d times, want 5", got)
	}
}
