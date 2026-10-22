package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRunAppReturnsWhenExternalPortIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	// an API that always fails, so runApp never deploys anything if it does not return
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer api.Close()

	config := &Config{
		GithubAPI:           api.URL,
		PollInterval:        Duration(10 * time.Millisecond),
		HealthCheckInterval: Duration(10 * time.Millisecond),
		APITimeout:          Duration(time.Second),
		DownloadTimeout:     Duration(time.Second),
		HealthCheckTimeout:  Duration(time.Second),
		StopTimeout:         Duration(time.Second),
	}
	app := App{ExternalPort: taken.Addr().(*net.TCPAddr).Port, Repo: "test/app", BinName: "app"}

	done := make(chan struct{})
	go func() {
		runApp(app, config, newClients(config), newProcessSet())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runApp kept running although external port %d is taken", app.ExternalPort)
	}
}

func TestFreePortCanBeBound(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if port <= 0 {
		t.Fatalf("freePort returned port %d", port)
	}

	// apps bind the port on all interfaces, e.g. ":<port>"
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("port %d from freePort cannot be bound: %v", port, err)
	}
	listener.Close()
}
