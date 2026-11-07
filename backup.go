package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"filippo.io/age"
)

// Wait after a failed backup before the next attempt, doubling per failure.
const (
	backupBackoff    = 3 * time.Minute
	backupBackoffMax = time.Hour
)

// backupTimeFormat is the UTC time in an object name; it has no ':', which
// some tools and file systems do not accept.
const backupTimeFormat = "2006-01-02T15-04-05Z"

var (
	backupPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)
	bucketPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// validateBackup checks the backup settings. Errors never hold the secret
// access key.
func validateBackup(b *BackupConfig) error {
	if len(b.Recipients) == 0 {
		return fmt.Errorf("recipients must hold at least one age public key (age1...)")
	}
	for i, r := range b.Recipients {
		if _, err := age.ParseX25519Recipient(r); err != nil {
			return fmt.Errorf("recipients[%d] is not an age X25519 public key: %v", i, err)
		}
	}
	if b.Prefix != "" && !backupPrefixPattern.MatchString(b.Prefix) {
		return fmt.Errorf("prefix must be path segments of letters, digits, '.', '_' and '-' separated by '/', got %q", b.Prefix)
	}
	t := b.Target
	switch t.Kind {
	case "dir":
		if !filepath.IsAbs(t.Path) {
			return fmt.Errorf("target.path must be an absolute path, got %q", t.Path)
		}
	case "s3":
		if _, err := parseEndpoint(t.Endpoint); err != nil {
			return fmt.Errorf("target.endpoint %w", err)
		}
		if t.Region == "" {
			return fmt.Errorf("target.region is required, \"auto\" for Cloudflare R2")
		}
		if !bucketPattern.MatchString(t.Bucket) {
			return fmt.Errorf("target.bucket must be a bucket name of 3 to 63 lowercase letters, digits, '.' and '-', got %q", t.Bucket)
		}
		if t.AccessKeyID == "" {
			return fmt.Errorf("target.accessKeyId is required")
		}
		if t.SecretAccessKey == "" {
			return fmt.Errorf("target.secretAccessKey is required")
		}
	case "":
		return fmt.Errorf("target.kind is required: \"dir\" or \"s3\"")
	default:
		return fmt.Errorf("target.kind must be \"dir\" or \"s3\", got %q", t.Kind)
	}
	return nil
}

// manifestEntry is a file of a backup.
type manifestEntry struct {
	// Name is the path in the archive, Path where the file was on the host.
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Mode is the file's permission bits, e.g. "0600".
	Mode string `json:"mode"`
}

// manifest is manifest.json, the first file of a backup.
type manifest struct {
	RunnerVersion string          `json:"runnerVersion"`
	Hostname      string          `json:"hostname"`
	Time          time.Time       `json:"time"`
	Entries       []manifestEntry `json:"entries"`
}

// backupFile is a file to put in an archive.
type backupFile struct {
	entry   manifestEntry
	modTime time.Time
	data    []byte
}

// appArchiveDir is the directory of an app's workDir in an archive.
func appArchiveDir(app App) string {
	return fmt.Sprintf("apps/%d-%s", app.ExternalPort, strings.ReplaceAll(app.BinName, "/", "_"))
}

// buildArchive builds an encrypted backup in memory: a tar of manifest.json,
// runner/config.json (cfgBytes, from cfgPath) and every app's workDir
// without its workDirIgnore files, gzipped and encrypted to recipients with
// age. Nothing is written to disk. Any file that cannot be read fails it.
func buildArchive(cfgBytes []byte, cfgPath string, apps []App, recipients []age.Recipient, now time.Time) ([]byte, manifest, error) {
	hostname, _ := os.Hostname()
	m := manifest{RunnerVersion: version, Hostname: hostname, Time: now.UTC(), Entries: []manifestEntry{}}

	sum := sha256.Sum256(cfgBytes)
	files := []backupFile{{
		entry: manifestEntry{
			Name: "runner/config.json", Path: cfgPath, SHA256: hex.EncodeToString(sum[:]),
			Size: int64(len(cfgBytes)), Mode: "0600",
		},
		modTime: now,
		data:    cfgBytes,
	}}
	if info, err := os.Stat(cfgPath); err == nil {
		files[0].entry.Mode = fmt.Sprintf("%04o", info.Mode().Perm())
		files[0].modTime = info.ModTime()
	}
	for _, app := range apps {
		if app.WorkDir == "" {
			continue
		}
		entries, err := listWorkDir(app.WorkDir, app.WorkDirIgnore)
		if err != nil {
			return nil, m, fmt.Errorf("workDir of %s: %w", app.Repo, err)
		}
		for _, e := range entries {
			path := filepath.Join(app.WorkDir, filepath.FromSlash(e.rel))
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, m, err
			}
			sum := sha256.Sum256(data)
			files = append(files, backupFile{
				entry: manifestEntry{
					Name: appArchiveDir(app) + "/" + e.rel, Path: path, SHA256: hex.EncodeToString(sum[:]),
					Size: int64(len(data)), Mode: fmt.Sprintf("%04o", e.info.Mode().Perm()),
				},
				modTime: e.info.ModTime(),
				data:    data,
			})
		}
	}
	for _, f := range files {
		m.Entries = append(m.Entries, f.entry)
	}
	manifestJSON, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, m, err
	}

	var out bytes.Buffer
	enc, err := age.Encrypt(&out, recipients...)
	if err != nil {
		return nil, m, err
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	add := func(name string, mode int64, modTime time.Time, data []byte) error {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data)), ModTime: modTime, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add("manifest.json", 0o600, now, manifestJSON); err != nil {
		return nil, m, err
	}
	for _, f := range files {
		mode, _ := strconv.ParseInt(f.entry.Mode, 8, 64)
		if err := add(f.entry.Name, mode, f.modTime, f.data); err != nil {
			return nil, m, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, m, err
	}
	if err := gz.Close(); err != nil {
		return nil, m, err
	}
	if err := enc.Close(); err != nil {
		return nil, m, err
	}
	return out.Bytes(), m, nil
}

