package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lineCount counts the lines of a file.
func lineCount(t *testing.T, name string) int {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func TestBackupHistoryAppendLoadTrim(t *testing.T) {
	file := filepath.Join(t.TempDir(), "backups.jsonl")
	h := loadBackupHistory(file)
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	add := func(i int) {
		h.add(backupRecord{Time: at.Add(time.Duration(i) * time.Minute), Result: "ok", Object: fmt.Sprintf("vm-01/%d.tar.gz.age", i), Reason: "config.json"})
	}
	for i := range backupHistoryRewrite {
		add(i)
	}
	if n := lineCount(t, file); n != backupHistoryRewrite {
		t.Errorf("%d lines after %d attempts, want them all", n, backupHistoryRewrite)
	}
	if n := len(h.newestFirst()); n != maxBackupRecords {
		t.Errorf("%d records in memory, want %d", n, maxBackupRecords)
	}
	// one more rewrites the file with the last 500
	add(backupHistoryRewrite)
	if n := lineCount(t, file); n != maxBackupRecords {
		t.Errorf("%d lines after the rewrite, want %d", n, maxBackupRecords)
	}
	for i := backupHistoryRewrite + 1; i < 650; i++ {
		add(i)
	}

	// a line that cannot be read is skipped on load
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n")
	f.Close()
	loaded := loadBackupHistory(file).newestFirst()
	if len(loaded) != maxBackupRecords || loaded[0].Object != "vm-01/649.tar.gz.age" ||
		loaded[len(loaded)-1].Object != "vm-01/150.tar.gz.age" {
		t.Errorf("loaded %d records from %s to %s, want 500 from 649 down to 150",
			len(loaded), loaded[0].Object, loaded[len(loaded)-1].Object)
	}

	// without dataDir only memory, capped too
	mem := loadBackupHistory("")
	for i := range 510 {
		mem.add(backupRecord{Object: fmt.Sprint(i)})
	}
	if got := mem.newestFirst(); len(got) != maxBackupRecords || got[0].Object != "509" {
		t.Errorf("in-memory history has %d records, newest %q", len(got), got[0].Object)
	}
}

func TestBackupHistoryRecordsReasons(t *testing.T) {
	captureLog(t)
	dataDir := t.TempDir()
	c, _, clock, _ := newBackupTestRunner(t, dataDir)
	backupNow := func() {
		clock.advance(time.Second)
		c.checkBackup()
		c.backup.wg.Wait()
	}
	backupNow()
	writeFile(t, c.app.WorkDir, "appsettings.json", `{"Db": "Host=db2"}`)
	c.checkWorkDirs()
	backupNow()
	c.configFile = []byte(`{"apps": [], "pollInterval": "1m"}`)
	backupNow()

	list := c.backup.list()
	if list == nil || list.Target != "dir "+c.config.Backup.Target.Path || list.Recipients != 1 {
		t.Fatalf("list %+v", list)
	}
	var reasons []string
	for _, e := range list.Entries {
		reasons = append(reasons, e.Reason)
		if e.Result != "ok" || e.Size == 0 || e.Files != 3 || !strings.HasPrefix(e.Object, "vm-01/") || e.Error != "" {
			t.Errorf("entry %+v", e)
		}
	}
	want := []string{"config.json", "workDir orders-api: appsettings.json", "start"}
	if strings.Join(reasons, "|") != strings.Join(want, "|") {
		t.Errorf("reasons newest first %q, want %q", reasons, want)
	}
	// kept in dataDir for the next start
	if got := loadBackupHistory(filepath.Join(dataDir, "backups.jsonl")).newestFirst(); len(got) != 3 || got[0].Reason != "config.json" {
		t.Errorf("backups.jsonl holds %+v", got)
	}
}

func TestBackupHistoryRecordsFailureWithoutSecrets(t *testing.T) {
	logs := captureLog(t)
	// a storage that echoes the credentials in its error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<Error>bad key EXAMPLEKEYID with secret top-secret-value</Error>")
	}))
	defer srv.Close()
	c, _, clock, _ := newBackupTestRunner(t, t.TempDir())
	c.config.Backup.Target = BackupTarget{Kind: "s3", Endpoint: srv.URL, Region: "auto", Bucket: "backups",
		AccessKeyID: "EXAMPLEKEYID", SecretAccessKey: "top-secret-value"}
	clock.advance(time.Second)
	c.checkBackup()
	c.backup.wg.Wait()

	list := c.backup.list()
	if list.Target != "s3 backups/vm-01" || len(list.Entries) != 1 {
		t.Fatalf("list %+v", list)
	}
	e := list.Entries[0]
	if e.Result != "failed" || !strings.Contains(e.Error, "403") || e.Reason != "start" || e.Files != 3 {
		t.Errorf("failed entry %+v", e)
	}
	data, _ := json.Marshal(list)
	for _, text := range []string{string(data), c.backup.snapshot().LastError, logs.String()} {
		if strings.Contains(text, "top-secret-value") || strings.Contains(text, "EXAMPLEKEYID") {
			t.Errorf("credentials recorded: %s", text)
		}
	}
}

func TestAdminBackupsEndpoint(t *testing.T) {
	a := newTestAdmin()
	if rec := get(t, a, "/backups"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /backups without backups answered %d, want 404", rec.Code)
	}
	a.backup = newBackupRunner("")
	a.backup.setConfig(&BackupConfig{Recipients: []string{"age1a", "age1b"}, Prefix: "vm-01",
		Target: BackupTarget{Kind: "s3", Bucket: "backups", SecretAccessKey: "top-secret-value"}})
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	a.backup.history.add(backupRecord{Time: at, Result: "ok", Object: "vm-01/a.tar.gz.age", Size: 10, Files: 2, Reason: "start"})
	a.backup.history.add(backupRecord{Time: at.Add(time.Minute), Result: "failed", Object: "vm-01/b.tar.gz.age", Files: 2, Reason: "config.json", Error: "403"})

	// same auth as the other endpoints
	req := httptest.NewRequest("GET", "/backups", nil)
	rec := httptest.NewRecorder()
	a.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /backups without credentials answered %d, want 401", rec.Code)
	}

	rec = get(t, a, "/backups")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET /backups answered %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if strings.Contains(rec.Body.String(), "top-secret-value") {
		t.Error("/backups holds the secret")
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["target"] != "s3 backups/vm-01" || got["recipients"] != float64(2) {
		t.Errorf("target %v recipients %v", got["target"], got["recipients"])
	}
	entries, _ := got["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries %v", got["entries"])
	}
	newest := entries[0].(map[string]any)
	for _, key := range []string{"time", "result", "object", "size", "files", "reason", "error"} {
		if _, ok := newest[key]; !ok {
			t.Errorf("entry has no %q: %v", key, newest)
		}
	}
	if newest["result"] != "failed" || newest["error"] != "403" || entries[1].(map[string]any)["reason"] != "start" {
		t.Errorf("entries not newest first: %v", entries)
	}
}
