package main

import (
	"encoding/csv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testRecorder returns a recorder writing to a temporary directory at a
// clock the test moves.
func testRecorder(t *testing.T, accessLog bool) (*recorder, *time.Time, string) {
	t.Helper()
	dir := t.TempDir()
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	mon := newAppMonitor(App{Repo: "owner/app", ExternalPort: 8000})
	r := mon.metrics
	r.now = func() time.Time { return now }
	r.stats = newMinuteStats(now)
	r.keepFiles(dir, dataSettings{dir: dir, retentionDays: 3, accessLog: accessLog, accessLogRetentionDays: 3})
	r.metrics.now = r.now
	if r.access != nil {
		r.access.now = r.now
	}
	return r, &now, dir
}

func request(remote, path string, headers ...string) *http.Request {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	return req
}

// csvRows reads a CSV file as maps of column to value.
func csvRows(t *testing.T, path string) []map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]string
	for _, rec := range records[1:] {
		row := map[string]string{}
		for i, col := range records[0] {
			row[col] = rec[i]
		}
		rows = append(rows, row)
	}
	return rows
}

func TestRecorderWritesMinuteRows(t *testing.T) {
	r, now, dir := testRecorder(t, false)
	r.mon.update(func(s *appStatus) {
		s.State = stateRunning
		s.ArtifactID = 42
		s.PID = 1234
	})

	start := *now
	r.record(request("192.0.2.1:5000", "/"), 200, 100, 3*time.Millisecond, start)
	r.record(request("192.0.2.1:5000", "/"), 201, 50, 30*time.Millisecond, start)
	r.record(request("192.0.2.2:5000", "/"), 404, 10, 8*time.Millisecond, start.Add(time.Second))
	r.record(request("192.0.2.3:5000", "/"), 502, 0, 2*time.Second, start.Add(2*time.Second))
	r.stats.rssMaxKB, r.stats.rssSampled = 2048, true

	*now = start.Add(time.Minute)
	r.flush(false)
	// a quiet minute is still written while a version is deployed
	r.record(request("192.0.2.1:5000", "/"), 200, 1, time.Millisecond, *now)
	*now = start.Add(2 * time.Minute)
	r.flush(false)
	r.close()

	rows := csvRows(t, filepath.Join(dir, "metrics-2026-11-02.csv"))
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3: %v", len(rows), rows)
	}
	want := map[string]string{
		"time": "2026-11-02T10:00:00Z", "artifact_id": "42", "pid": "1234", "requests": "4",
		"status_2xx": "2", "status_3xx": "0", "status_4xx": "1", "status_5xx": "1",
		"bytes_out": "160", "p50_ms": "10", "p95_ms": "2000", "max_ms": "2000",
		"peak_rps": "2", "clients": "3", "clients_today": "3", "rss_max_kb": "2048",
	}
	for col, v := range want {
		if rows[0][col] != v {
			t.Errorf("first row %s = %q, want %q", col, rows[0][col], v)
		}
	}
	if rows[1]["requests"] != "1" || rows[1]["clients"] != "1" || rows[1]["clients_today"] != "3" || rows[1]["rss_max_kb"] != "" {
		t.Errorf("second row %v", rows[1])
	}
	if rows[2]["requests"] != "0" || rows[2]["time"] != "2026-11-02T10:02:00Z" {
		t.Errorf("final row %v", rows[2])
	}
	if s := r.mon.snapshot(); s.RequestsTotal != 5 || s.RequestsLastMinute != 0 {
		t.Errorf("status total %d, last minute %d", s.RequestsTotal, s.RequestsLastMinute)
	}
}

func TestRecorderSkipsIdleMinutesBeforeDeploy(t *testing.T) {
	r, now, dir := testRecorder(t, false)
	*now = now.Add(time.Minute)
	r.flush(false)
	r.close()
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("files %v, want none without requests or a deployed version", got)
	}
}