// backupObjectName is the name of a backup made at t.
func backupObjectName(prefix string, t time.Time) string {
	name := t.UTC().Format(backupTimeFormat) + ".tar.gz.age"
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// backupStatus is what the admin UI shows of the backups.
type backupStatus struct {
	LastTime   time.Time `json:"lastTime,omitzero"`
	LastSize   int64     `json:"lastSize"`
	LastObject string    `json:"lastObject"`
	LastError  string    `json:"lastError"`
}

// backupState is dataDir/backup-state.json: the last successful backup.
type backupState struct {
	Time   time.Time `json:"time"`
	Hash   string    `json:"hash"`
	Object string    `json:"object"`
	Size   int64     `json:"size"`
}

// backupJob is everything one backup needs, taken from the reload loop.
type backupJob struct {
	hash       string
	cfgBytes   []byte
	cfgPath    string
	apps       []App
	config     BackupConfig
	clients    *Clients
	recipients []age.Recipient
	// parts and reason tell what changed since the last successful backup
	parts  map[string]backupPart
	reason string
}

// backupRunner runs one backup at a time, in its own goroutine, and keeps
// what the admin UI shows. The reload loop calls trigger every tick.
type backupRunner struct {
	running atomic.Bool
	// now is time.Now, replaced by tests
	now func() time.Time
	// stateFile is dataDir/backup-state.json, empty without dataDir
	stateFile string
	// wg tracks the running backup, for tests
	wg sync.WaitGroup
	// history is every attempt, for the admin UI's Backups tab
	history *backupHistory

	mu      sync.Mutex
	enabled bool
	// target and recipients describe the current settings for the admin UI
	target     string
	recipients int
	status     backupStatus
	// lastParts are the parts of the last successful backup since runner
	// started, nil before it
	lastParts map[string]backupPart
	// lastHash is the content of the last successful backup
	lastHash string
	// failures counts consecutive failed backups; next is when the next
	// attempt may start after one
	failures int
	next     time.Time
}

// newBackupRunner loads the last successful backup from dataDir, if set.
func newBackupRunner(dataDir string) *backupRunner {
	b := &backupRunner{now: time.Now, history: loadBackupHistory("")}
	if dataDir == "" {
		return b
	}
	b.history = loadBackupHistory(filepath.Join(dataDir, "backups.jsonl"))
	b.stateFile = filepath.Join(dataDir, "backup-state.json")
	data, err := os.ReadFile(b.stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			logf("[MAIN] Failed to read %s, backing up on the first check: %v\n", b.stateFile, err)
		}
		return b
	}
	var st backupState
	if err := json.Unmarshal(data, &st); err != nil {
		logf("[MAIN] Failed to read %s, backing up on the first check: %v\n", b.stateFile, err)
		return b
	}
	b.lastHash = st.Hash
	b.status = backupStatus{LastTime: st.Time, LastSize: st.Size, LastObject: st.Object}
	return b
}

// snapshot returns the status for the admin UI, nil when backups are off.
func (b *backupRunner) snapshot() *backupStatus {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.enabled {
		return nil
	}
	s := b.status
	return &s
}

// setConfig records the backup settings, nil when backups are off.
func (b *backupRunner) setConfig(config *BackupConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.enabled = config != nil
	if config != nil {
		b.target = backupTargetName(config)
		b.recipients = len(config.Recipients)
	}
}

// backupList is the answer of GET /backups.
type backupList struct {
	// Target is "s3 <bucket>/<prefix>" or "dir <path>".
	Target     string         `json:"target"`
	Recipients int            `json:"recipients"`
	Entries    []backupRecord `json:"entries"`
}

// list returns the backup attempts newest first, nil when backups are off.
func (b *backupRunner) list() *backupList {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	enabled, target, recipients := b.enabled, b.target, b.recipients
	b.mu.Unlock()
	if !enabled {
		return nil
	}
	return &backupList{Target: target, Recipients: recipients, Entries: b.history.newestFirst()}
}

