package main

import (
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

// memoryHeader are the columns of runner's memory CSV, one row per minute.
const memoryHeader = "time,runner_rss_max_kb,total_rss_max_kb"

// memoryPoint is one minute of the memory used on the machine by runner, for
// the admin UI's chart: runner itself, and the total of runner and every
// process it started, which are the apps and any of their children.
type memoryPoint struct {
	Time     time.Time `json:"time"`
	RunnerKB int64     `json:"runnerKb"`
	TotalKB  int64     `json:"totalKb"`
}

// memoryRecorder samples runner's own memory and the total of its process
// tree every memorySampleInterval and keeps the highest of each minute, like
// an app's recorder. It is only available on Linux; elsewhere it records
// nothing.
type memoryRecorder struct {
	now  func() time.Time
	pid  int
	file *dailyFile

	mu sync.Mutex
	// the minute being sampled, and its highest samples if sampled
	start             time.Time
	runnerKB, totalKB int64
	sampled           bool
	// latest is the last sample, nil before the first one
	latest *memoryPoint
	// history is a ring of the last metricsHistory minutes; once full, the
	// oldest one is at next
	history []memoryPoint
	next    int
	closed  bool
	stopped chan struct{}
}

func newMemoryRecorder() *memoryRecorder {
	now := time.Now
	return &memoryRecorder{
		now:     now,
		pid:     os.Getpid(),
		start:   now().UTC().Truncate(time.Minute),
		stopped: make(chan struct{}),
	}
}

// keepFiles makes the recorder write memory-<day>.csv to dir and loads the
// last day from it.
func (r *memoryRecorder) keepFiles(dir string, retentionDays int) {
	r.file = newDailyFile(dir, "memory", ".csv", retentionDays, logf)
	r.file.header = memoryHeader
	now := r.now().UTC()
	since := now.Add(-metricsHistory * time.Minute)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, day := range []time.Time{now.AddDate(0, 0, -1), now} {
		for _, p := range readMemoryPoints(filepath.Join(dir, "memory-"+day.Format(dayFormat)+".csv")) {
			if p.Time.After(since) {
				r.addPoint(p)
			}
		}
	}
}

// readMemoryPoints reads a memory file, by column name, skipping rows that
// do not parse.
func readMemoryPoints(path string) []memoryPoint {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	reader := csv.NewReader(f)
	// a row cut short by a crash has fewer fields
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil
	}
	col := map[string]int{}
	for i, name := range header {
		col[name] = i
	}
	for _, name := range []string{"time", "runner_rss_max_kb", "total_rss_max_kb"} {
		if _, ok := col[name]; !ok {
			return nil
		}
	}
	var points []memoryPoint
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(rec) != len(header) {
			continue
		}
		t, err1 := time.Parse(time.RFC3339, rec[col["time"]])
		runner, err2 := strconv.ParseInt(rec[col["runner_rss_max_kb"]], 10, 64)
		total, err3 := strconv.ParseInt(rec[col["total_rss_max_kb"]], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		points = append(points, memoryPoint{Time: t, RunnerKB: runner, TotalKB: total})
	}
	return points
}

// sample reads the memory of runner and of its process tree.
func (r *memoryRecorder) sample() {
	runner, ok1 := processRSS(r.pid)
	total, ok2 := processTreeRSS(r.pid)
	if !ok1 || !ok2 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runnerKB = max(r.runnerKB, runner)
	r.totalKB = max(r.totalKB, total)
	r.sampled = true
	r.latest = &memoryPoint{Time: r.now(), RunnerKB: runner, TotalKB: total}
}

// flush writes the row of the minute that is over, if it was sampled, and
// starts the next one. Once closed, only close's final flush writes.
func (r *memoryRecorder) flush(final bool) {
	r.mu.Lock()
	if r.closed && !final {
		r.mu.Unlock()
		return
	}
	p := memoryPoint{Time: r.start, RunnerKB: r.runnerKB, TotalKB: r.totalKB}
	sampled := r.sampled
	r.start = r.now().UTC().Truncate(time.Minute)
	r.runnerKB, r.totalKB, r.sampled = 0, 0, false
	if sampled {
		r.addPoint(p)
	}
	r.mu.Unlock()
	if sampled {
		r.file.writeLineAt(p.Time, csvRow(p.Time.Format(time.RFC3339),
			strconv.FormatInt(p.RunnerKB, 10), strconv.FormatInt(p.TotalKB, 10)))
	}
}

// addPoint adds a minute to the history, under r.mu. A minute that is
// already the newest one, written on shutdown and again after a restart
// within the same minute, is merged into it.
func (r *memoryRecorder) addPoint(p memoryPoint) {
	if n := len(r.history); n > 0 {
		last := &r.history[(r.next+n-1)%n]
		if last.Time.Equal(p.Time) {
			last.RunnerKB = max(last.RunnerKB, p.RunnerKB)
			last.TotalKB = max(last.TotalKB, p.TotalKB)
			return
		}
	}
	if len(r.history) < metricsHistory {
		r.history = append(r.history, p)
	} else {
		r.history[r.next] = p
		r.next = (r.next + 1) % metricsHistory
	}
}

// points returns the minutes kept in memory, oldest first.
func (r *memoryRecorder) points() []memoryPoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(slices.Clone(r.history[r.next:]), r.history[:r.next]...)
}

// current returns the last sample, or nil before the first one or off Linux.
func (r *memoryRecorder) current() *memoryPoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latest == nil {
		return nil
	}
	p := *r.latest
	return &p
}

// run samples memory and writes a row every minute, on the minute, until
// stop is closed; then it writes the minute so far and closes the file.
func (r *memoryRecorder) run(stop <-chan struct{}) {
	sample := time.NewTicker(memorySampleInterval)
	defer sample.Stop()
	flush := time.NewTimer(untilNextMinute(r.now()))
	defer flush.Stop()
	r.sample()
	for {
		select {
		case <-stop:
			r.close()
			return
		case <-r.stopped:
			return
		case <-sample.C:
			r.sample()
		case <-flush.C:
			r.flush(false)
			flush.Reset(untilNextMinute(r.now()))
		}
	}
}

// close writes the minute so far and closes the file, once.
func (r *memoryRecorder) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.stopped)
	r.mu.Unlock()
	r.flush(true)
	r.file.Close()
}
