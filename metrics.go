package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"hash/maphash"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// memorySampleInterval is how often the running process's memory is read.
const memorySampleInterval = 10 * time.Second

// maxClientsPerDay caps the clients remembered for clients_today; past it the
// count stops growing.
const maxClientsPerDay = 1_000_000

// metricsHistory is how many minutes of metrics are kept in memory for the
// admin UI's charts: one day.
const metricsHistory = 24 * 60

// maxAccessPathLength caps a path in the access log.
const maxAccessPathLength = 256

// latencyBuckets are the upper bounds, in milliseconds, of the latency
// histogram; one more bucket holds slower requests.
var latencyBuckets = [...]int64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// metricsHeader are the columns of the metrics CSV, one row per minute.
const metricsHeader = "time,artifact_id,pid,requests,status_2xx,status_3xx,status_4xx,status_5xx," +
	"bytes_out,p50_ms,p95_ms,max_ms,peak_rps,clients,clients_today,rss_max_kb"

// accessHeader are the columns of the access log CSV, one row per request.
const accessHeader = "time,method,path,status,ms,bytes,client"

// dataSettings are the settings of the files runner keeps in dataDir. They
// are read at startup only, so every app writes to the same place.
type dataSettings struct {
	dir                    string
	retentionDays          int
	accessLog              bool
	accessLogRetentionDays int
}

func newDataSettings(config *Config) dataSettings {
	return dataSettings{
		dir:                    config.DataDir,
		retentionDays:          config.RetentionDays,
		accessLog:              config.AccessLog,
		accessLogRetentionDays: config.AccessLogRetentionDays,
	}
}

// appDataDir is the directory of an app's files in dataDir, unique per app
// since external ports are, e.g. "owner_repo-8000".
func appDataDir(dataDir string, app App) string {
	name := unsafeFileChars.ReplaceAllString(app.Repo, "_")
	return filepath.Join(dataDir, name+"-"+strconv.Itoa(app.ExternalPort))
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// metricPoint is one minute of an app's metrics, for the admin UI's charts.
type metricPoint struct {
	Time      time.Time `json:"time"`
	Requests  int64     `json:"requests"`
	Status5xx int64     `json:"status5xx"`
	P95Ms     int64     `json:"p95Ms"`
	// RSSKB is null when memory was not read, e.g. on macOS
	RSSKB *int64 `json:"rssKb"`
}

// minuteStats are the requests of one minute.
type minuteStats struct {
	start    time.Time
	requests int64
	// status counts responses by class: index 2 for 2xx up to 5 for 5xx
	status  [6]int64
	bytes   int64
	latency [len(latencyBuckets) + 1]int64
	maxMs   int64
	// second is the unix second perSecond counts the requests of
	second    int64
	perSecond int64
	peakRPS   int64
	clients   map[uint64]struct{}
	// rssMaxKB is the highest memory sample of the minute, if rssSampled
	rssMaxKB   int64
	rssSampled bool
}

func newMinuteStats(now time.Time) *minuteStats {
	return &minuteStats{start: now.UTC().Truncate(time.Minute), clients: map[uint64]struct{}{}}
}

// recorder counts the requests through an app's proxy and samples the memory
// of its running process. Every minute it writes one row of totals to the
// metrics CSV and shows the latest numbers in the admin UI; with the access
// log on, it also writes a row per request. Client IPs are never stored: the
// access log has a hash salted with a random key that changes every UTC day
// and is kept in memory only. Without files it only feeds the admin UI.
type recorder struct {
	mon *appMonitor
	now func() time.Time
	// metrics and access are nil when not written
	metrics, access *dailyFile
	seed            maphash.Seed

	mu    sync.Mutex
	stats *minuteStats
	day   string
	today map[uint64]struct{}
	salt  []byte
	total int64
	// history is a ring of the last metricsHistory minutes; once full, the
	// oldest one is at next
	history []metricPoint
	next    int
	closed  bool
	stopped chan struct{}
}

func newRecorder(mon *appMonitor) *recorder {
	now := time.Now
	return &recorder{
		mon:     mon,
		now:     now,
		seed:    maphash.MakeSeed(),
		stats:   newMinuteStats(now()),
		today:   map[uint64]struct{}{},
		stopped: make(chan struct{}),
	}
}

// keepFiles makes the recorder write its files to dir.
func (r *recorder) keepFiles(dir string, data dataSettings) {
	r.metrics = newDailyFile(dir, "metrics", ".csv", data.retentionDays, logf)
	r.metrics.header = metricsHeader
	if data.accessLog {
		r.access = newDailyFile(dir, "access", ".csv", data.accessLogRetentionDays, logf)
		r.access.header = accessHeader
		r.access.maxBytes = maxDailyFileBytes
	}
}

// wrap returns next counting each request it serves.
func (r *recorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := r.now()
		sw := &statusWriter{ResponseWriter: w}
		// deferred, so a response the proxy aborts with a panic, when the
		// client goes away, is counted too
		defer func() { r.record(req, sw.statusCode(), sw.bytes, r.now().Sub(start), start) }()
		next.ServeHTTP(sw, req)
	})
}