func TestRecorderClientsTodayResetsAtMidnight(t *testing.T) {
	r, now, dir := testRecorder(t, false)
	*now = time.Date(2026, 11, 2, 23, 59, 10, 0, time.UTC)
	r.stats = newMinuteStats(*now)
	r.record(request("192.0.2.1:1", "/"), 200, 0, 0, *now)
	r.record(request("192.0.2.2:1", "/"), 200, 0, 0, *now)
	*now = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	r.flush(false)
	r.record(request("192.0.2.1:1", "/"), 200, 0, 0, *now)
	*now = now.Add(time.Minute)
	r.flush(false)
	r.close()

	day1 := csvRows(t, filepath.Join(dir, "metrics-2026-11-02.csv"))
	day2 := csvRows(t, filepath.Join(dir, "metrics-2026-11-03.csv"))
	if len(day1) != 1 || day1[0]["clients_today"] != "2" {
		t.Errorf("last row of the day %v, want clients_today 2", day1)
	}
	if len(day2) != 1 || day2[0]["clients_today"] != "1" {
		t.Errorf("first row of the next day %v, want clients_today 1", day2)
	}
}

func TestRecorderAccessLog(t *testing.T) {
	r, now, dir := testRecorder(t, true)
	r.record(request("127.0.0.1:1", "/items/123/a,b?token=secret", "CF-Connecting-IP", "198.51.100.7"), 200, 5, 12*time.Millisecond, *now)
	r.record(request("127.0.0.1:1", "/items/456", "CF-Connecting-IP", "198.51.100.7"), 200, 5, 1*time.Millisecond, *now)
	r.record(request("127.0.0.1:1", "/items/456", "CF-Connecting-IP", "198.51.100.8"), 200, 5, 1*time.Millisecond, *now)
	r.close()

	path := filepath.Join(dir, "access-2026-11-02.csv")
	content := readFile(t, path)
	for _, secret := range []string{"secret", "token", "198.51.100", "123"} {
		if strings.Contains(content, secret) {
			t.Errorf("access log holds %q:\n%s", secret, content)
		}
	}
	rows := csvRows(t, path)
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3", len(rows))
	}
	if rows[0]["path"] != "/items/:id/a,b" || rows[0]["ms"] != "12" || rows[0]["status"] != "200" {
		t.Errorf("first row %v", rows[0])
	}
	if rows[0]["client"] == "" || rows[0]["client"] != rows[1]["client"] || rows[1]["client"] == rows[2]["client"] {
		t.Errorf("clients %q %q %q, want the same hash for the same IP only", rows[0]["client"], rows[1]["client"], rows[2]["client"])
	}
}

