package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// History of backups: runner's own record of the backups it attempted. It
// never lists the target, whose credentials may be write-only.
const (
	// maxBackupRecords is how many attempts are kept
	maxBackupRecords = 500
	// backupHistoryRewrite is how many lines backups.jsonl may grow to before
	// it is rewritten with the last maxBackupRecords
	backupHistoryRewrite = 600
)

// backupRecord is one backup attempt, a line of dataDir/backups.jsonl.
type backupRecord struct {
	Time time.Time `json:"time"`
	// Result is "ok" or "failed", with the error in Error.
	Result string `json:"result"`
	Object string `json:"object"`
	Size   int64  `json:"size"`
	Files  int    `json:"files"`
	// Reason is what changed: "start", "config.json" or
	// "workDir <binName>: <files>", separated by "; ".
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"`
}

// backupHistory keeps the last maxBackupRecords attempts in memory and, with
// dataDir, in backups.jsonl. Safe for concurrent use.
type backupHistory struct {
	mu sync.Mutex
	// records are oldest first
	records []backupRecord
	// file is dataDir/backups.jsonl, empty without dataDir; lines counts
	// the lines in it
	file  string
	lines int
}

// loadBackupHistory reads dataDir/backups.jsonl, if dataDir is set. Lines
// that cannot be read are skipped.
func loadBackupHistory(file string) *backupHistory {
	h := &backupHistory{file: file}
	if file == "" {
		return h
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if !os.IsNotExist(err) {
			logf("[MAIN] Failed to read %s, the backup history starts over: %v\n", file, err)
		}
		return h
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		h.lines++
		var rec backupRecord
		if json.Unmarshal(scanner.Bytes(), &rec) == nil {
			h.records = append(h.records, rec)
		}
	}
	h.records = lastRecords(h.records)
	return h
}

// lastRecords returns the last maxBackupRecords of records.
func lastRecords(records []backupRecord) []backupRecord {
	if len(records) > maxBackupRecords {
		return slices.Clone(records[len(records)-maxBackupRecords:])
	}
	return records
}

// add records an attempt, appending it to the file, which is rewritten once
// it has backupHistoryRewrite lines. A file that cannot be written is
// logged; the record is kept in memory.
func (h *backupHistory) add(rec backupRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = lastRecords(append(h.records, rec))
	if h.file == "" {
		return
	}
	if err := h.write(rec); err != nil {
		logf("[MAIN] Failed to write %s: %v\n", h.file, err)
	}
}

func (h *backupHistory) write(rec backupRecord) error {
	if h.lines+1 > backupHistoryRewrite {
		var b strings.Builder
		for _, r := range h.records {
			line, _ := json.Marshal(r)
			b.Write(line)
			b.WriteByte('\n')
		}
		tmp := h.file + ".tmp"
		if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, h.file); err != nil {
			return err
		}
		h.lines = len(h.records)
		return nil
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(h.file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	h.lines++
	return nil
}

// newestFirst returns a copy of the records, newest first.
func (h *backupHistory) newestFirst() []backupRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]backupRecord, len(h.records))
	for i, r := range h.records {
		out[len(out)-1-i] = r
	}
	return out
}

// backupPart is one thing a backup holds, to tell what changed between two
// backups: config.json, or an app's workDir with its files.
type backupPart struct {
	label string
	hash  string
	files map[string]string
}

// backupParts returns the parts a backup would hold now, keyed by
// "config.json" or "workDir <externalPort>".
func (r *appRunner) backupParts() map[string]backupPart {
	parts := map[string]backupPart{
		"config.json": {label: "config.json", hash: contentKey(r.configFile)},
	}
	for _, app := range r.config.Apps {
		if st := r.workDirs[app.ExternalPort]; app.WorkDir != "" && st != nil {
			parts["workDir "+strconv.Itoa(app.ExternalPort)] = backupPart{
				label: "workDir " + app.BinName, hash: app.WorkDir + "\x00" + st.hash, files: st.files,
			}
		}
	}
	return parts
}

// backupReason describes what changed from the parts of the last successful
// backup to now; "start" when there was none since runner started.
func backupReason(last, now map[string]backupPart) string {
	if last == nil {
		return "start"
	}
	var reasons []string
	keys := make([]string, 0, len(now))
	for key := range now {
		keys = append(keys, key)
	}
	// config.json first, then the apps
	slices.SortFunc(keys, func(a, b string) int {
		if a == "config.json" {
			return -1
		}
		if b == "config.json" {
			return 1
		}
		return strings.Compare(a, b)
	})
	for _, key := range keys {
		p, was := now[key], last[key]
		if p.hash == was.hash {
			continue
		}
		if key == "config.json" {
			reasons = append(reasons, "config.json")
			continue
		}
		if names := describeFiles(changedFiles(was.files, p.files)); names != "" {
			reasons = append(reasons, p.label+": "+names)
		} else {
			reasons = append(reasons, p.label)
		}
	}
	for key, was := range last {
		if _, ok := now[key]; !ok && key != "config.json" {
			reasons = append(reasons, was.label+" removed")
		}
	}
	if len(reasons) == 0 {
		return "unchanged"
	}
	return strings.Join(reasons, "; ")
}

// backupTargetName describes a target for the admin UI, without secrets:
// "s3 <bucket>/<prefix>" or "dir <path>".
func backupTargetName(b *BackupConfig) string {
	if b.Target.Kind == "s3" {
		if b.Prefix == "" {
			return "s3 " + b.Target.Bucket
		}
		return "s3 " + b.Target.Bucket + "/" + b.Prefix
	}
	return "dir " + b.Target.Path
}

// redactSecrets removes the credentials of b from an error text, in case a
// storage echoes them back.
func redactSecrets(text string, b BackupConfig) string {
	for _, secret := range []string{b.Target.SecretAccessKey, b.Target.AccessKeyID} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return text
}
