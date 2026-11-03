package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testMemoryRecorder(t *testing.T, dir string, now *time.Time) *memoryRecorder {
	t.Helper()
	r := newMemoryRecorder()
	r.now = func() time.Time { return *now }
	r.start = now.Truncate(time.Minute)
	r.keepFiles(dir, 3)
	r.file.now = r.now
	return r
}

func TestMemoryRecorderWritesAndReloads(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	// yesterday's file: one row too old for the chart, one recent enough
	os.WriteFile(filepath.Join(dir, "memory-2026-11-01.csv"), []byte(memoryHeader+"\n"+
		"2026-11-01T09:00:00Z,10,20\n"+
		"2026-11-01T10:30:00Z,11,21\n"+
		"not,a,row\n"), 0o640)
	r := testMemoryRecorder(t, dir, &now)

	// an unsampled minute writes nothing
	now = now.Add(time.Minute)
	r.flush(false)
	r.mu.Lock()
	r.runnerKB, r.totalKB, r.sampled = 12, 40, true
	r.mu.Unlock()
	now = now.Add(30 * time.Second)
	r.close()

	rows := csvRows(t, filepath.Join(dir, "memory-2026-11-02.csv"))
	if len(rows) != 1 || rows[0]["time"] != "2026-11-02T10:01:00Z" || rows[0]["runner_rss_max_kb"] != "12" || rows[0]["total_rss_max_kb"] != "40" {
		t.Fatalf("rows %v", rows)
	}

	// restarted within the same minute: the minute is merged, not repeated
	r2 := testMemoryRecorder(t, dir, &now)
	r2.mu.Lock()
	r2.runnerKB, r2.totalKB, r2.sampled = 15, 30, true
	r2.mu.Unlock()
	now = now.Add(10 * time.Second)
	r2.flush(false)
	points := r2.points()
	if len(points) != 2 {
		t.Fatalf("%d points, want 2: %+v", len(points), points)
	}
	if p := points[0]; p.RunnerKB != 11 || p.TotalKB != 21 {
		t.Errorf("yesterday's point %+v", p)
	}
	if p := points[1]; p.RunnerKB != 15 || p.TotalKB != 40 || !p.Time.Equal(time.Date(2026, 11, 2, 10, 1, 0, 0, time.UTC)) {
		t.Errorf("merged point %+v", p)
	}
}

func TestMemoryRecorderKeepsLastDay(t *testing.T) {
	r := newMemoryRecorder()
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for i := range metricsHistory + 5 {
		r.addPoint(memoryPoint{Time: start.Add(time.Duration(i) * time.Minute), RunnerKB: int64(i)})
	}
	points := r.points()
	if len(points) != metricsHistory || points[0].RunnerKB != 5 || points[len(points)-1].RunnerKB != metricsHistory+4 {
		t.Fatalf("%d points from %d to %d", len(points), points[0].RunnerKB, points[len(points)-1].RunnerKB)
	}
}