func TestRecorderWrapCountsStatusAndBytes(t *testing.T) {
	r, _, _ := testRecorder(t, false)
	r.now = time.Now
	handler := r.wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/created":
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, "made")
		case "/stream":
			io.WriteString(w, "a")
			// the reverse proxy flushes through http.ResponseController
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush through the wrapper: %v", err)
			}
			io.WriteString(w, "bc")
		}
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()
	for _, path := range []string{"/created", "/stream", "/empty"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stats.requests != 3 || r.stats.status[2] != 3 || r.stats.bytes != 7 {
		t.Fatalf("requests %d, 2xx %d, bytes %d, want 3, 3, 7", r.stats.requests, r.stats.status[2], r.stats.bytes)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		headers []string
		want    string
	}{
		{"direct", "192.0.2.1:5000", nil, "192.0.2.1"},
		{"headers from a remote client are ignored", "192.0.2.1:5000", []string{"CF-Connecting-IP", "198.51.100.1", "X-Forwarded-For", "198.51.100.2"}, "192.0.2.1"},
		{"cloudflared", "127.0.0.1:5000", []string{"CF-Connecting-IP", "198.51.100.1", "X-Forwarded-For", "198.51.100.2"}, "198.51.100.1"},
		{"local proxy appends the client", "127.0.0.1:5000", []string{"X-Forwarded-For", "10.0.0.1, 198.51.100.2"}, "198.51.100.2"},
		{"several X-Forwarded-For headers", "[::1]:5000", []string{"X-Forwarded-For", "10.0.0.1", "X-Forwarded-For", "198.51.100.3"}, "198.51.100.3"},
		{"loopback without headers", "127.0.0.1:5000", nil, "127.0.0.1"},
	}
	for _, tt := range tests {
		if got := clientIP(request(tt.remote, "/", tt.headers...)); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	tests := map[string]string{
		"/":              "/",
		"/api/items/123": "/api/items/:id",
		"/users/550e8400-e29b-41d4-a716-446655440000": "/users/:uuid",
		"/commits/0123456789abcdef0123":               "/commits/:hex",
		"/files/" + strings.Repeat("x", 65):           "/files/:long",
		"/health":                                     "/health",
		"/v2/orders/7/lines/":                         "/v2/orders/:id/lines/",
	}
	for in, want := range tests {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normalizePath("/" + strings.Repeat("a/", 200)); len(got) != maxAccessPathLength {
		t.Errorf("long path kept %d characters, want %d", len(got), maxAccessPathLength)
	}
}

func TestPercentile(t *testing.T) {
	s := newMinuteStats(time.Now())
	for i := range 100 {
		ms := int64(1)
		if i >= 90 {
			ms = 20000
		}
		s.requests++
		s.latency[latencyBucket(ms)]++
		s.maxMs = max(s.maxMs, ms)
	}
	if p50 := s.percentile(0.5); p50 != 5 {
		t.Errorf("p50 = %d, want 5, the bound of the first bucket", p50)
	}
	if p95 := s.percentile(0.95); p95 != 20000 {
		t.Errorf("p95 = %d, want 20000 (beyond the last bucket)", p95)
	}
}

func TestAppDataDir(t *testing.T) {
	got := appDataDir("data", App{Repo: "owner/my app", ExternalPort: 8000})
	if want := filepath.Join("data", "owner_my_app-8000"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseProcFiles(t *testing.T) {
	if kb, ok := parseVmRSS("Name:\tapp\nVmPeak:\t  999 kB\nVmRSS:\t   12345 kB\n"); !ok || kb != 12345 {
		t.Errorf("parseVmRSS = %d, %v", kb, ok)
	}
	if _, ok := parseVmRSS("Name:\tzombie\nState:\tZ (zombie)\n"); ok {
		t.Error("parseVmRSS found memory of a zombie")
	}
	if ppid, ok := parsePPID("1234 (my (odd) app) S 99 1234 1234 0"); !ok || ppid != 99 {
		t.Errorf("parsePPID = %d, %v", ppid, ok)
	}
	if got := formatKB(1536); got != "1.5 MB" {
		t.Errorf("formatKB = %q", got)
	}
}

func TestRecorderKeepsLastDayInMemory(t *testing.T) {
	r, now, _ := testRecorder(t, false)
	r.metrics, r.access = nil, nil
	start := *now
	for i := range metricsHistory + 10 {
		r.record(request("192.0.2.1:1", "/"), 500, 0, time.Millisecond, *now)
		if i == 0 {
			r.stats.rssMaxKB, r.stats.rssSampled = 512, true
		}
		*now = now.Add(time.Minute)
		r.flush(false)
	}
	points := r.points()
	if len(points) != metricsHistory {
		t.Fatalf("%d points, want %d", len(points), metricsHistory)
	}
	if want := start.Add(10 * time.Minute); !points[0].Time.Equal(want) {
		t.Errorf("oldest point at %s, want %s", points[0].Time, want)
	}
	last := points[len(points)-1]
	if want := start.Add((metricsHistory + 9) * time.Minute); !last.Time.Equal(want) || last.Requests != 1 || last.Status5xx != 1 || last.RSSKB != nil {
		t.Errorf("newest point %+v, want 1 request, 1 5xx, no memory at %s", last, want)
	}
}