// record counts one request that started at start.
func (r *recorder) record(req *http.Request, status int, bytes int64, elapsed time.Duration, start time.Time) {
	ms := elapsed.Milliseconds()
	ip := clientIP(req)
	client := maphash.String(r.seed, ip)

	r.mu.Lock()
	r.rollDay(start)
	s := r.stats
	s.requests++
	r.total++
	if class := status / 100; class >= 2 && class <= 5 {
		s.status[class]++
	}
	s.bytes += bytes
	s.latency[latencyBucket(ms)]++
	s.maxMs = max(s.maxMs, ms)
	if sec := start.Unix(); sec != s.second {
		s.second, s.perSecond = sec, 0
	}
	s.perSecond++
	s.peakRPS = max(s.peakRPS, s.perSecond)
	s.clients[client] = struct{}{}
	if len(r.today) < maxClientsPerDay {
		r.today[client] = struct{}{}
	}
	var hashed string
	if r.access != nil {
		mac := hmac.New(sha256.New, r.salt)
		mac.Write([]byte(ip))
		hashed = hex.EncodeToString(mac.Sum(nil)[:6])
	}
	r.mu.Unlock()

	if r.access != nil {
		r.access.writeLine(csvRow(
			start.UTC().Format(lineTimeFormat), req.Method, normalizePath(req.URL.Path),
			strconv.Itoa(status), strconv.FormatInt(ms, 10), strconv.FormatInt(bytes, 10), hashed))
	}
}

// rollDay starts counting the clients of a new UTC day, with a new salt.
func (r *recorder) rollDay(now time.Time) {
	day := now.UTC().Format(dayFormat)
	if day == r.day {
		return
	}
	r.day = day
	r.today = map[uint64]struct{}{}
	r.salt = make([]byte, 32)
	rand.Read(r.salt)
}

// sampleMemory reads the memory of the running process and its children.
// It is only available on Linux.
func (r *recorder) sampleMemory() {
	s := r.mon.snapshot()
	var kb int64
	ok := false
	if s.State == stateRunning && s.PID != 0 {
		kb, ok = processTreeRSS(s.PID)
	}
	r.mon.update(func(s *appStatus) { s.MemoryKB = kb })
	if !ok {
		return
	}
	r.mu.Lock()
	r.stats.rssMaxKB = max(r.stats.rssMaxKB, kb)
	r.stats.rssSampled = true
	r.mu.Unlock()
}

// flush writes the row of the minute that is over and starts the next one.
// A minute without requests is written only while a version is deployed.
// Once closed, only close's final flush writes.
func (r *recorder) flush(final bool) {
	now := r.now()
	r.mu.Lock()
	if r.closed && !final {
		r.mu.Unlock()
		return
	}
	s := r.stats
	r.stats = newMinuteStats(now)
	// the clients of the day that ends are counted before they are reset
	clientsToday := len(r.today)
	r.rollDay(now)
	total := r.total
	r.mu.Unlock()

	status := r.mon.snapshot()
	r.mon.update(func(st *appStatus) {
		st.RequestsLastMinute = s.requests
		st.RequestsTotal = total
	})
	if s.requests == 0 && status.ArtifactID == 0 {
		return
	}
	point := metricPoint{Time: s.start, Requests: s.requests, Status5xx: s.status[5], P95Ms: s.percentile(0.95)}
	rss := ""
	if s.rssSampled {
		rss = strconv.FormatInt(s.rssMaxKB, 10)
		kb := s.rssMaxKB
		point.RSSKB = &kb
	}
	r.mu.Lock()
	if len(r.history) < metricsHistory {
		r.history = append(r.history, point)
	} else {
		r.history[r.next] = point
		r.next = (r.next + 1) % metricsHistory
	}
	r.mu.Unlock()
	pid := ""
	if status.State == stateRunning {
		pid = strconv.Itoa(status.PID)
	}
	r.metrics.writeLineAt(s.start, csvRow(
		s.start.Format(time.RFC3339), strconv.FormatInt(status.ArtifactID, 10), pid,
		strconv.FormatInt(s.requests, 10),
		strconv.FormatInt(s.status[2], 10), strconv.FormatInt(s.status[3], 10),
		strconv.FormatInt(s.status[4], 10), strconv.FormatInt(s.status[5], 10),
		strconv.FormatInt(s.bytes, 10),
		strconv.FormatInt(s.percentile(0.50), 10), strconv.FormatInt(s.percentile(0.95), 10),
		strconv.FormatInt(s.maxMs, 10), strconv.FormatInt(s.peakRPS, 10),
		strconv.Itoa(len(s.clients)), strconv.Itoa(clientsToday), rss))
}