// trigger starts a backup in the background when hash differs from the last
// successful backup, none is running, and no failure asks to wait. It returns
// whether it started one. job is only built when needed.
func (b *backupRunner) trigger(hash string, job func() (backupJob, error)) bool {
	b.mu.Lock()
	due := hash != b.lastHash && !b.now().Before(b.next)
	b.mu.Unlock()
	if !due || !b.running.CompareAndSwap(false, true) {
		return false
	}
	j, err := job()
	if err != nil {
		b.done(j, backupResult{}, err)
		b.running.Store(false)
		return true
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.running.Store(false)
		res, err := runBackup(j, b.now())
		b.done(j, res, err)
	}()
	return true
}

// backupResult is what a backup wrote.
type backupResult struct {
	object string
	size   int64
	files  int
}

// runBackup builds the archive of j and writes it to its target.
func runBackup(j backupJob, now time.Time) (backupResult, error) {
	data, m, err := buildArchive(j.cfgBytes, j.cfgPath, j.apps, j.recipients, now)
	res := backupResult{object: backupObjectName(j.config.Prefix, now), files: len(m.Entries)}
	if err != nil {
		return res, err
	}
	res.size = int64(len(data))
	target, err := newBackupTarget(j.config.Target, j.clients)
	if err != nil {
		return res, err
	}
	ctx, cancel := contextWithTimeout(j.clients)
	defer cancel()
	if err := target.Put(ctx, res.object, data); err != nil {
		return res, err
	}
	return res, nil
}

// done records the result of a backup in the status and the history. A
// failure is logged once per streak of failures, and delays the next
// attempt.
func (b *backupRunner) done(j backupJob, res backupResult, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	object, size := res.object, res.size
	rec := backupRecord{Time: now.UTC(), Result: "ok", Object: object, Size: size, Files: res.files, Reason: j.reason}
	if err != nil {
		msg := redactSecrets(err.Error(), j.config)
		rec.Result, rec.Error = "failed", msg
		b.history.add(rec)
		wait := restartDelay(b.failures, backupBackoff, backupBackoffMax)
		if b.failures == 0 {
			logf("[MAIN] Backup failed, retrying in %s and up to every %s until it succeeds: %s\n", wait, backupBackoffMax, msg)
		}
		b.failures++
		b.next = now.Add(wait)
		b.status.LastError = msg
		return
	}
	b.history.add(rec)
	b.lastParts = j.parts
	if b.failures > 0 {
		logf("[MAIN] Backup succeeded again after %d failed attempts\n", b.failures)
	}
	b.failures = 0
	b.next = time.Time{}
	b.lastHash = j.hash
	b.status = backupStatus{LastTime: now, LastSize: size, LastObject: object}
	logf("[MAIN] Backup written to %s (%s)\n", object, formatBytes(size))
	if b.stateFile != "" {
		st := backupState{Time: now, Hash: j.hash, Object: object, Size: size}
		if err := writeFileAtomic(b.stateFile, st); err != nil {
			logf("[MAIN] Failed to write %s, the next start backs up again: %v\n", b.stateFile, err)
		}
	}
}

// writeFileAtomic writes v as JSON to name through a rename.
func writeFileAtomic(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// formatBytes formats a size such as "12.3 KB".
func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// backupHash identifies what a backup would hold now: config.json and the
// content of every app's workDir as last read by the reload loop.
func (r *appRunner) backupHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "config.json\x00%x\n", sha256.Sum256(r.configFile))
	for _, app := range r.config.Apps {
		if st := r.workDirs[app.ExternalPort]; app.WorkDir != "" && st != nil {
			fmt.Fprintf(h, "%d\x00%s\x00%s\n", app.ExternalPort, app.WorkDir, st.hash)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// checkBackup starts a backup when config.json or a workDir changed since
// the last successful one. It runs at start and on every reload tick.
func (r *appRunner) checkBackup() {
	b := r.config.Backup
	r.backup.setConfig(b)
	if b == nil {
		return
	}
	r.backup.trigger(r.backupHash(), func() (backupJob, error) {
		parts := r.backupParts()
		r.backup.mu.Lock()
		reason := backupReason(r.backup.lastParts, parts)
		r.backup.mu.Unlock()
		j := backupJob{
			hash:     r.backupHash(),
			cfgBytes: r.configFile,
			apps:     append([]App(nil), r.config.Apps...),
			config:   *b,
			clients:  r.clients,
			parts:    parts,
			reason:   reason,
		}
		cwd, err := os.Getwd()
		if err != nil {
			return j, err
		}
		j.cfgPath = filepath.Join(cwd, "config.json")
		for _, s := range b.Recipients {
			rec, err := age.ParseX25519Recipient(s)
			if err != nil {
				return j, err
			}
			j.recipients = append(j.recipients, rec)
		}
		return j, nil
	})
}
