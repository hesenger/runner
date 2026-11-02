package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxDailyFileBytes caps one day's file of app output or access log, so a
// noisy app cannot fill the disk. Metrics files stay far below it.
const maxDailyFileBytes = 256 << 20

// dayFormat is the date in the name of a daily file, in UTC.
const dayFormat = "2006-01-02"

// dailyFile appends lines to <dir>/<prefix>-<date><ext>, one file per UTC day.
// When it opens a new day's file it deletes this prefix's files older than
// keepDays. A failed write is reported once and the line dropped, so a full
// disk never stops runner or an app; the next line tries again. All methods
// are safe for concurrent use, and do nothing on a nil file.
type dailyFile struct {
	dir, prefix, ext string
	keepDays         int
	// maxBytes caps one day's file, 0 for no cap; once it is reached, lines
	// are dropped until the next day
	maxBytes int64
	// header is written first to every new file, e.g. the columns of a CSV
	header string
	// report logs a failed write; it must not write to this file
	report func(format string, args ...any)
	now    func() time.Time

	mu      sync.Mutex
	day     string
	f       *os.File
	size    int64
	full    bool
	failing bool
}

func newDailyFile(dir, prefix, ext string, keepDays int, report func(format string, args ...any)) *dailyFile {
	return &dailyFile{dir: dir, prefix: prefix, ext: ext, keepDays: keepDays, report: report, now: time.Now}
}

// writeLine appends line and a newline to the file of the current day.
func (d *dailyFile) writeLine(line string) {
	if d == nil {
		return
	}
	d.writeLineAt(d.now(), line)
}

// writeLineAt appends line and a newline to the file of the day of at, so a
// row for the last minute of a day lands in that day's file.
func (d *dailyFile) writeLineAt(at time.Time, line string) {
	if d == nil {
		return
	}
	var failure string
	d.mu.Lock()
	err := d.writeLocked(at.UTC().Format(dayFormat), line+"\n")
	switch {
	case err != nil && !d.failing:
		d.failing = true
		failure = fmt.Sprintf("Failed to write %s, dropping lines until it works again: %v", d.path(), err)
	case err == nil && d.failing:
		d.failing = false
		failure = fmt.Sprintf("Writing %s again", d.path())
	}
	d.mu.Unlock()
	// report outside the lock, since it may write to another daily file
	if failure != "" {
		d.report("[MAIN] %s\n", failure)
	}
}

func (d *dailyFile) writeLocked(day, line string) error {
	if day != d.day || d.f == nil {
		if err := d.open(day); err != nil {
			return err
		}
	}
	if d.full {
		return nil
	}
	if d.maxBytes > 0 && d.size+int64(len(line)) > d.maxBytes {
		d.full = true
		line = fmt.Sprintf("%s runner: this file reached %d MB, dropping lines until the next day (UTC)\n",
			d.now().UTC().Format(time.RFC3339), d.maxBytes>>20)
	}
	n, err := d.f.WriteString(line)
	d.size += int64(n)
	return err
}

// open switches to the file of day, creating it, and deletes old files.
func (d *dailyFile) open(day string) error {
	if d.f != nil {
		d.f.Close()
		d.f = nil
	}
	if err := os.MkdirAll(d.dir, 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d.dir, d.prefix+"-"+day+d.ext), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	d.f, d.day, d.size, d.full = f, day, info.Size(), false
	if d.size == 0 && d.header != "" {
		n, err := f.WriteString(d.header + "\n")
		d.size += int64(n)
		if err != nil {
			return err
		}
	}
	d.prune(day)
	return nil
}

// prune deletes this prefix's files dated more than keepDays before today.
func (d *dailyFile) prune(day string) {
	today := max(day, d.now().UTC().Format(dayFormat))
	t, _ := time.Parse(dayFormat, today)
	oldest := t.AddDate(0, 0, -d.keepDays)
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		date, ok := strings.CutPrefix(e.Name(), d.prefix+"-")
		if !ok {
			continue
		}
		date, ok = strings.CutSuffix(date, d.ext)
		if !ok {
			continue
		}
		day, err := time.Parse(dayFormat, date)
		if err == nil && day.Before(oldest) {
			os.Remove(filepath.Join(d.dir, e.Name()))
		}
	}
}

// path is the file of the current day, for messages.
func (d *dailyFile) path() string {
	day := d.day
	if day == "" {
		day = d.now().UTC().Format(dayFormat)
	}
	return filepath.Join(d.dir, d.prefix+"-"+day+d.ext)
}

// Close closes the current file; a later write opens it again.
func (d *dailyFile) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f != nil {
		d.f.Close()
		d.f = nil
	}
}