// points returns the minutes kept in memory, oldest first.
func (r *recorder) points() []metricPoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(slices.Clone(r.history[r.next:]), r.history[:r.next]...)
}

// run samples memory and writes a row every minute, on the minute, until
// stop is closed; then it writes the minute so far and closes the files.
func (r *recorder) run(stop <-chan struct{}) {
	sample := time.NewTicker(memorySampleInterval)
	defer sample.Stop()
	flush := time.NewTimer(untilNextMinute(r.now()))
	defer flush.Stop()
	r.sampleMemory()
	for {
		select {
		case <-stop:
			r.close()
			return
		case <-r.stopped:
			return
		case <-sample.C:
			r.sampleMemory()
		case <-flush.C:
			r.flush(false)
			flush.Reset(untilNextMinute(r.now()))
		}
	}
}

// close writes the minute so far and closes the files, once; runner calls it
// on shutdown, as the app's processes are stopped.
func (r *recorder) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.stopped)
	r.mu.Unlock()
	r.flush(true)
	r.metrics.Close()
	r.access.Close()
}

func untilNextMinute(now time.Time) time.Duration {
	return now.Truncate(time.Minute).Add(time.Minute).Sub(now)
}

// latencyBucket returns the histogram bucket of a request that took ms.
func latencyBucket(ms int64) int {
	for i, bound := range latencyBuckets {
		if ms <= bound {
			return i
		}
	}
	return len(latencyBuckets)
}

// percentile returns the upper bound of the latency bucket that holds the
// q quantile, so p95 = 100 means 95% of requests took at most 100ms. Beyond
// the last bucket it returns the slowest request.
func (s *minuteStats) percentile(q float64) int64 {
	if s.requests == 0 {
		return 0
	}
	rank := int64(q*float64(s.requests) + 0.999999)
	var seen int64
	for i, n := range s.latency {
		seen += n
		if seen >= rank {
			if i == len(latencyBuckets) {
				return s.maxMs
			}
			return min(latencyBuckets[i], s.maxMs)
		}
	}
	return s.maxMs
}

// clientIP returns the address of the client. Behind a proxy on the same
// host, such as cloudflared or Caddy, every request comes from a loopback
// address, so the client is taken from CF-Connecting-IP, or else from the
// last X-Forwarded-For entry, which the local proxy added; earlier entries
// come from the client and can be forged. From any other address the
// headers are ignored.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return host
	}
	if v := strings.TrimSpace(req.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := req.Header.Values("X-Forwarded-For"); len(v) > 0 {
		last := v[len(v)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if last = strings.TrimSpace(last); last != "" {
			return last
		}
	}
	return host
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// normalizePath replaces the IDs in a path, so the access log counts
// /items/123 and /items/456 as one endpoint: digits become :id, a UUID
// :uuid, 16 or more hex digits :hex, and a segment over 64 characters, such
// as a token, :long. The query string is never logged.
func normalizePath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		switch {
		case s == "":
		case isDigits(s):
			segments[i] = ":id"
		case uuidPattern.MatchString(s):
			segments[i] = ":uuid"
		case len(s) >= 16 && isHex(s):
			segments[i] = ":hex"
		case len(s) > 64:
			segments[i] = ":long"
		}
	}
	path = strings.Join(segments, "/")
	if len(path) > maxAccessPathLength {
		path = path[:maxAccessPathLength]
	}
	return path
}

func isDigits(s string) bool {
	return strings.Trim(s, "0123456789") == ""
}

func isHex(s string) bool {
	return strings.Trim(s, "0123456789abcdefABCDEF") == ""
}

// csvRow formats fields as one CSV line without the newline, quoting fields
// that need it, such as a path with a comma.
func csvRow(fields ...string) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	w.Write(fields)
	w.Flush()
	return strings.TrimSuffix(b.String(), "\n")
}

// statusWriter records the status and size of a response. Unwrap lets
// http.ResponseController reach the connection, which the reverse proxy
// uses to flush streamed responses and hijack upgraded ones.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	// a 1xx response such as 103 Early Hints comes before the final one
	if w.status == 0 || w.status < 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// statusCode is the status sent, 200 when the handler wrote nothing.
func (w *statusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
