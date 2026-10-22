package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestAdmin returns an admin server for user admin, password secret, with
// one app on external port 8000.
func newTestAdmin() *adminServer {
	return &adminServer{
		user:       "admin",
		password:   "secret",
		started:    time.Now(),
		runnerLogs: newLogBuffer(10),
		apps:       []*appMonitor{newAppMonitor(App{ExternalPort: 8000, Repo: "owner/app"})},
	}
}

func TestAdminRequiresBasicAuth(t *testing.T) {
	srv := httptest.NewServer(newTestAdmin().handler())
	defer srv.Close()

	for _, tc := range []struct {
		name           string
		user, password string
		auth           bool
		want           int
	}{
		{name: "no credentials", want: http.StatusUnauthorized},
		{name: "wrong user", user: "root", password: "secret", auth: true, want: http.StatusUnauthorized},
		{name: "wrong password", user: "admin", password: "secret2", auth: true, want: http.StatusUnauthorized},
		{name: "empty password", user: "admin", auth: true, want: http.StatusUnauthorized},
		{name: "right credentials", user: "admin", password: "secret", auth: true, want: http.StatusOK},
	} {
		for _, path := range []string{"/", "/status"} {
			req, _ := http.NewRequest("GET", srv.URL+path, nil)
			if tc.auth {
				req.SetBasicAuth(tc.user, tc.password)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("%s: GET %s answered %d, want %d", tc.name, path, resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") != `Basic realm="runner"` {
				t.Errorf("%s: GET %s sent WWW-Authenticate %q", tc.name, path, resp.Header.Get("WWW-Authenticate"))
			}
		}
	}
}

// get sends an authenticated GET to the admin handler and returns the recorder.
func get(t *testing.T, a *adminServer, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	a.handler().ServeHTTP(rec, req)
	return rec
}

func TestAdminServesPage(t *testing.T) {
	rec := get(t, newTestAdmin(), "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>Runner</title>") {
		t.Fatalf("GET / answered %d with %.100q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type %q, want text/html", ct)
	}
	if rec := get(t, newTestAdmin(), "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope answered %d, want 404", rec.Code)
	}
}

func TestAdminStatusJSON(t *testing.T) {
	a := newTestAdmin()
	started := time.Now()
	a.apps[0].update(func(s *appStatus) {
		s.State = stateRunning
		s.ArtifactID = 42
		s.ArtifactName = "app"
		s.Branch = "main"
		s.InternalPort = 50000
		s.PID = 1234
		s.StartedAt = started
		s.LastPoll = started
		s.FailedArtifactIDs = append(s.FailedArtifactIDs, 41)
	})
	a.apps[0].deployDone(41, started, "health check: failed to start")
	a.apps[0].deployDone(42, started, "")

	rec := get(t, a, "/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status answered %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}

	// decode generically to check the field names the page relies on
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, rec.Body)
	}
	for _, key := range []string{"version", "startedAt", "now", "apps"} {
		if _, ok := got[key]; !ok {
			t.Errorf("status has no %q: %s", key, rec.Body)
		}
	}
	apps := got["apps"].([]any)
	if len(apps) != 1 {
		t.Fatalf("status has %d apps, want 1", len(apps))
	}
	app := apps[0].(map[string]any)
	for key, want := range map[string]any{
		"name":                "owner/app",
		"externalPort":        float64(8000),
		"state":               "running",
		"error":               "",
		"deployingArtifactId": float64(0),
		"artifactId":          float64(42),
		"artifactName":        "app",
		"branch":              "main",
		"internalPort":        float64(50000),
		"pid":                 float64(1234),
		"restarts":            float64(0),
		"lastPollError":       "",
	} {
		if app[key] != want {
			t.Errorf("apps[0].%s = %v, want %v", key, app[key], want)
		}
	}
	for _, key := range []string{"startedAt", "lastPoll"} {
		if _, err := time.Parse(time.RFC3339, app[key].(string)); err != nil {
			t.Errorf("apps[0].%s: %v", key, err)
		}
	}
	// never polled yet
	if _, ok := app["nextPoll"]; ok {
		t.Errorf("apps[0].nextPoll is set although it is unknown: %v", app["nextPoll"])
	}
	if ids := app["failedArtifactIds"].([]any); len(ids) != 1 || ids[0] != float64(41) {
		t.Errorf("apps[0].failedArtifactIds = %v, want [41]", ids)
	}

	deploys := app["deploys"].([]any)
	if len(deploys) != 2 {
		t.Fatalf("apps[0].deploys has %d entries, want 2", len(deploys))
	}
	newest, oldest := deploys[0].(map[string]any), deploys[1].(map[string]any)
	if newest["artifactId"] != float64(42) || newest["result"] != "success" || newest["reason"] != "" {
		t.Errorf("newest deploy %v, want 42 success", newest)
	}
	if oldest["artifactId"] != float64(41) || oldest["result"] != "failed" || oldest["reason"] != "health check: failed to start" {
		t.Errorf("oldest deploy %v, want 41 failed with its reason", oldest)
	}
	if _, ok := newest["durationMs"].(float64); !ok {
		t.Errorf("deploy has no durationMs: %v", newest)
	}
}

