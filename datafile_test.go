package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// files returns the names in dir, sorted.
func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDailyFileRotatesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	// files of other prefixes and unrelated names are left alone
	for _, name := range []string{"metrics-2026-10-01.csv", "metrics-2026-10-29.csv", "output-2026-10-01.log", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, name), []byte("old\n"), 0o640)
	}
	now := time.Date(2026, 11, 1, 23, 59, 0, 0, time.UTC)
	d := newDailyFile(dir, "metrics", ".csv", 3, t.Logf)
	d.header = "a,b"
	d.now = func() time.Time { return now }

	d.writeLine("1,2")
	now = now.Add(2 * time.Minute)
	d.writeLine("3,4")
	d.writeLine("5,6")
	d.Close()

	// 2026-10-29 was kept on 2026-11-01, but is more than 3 days before 2026-11-02
	want := []string{"metrics-2026-11-01.csv", "metrics-2026-11-02.csv", "notes.txt", "output-2026-10-01.log"}
	if got := files(t, dir); !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	if got := readFile(t, filepath.Join(dir, "metrics-2026-11-01.csv")); got != "a,b\n1,2\n" {
		t.Errorf("first day %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "metrics-2026-11-02.csv")); got != "a,b\n3,4\n5,6\n" {
		t.Errorf("second day %q", got)
	}

	// after a restart the header is not written again
	d2 := newDailyFile(dir, "metrics", ".csv", 3, t.Logf)
	d2.header = "a,b"
	d2.now = func() time.Time { return now }
	d2.writeLine("7,8")
	d2.Close()
	if got := readFile(t, filepath.Join(dir, "metrics-2026-11-02.csv")); got != "a,b\n3,4\n5,6\n7,8\n" {
		t.Errorf("after reopening %q", got)
	}
}

func TestDailyFileCapsSize(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	d := newDailyFile(dir, "output", ".log", 3, t.Logf)
	d.maxBytes = 20
	d.now = func() time.Time { return now }
	for i := range 10 {
		d.writeLine(fmt.Sprintf("line %d", i))
	}
	got := readFile(t, filepath.Join(dir, "output-2026-11-02.log"))
	if !strings.HasPrefix(got, "line 0\nline 1\n") || strings.Contains(got, "line 3") || !strings.Contains(got, "dropping lines until the next day") {
		t.Fatalf("capped file %q", got)
	}

	// the next day starts a new file
	now = now.Add(24 * time.Hour)
	d.writeLine("next day")
	d.Close()
	if got := readFile(t, filepath.Join(dir, "output-2026-11-03.log")); got != "next day\n" {
		t.Fatalf("next day %q", got)
	}
}

func TestDailyFileReportsFailureOnce(t *testing.T) {
	dir := t.TempDir()
	// a file where the directory should be makes every write fail
	blocked := filepath.Join(dir, "app")
	os.WriteFile(blocked, nil, 0o640)
	var reports []string
	d := newDailyFile(blocked, "output", ".log", 3, func(format string, args ...any) {
		reports = append(reports, fmt.Sprintf(format, args...))
	})
	d.writeLine("a")
	d.writeLine("b")
	if len(reports) != 1 || !strings.Contains(reports[0], "Failed to write") {
		t.Fatalf("reports %q, want one failure", reports)
	}

	os.Remove(blocked)
	d.writeLine("c")
	d.Close()
	if len(reports) != 2 || !strings.Contains(reports[1], "again") {
		t.Fatalf("reports %q, want a recovery", reports)
	}
	if got := readFile(t, d.path()); got != "c\n" {
		t.Fatalf("file %q, want only the line after recovery", got)
	}
}

func TestDailyFileNil(t *testing.T) {
	var d *dailyFile
	d.writeLine("ignored")
	d.Close()
}

func TestLogBufferKeepsFile(t *testing.T) {
	dir := t.TempDir()
	b := newLogBuffer(10)
	b.file = newDailyFile(dir, "output", ".log", 3, t.Logf)
	b.add("hello")
	b.file.Close()
	got := readFile(t, b.file.path())
	if !strings.HasSuffix(got, "Z hello\n") {
		t.Fatalf("file %q, want a UTC timestamp and the line", got)
	}
	if _, err := time.Parse(lineTimeFormat, strings.Fields(got)[0]); err != nil {
		t.Fatalf("timestamp: %v", err)
	}
}