func TestAppMonitorKeepsRecentDeploys(t *testing.T) {
	m := newAppMonitor(App{})
	for id := int64(1); id <= maxDeploys+5; id++ {
		m.deployDone(id, time.Now(), "")
	}
	deploys := m.snapshot().Deploys
	if len(deploys) != maxDeploys {
		t.Fatalf("kept %d deploys, want %d", len(deploys), maxDeploys)
	}
	if deploys[0].ArtifactID != maxDeploys+5 || deploys[maxDeploys-1].ArtifactID != 6 {
		t.Errorf("kept deploys %d..%d, want %d..6", deploys[0].ArtifactID, deploys[maxDeploys-1].ArtifactID, maxDeploys+5)
	}
}

// readEvents reads Server-Sent Events from sc until it has n data lines.
func readEvents(t *testing.T, sc *bufio.Scanner, n int) []logLine {
	t.Helper()
	var lines []logLine
	for len(lines) < n && sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var l logLine
		if err := json.Unmarshal([]byte(data), &l); err != nil {
			t.Fatalf("invalid event data %q: %v", data, err)
		}
		lines = append(lines, l)
	}
	if len(lines) < n {
		t.Fatalf("stream ended after %d events, want %d: %v", len(lines), n, sc.Err())
	}
	return lines
}

func TestAdminLogsReplaysThenStreams(t *testing.T) {
	a := newTestAdmin()
	a.runnerLogs.add("[MAIN] runner line")
	a.apps[0].logs.add("app line 1")
	a.apps[0].logs.add("app line 2")
	srv := httptest.NewServer(a.handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	open := func(query string) *bufio.Scanner {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/logs"+query, nil)
		req.SetBasicAuth("admin", "secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("GET /logs%s answered %d with %q", query, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		return bufio.NewScanner(resp.Body)
	}

	// an app's buffered lines first, then lines added while connected
	appStream := open("?app=8000")
	if got := texts(readEvents(t, appStream, 2)); strings.Join(got, ",") != "app line 1,app line 2" {
		t.Fatalf("replay %v", got)
	}
	a.apps[0].logs.add("app line 3")
	if got := texts(readEvents(t, appStream, 1)); got[0] != "app line 3" {
		t.Fatalf("live line %v, want app line 3", got)
	}

	// runner's own log, by default and by name
	for _, query := range []string{"", "?app=runner"} {
		if got := texts(readEvents(t, open(query), 1)); got[0] != "[MAIN] runner line" {
			t.Errorf("GET /logs%s replayed %v", query, got)
		}
	}

	if rec := get(t, a, "/logs?app=9999"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /logs?app=9999 answered %d, want 404", rec.Code)
	}
}
