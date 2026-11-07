package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

// testIdentity is an age key pair for a test.
func testIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// openArchive decrypts, gunzips and untars a backup into a map of name to
// content.
func openArchive(t *testing.T, data []byte, id age.Identity) map[string]string {
	t.Helper()
	r, err := age.Decrypt(bytes.NewReader(data), id)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[hdr.Name] = string(b)
	}
	return files
}

func TestValidateBackup(t *testing.T) {
	recipient := testIdentity(t).Recipient().String()
	valid := []*BackupConfig{
		{Recipients: []string{recipient}, Target: BackupTarget{Kind: "dir", Path: "/var/backups/runner"}},
		{Recipients: []string{recipient}, Prefix: "vm-01/runner", Target: BackupTarget{
			Kind: "s3", Endpoint: "https://account.r2.cloudflarestorage.com", Region: "auto", Bucket: "backups",
			AccessKeyID: "key", SecretAccessKey: "top-secret-value",
		}},
	}
	for _, b := range valid {
		if err := validateBackup(b); err != nil {
			t.Errorf("valid backup %+v rejected: %v", b.Target.Kind, err)
		}
	}

	s3 := func(change func(b *BackupConfig)) *BackupConfig {
		b := *valid[1]
		b.Recipients = []string{recipient}
		change(&b)
		return &b
	}
	for name, tc := range map[string]struct {
		b    *BackupConfig
		want string
	}{
		"no recipients": {s3(func(b *BackupConfig) { b.Recipients = nil }), "recipients must hold"},
		"bad recipient": {s3(func(b *BackupConfig) { b.Recipients = []string{recipient, "ssh-ed25519 AAAA"} }), "recipients[1]"},
		"prefix slash":  {s3(func(b *BackupConfig) { b.Prefix = "/vm-01" }), "prefix must be"},
		"prefix dots":   {s3(func(b *BackupConfig) { b.Prefix = "vm-01/../x" }), "prefix must be"},
		"no kind":       {s3(func(b *BackupConfig) { b.Target.Kind = "" }), "target.kind is required"},
		"other kind":    {s3(func(b *BackupConfig) { b.Target.Kind = "gcs" }), "target.kind must be"},
		"relative dir":  {s3(func(b *BackupConfig) { b.Target = BackupTarget{Kind: "dir", Path: "backups"} }), "target.path must be an absolute path"},
		"bad endpoint":  {s3(func(b *BackupConfig) { b.Target.Endpoint = "account.r2.cloudflarestorage.com" }), "target.endpoint must be a URL"},
		"no region":     {s3(func(b *BackupConfig) { b.Target.Region = "" }), "target.region is required"},
		"bad bucket":    {s3(func(b *BackupConfig) { b.Target.Bucket = "My_Bucket" }), "target.bucket must be"},
		"no access key": {s3(func(b *BackupConfig) { b.Target.AccessKeyID = "" }), "target.accessKeyId is required"},
		"no secret":     {s3(func(b *BackupConfig) { b.Target.SecretAccessKey = "" }), "target.secretAccessKey is required"},
	} {
		err := validateBackup(tc.b)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "top-secret-value") {
			t.Errorf("%s: error %q contains the secret", name, err)
		}
	}

	// config.json names the field
	config := testConfig()
	config.Backup = s3(func(b *BackupConfig) { b.Target.Region = "" })
	if err := validateConfig(config); err == nil || !strings.HasPrefix(err.Error(), "backup: target.region") {
		t.Errorf("validateConfig error %v, want one starting with backup: target.region", err)
	}
}

// backupApp is an app with a workDir holding appsettings.json, a secret
// and an ignored cache.
func backupApp(t *testing.T) App {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "appsettings.json", `{"Db": "Host=db"}`)
	writeFile(t, dir, "secrets/key.txt", "plaintext-marker-5d1e")
	writeFile(t, dir, "cache/blob", "cached")
	os.Chmod(filepath.Join(dir, "secrets/key.txt"), 0o600)
	return App{ExternalPort: 8000, Repo: "owner/orders-api", BinName: "orders-api", WorkDir: dir, WorkDirIgnore: []string{"cache/"}}
}

func TestBuildArchiveRoundTrip(t *testing.T) {
	id := testIdentity(t)
	app := backupApp(t)
	cfg := []byte(`{"apps": []}`)
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	data, m, err := buildArchive(cfg, "/srv/runner/config.json", []App{app, {ExternalPort: 8001, BinName: "web"}}, []age.Recipient{id.Recipient()}, now)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("plaintext-marker")) {
		t.Error("archive holds plaintext")
	}

	files := openArchive(t, data, id)
	want := map[string]string{
		"runner/config.json":                    string(cfg),
		"apps/8000-orders-api/appsettings.json": `{"Db": "Host=db"}`,
		"apps/8000-orders-api/secrets/key.txt":  "plaintext-marker-5d1e",
	}
	for name, content := range want {
		if files[name] != content {
			t.Errorf("%s = %q, want %q", name, files[name], content)
		}
	}
	if _, ok := files["apps/8000-orders-api/cache/blob"]; ok {
		t.Error("ignored file is in the archive")
	}
	if len(files) != len(want)+1 {
		t.Errorf("archive has %d files, want %d: %v", len(files), len(want)+1, files)
	}

	var got manifest
	if err := json.Unmarshal([]byte(files["manifest.json"]), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Time.Equal(now) || got.RunnerVersion != version || len(got.Entries) != 3 || len(m.Entries) != 3 {
		t.Fatalf("manifest %+v", got)
	}
	for _, e := range got.Entries {
		sum := sha256.Sum256([]byte(files[e.Name]))
		if e.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: manifest hash does not match the content", e.Name)
		}
		if e.Name == "apps/8000-orders-api/secrets/key.txt" &&
			(e.Path != filepath.Join(app.WorkDir, "secrets", "key.txt") || e.Mode != "0600") {
			t.Errorf("manifest entry %+v, want the original path and mode 0600", e)
		}
	}
	if got.Entries[0].Path != "/srv/runner/config.json" {
		t.Errorf("config.json path %q", got.Entries[0].Path)
	}

	// another key cannot decrypt it
	if _, err := age.Decrypt(bytes.NewReader(data), testIdentity(t)); err == nil {
		t.Error("archive decrypted with another key")
	}
}

func TestBuildArchiveFailsOnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	app := backupApp(t)
	os.Chmod(filepath.Join(app.WorkDir, "secrets/key.txt"), 0)
	_, _, err := buildArchive([]byte("{}"), "/x/config.json", []App{app}, []age.Recipient{testIdentity(t).Recipient()}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "key.txt") {
		t.Errorf("unreadable file: error %v, want one naming key.txt", err)
	}
}

func TestBackupObjectName(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 4, 5, 0, time.FixedZone("x", 3600))
	if got := backupObjectName("vm-01", at); got != "vm-01/2026-11-07T09-04-05Z.tar.gz.age" {
		t.Errorf("object name %q", got)
	}
	if got := backupObjectName("", at); got != "2026-11-07T09-04-05Z.tar.gz.age" {
		t.Errorf("object name without prefix %q", got)
	}
}

func TestSigV4Vectors(t *testing.T) {
	emptyHash := hex.EncodeToString(func() []byte { s := sha256.Sum256(nil); return s[:] }())
	for _, tc := range []struct {
		name, method, url, region, service, key, secret, payloadHash string
		at                                                           time.Time
		headers                                                      map[string]string
		want                                                         string
	}{
		{
			// AWS SigV4 test suite: get-vanilla
			name: "get-vanilla", method: "GET", url: "https://example.amazonaws.com/",
			region: "us-east-1", service: "service", key: "AKIDEXAMPLE", secret: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
			payloadHash: emptyHash, at: time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC),
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			// Amazon S3 API reference, signature calculation example: GET Object
			name: "s3-get-object", method: "GET", url: "https://examplebucket.s3.amazonaws.com/test.txt",
			region: "us-east-1", service: "s3", key: "AKIAIOSFODNN7EXAMPLE", secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			payloadHash: emptyHash, at: time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC),
			headers: map[string]string{"Range": "bytes=0-9", "X-Amz-Content-Sha256": emptyHash},
			want:    "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			// Amazon S3 API reference, signature calculation example: PUT Object
			name: "s3-put-object", method: "PUT", url: "https://examplebucket.s3.amazonaws.com/test$file.text",
			region: "us-east-1", service: "s3", key: "AKIAIOSFODNN7EXAMPLE", secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			payloadHash: "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072", at: time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC),
			headers: map[string]string{
				"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY",
				"X-Amz-Content-Sha256": "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			},
			want: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
	} {
		req, err := http.NewRequest(tc.method, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		signV4(req, tc.payloadHash, tc.key, tc.secret, tc.region, tc.service, tc.at)
		if got := req.Header.Get("Authorization"); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestS3TargetPut(t *testing.T) {
	body := []byte("encrypted-bytes")
	var got *http.Request
	var gotBody []byte
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		io.WriteString(w, "<Error>denied</Error>")
	}))
	defer srv.Close()

	config := testConfig()
	target, err := newBackupTarget(BackupTarget{
		Kind: "s3", Endpoint: srv.URL, Region: "auto", Bucket: "backups", AccessKeyID: "key-id", SecretAccessKey: "secret",
	}, newClients(config))
	if err != nil {
		t.Fatal(err)
	}
	target.(*s3Target).now = func() time.Time { return time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC) }
	if err := target.Put(context.Background(), "vm-01/2026-11-07T10-00-00Z.tar.gz.age", body); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if got.Method != http.MethodPut || got.URL.Path != "/backups/vm-01/2026-11-07T10-00-00Z.tar.gz.age" {
		t.Errorf("request %s %s, want PUT /backups/vm-01/...", got.Method, got.URL.Path)
	}
	if got.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) || got.Header.Get("X-Amz-Date") != "20261107T100000Z" {
		t.Errorf("headers %v", got.Header)
	}
	auth := got.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=key-id/20261107/auto/s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=") {
		t.Errorf("authorization %q", auth)
	}
	if !bytes.Equal(gotBody, body) || got.ContentLength != int64(len(body)) {
		t.Errorf("body %q length %d", gotBody, got.ContentLength)
	}

	status = http.StatusForbidden
	err = target.Put(context.Background(), "x.tar.gz.age", body)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "denied") {
		t.Errorf("403: error %v", err)
	}
}

// newBackupTestRunner returns a runner with the app of backupApp and backups to a
// dir target, its target dir and its clock.
func newBackupTestRunner(t *testing.T, dataDir string) (*workDirRunner, string, *fakeClock, *age.X25519Identity) {
	t.Helper()
	app := backupApp(t)
	c := newWorkDirRunner(t, app.WorkDir, app.WorkDirIgnore)
	id := testIdentity(t)
	target := filepath.Join(t.TempDir(), "backups")
	c.config.Backup = &BackupConfig{Recipients: []string{id.Recipient().String()}, Prefix: "vm-01", Target: BackupTarget{Kind: "dir", Path: target}}
	c.configFile = []byte(`{"apps": []}`)
	c.backup = newBackupRunner(dataDir)
	clock := &fakeClock{t: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)}
	c.backup.now = clock.now
	return c, target, clock, id
}

// objects lists the files under dir, relative to it.
func objects(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	return names
}

func TestBackupOncePerChange(t *testing.T) {
	logs := captureLog(t)
	dataDir := t.TempDir()
	c, target, clock, id := newBackupTestRunner(t, dataDir)
	backupNow := func() {
		clock.advance(time.Second)
		c.checkBackup()
		c.backup.wg.Wait()
	}

	// at start, then not again while nothing changes
	for range 3 {
		backupNow()
	}
	got := objects(t, target)
	if len(got) != 1 || got[0] != "vm-01/2026-11-07T10-00-01Z.tar.gz.age" {
		t.Fatalf("objects %v, want one backup at start", got)
	}
	data, _ := os.ReadFile(filepath.Join(target, got[0]))
	if files := openArchive(t, data, id); files["apps/8000-orders-api/secrets/key.txt"] != "plaintext-marker-5d1e" {
		t.Errorf("backup content %v", files)
	}
	s := c.backup.snapshot()
	if s == nil || s.LastObject != got[0] || s.LastSize != int64(len(data)) || s.LastError != "" {
		t.Errorf("status %+v", s)
	}

	// a workDir change, once the reload loop saw it
	writeFile(t, c.app.WorkDir, "appsettings.json", `{"Db": "Host=db2"}`)
	backupNow()
	if n := len(objects(t, target)); n != 1 {
		t.Errorf("%d objects before the reload loop saw the change, want 1", n)
	}
	c.checkWorkDirs()
	backupNow()
	backupNow()
	if n := len(objects(t, target)); n != 2 {
		t.Errorf("%d objects after a workDir change, want 2", n)
	}
	// an ignored file is not a change
	writeFile(t, c.app.WorkDir, "cache/blob", "other")
	c.checkWorkDirs()
	backupNow()
	// a config.json change
	c.configFile = []byte(`{"apps": [], "pollInterval": "1m"}`)
	backupNow()
	backupNow()
	if n := len(objects(t, target)); n != 3 {
		t.Errorf("%d objects after a config.json change, want 3", n)
	}

	// no plaintext anywhere on disk but the original files, and no .part
	for _, dir := range []string{target, dataDir} {
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte("plaintext-marker")) || strings.HasSuffix(p, ".part") {
				t.Errorf("%s holds plaintext or is left over", p)
			}
			return nil
		})
	}

	// a restart with the same content does not back up again
	var st backupState
	stateData, err := os.ReadFile(filepath.Join(dataDir, "backup-state.json"))
	if err != nil || json.Unmarshal(stateData, &st) != nil || st.Hash != c.backupHash() || st.Size == 0 {
		t.Fatalf("backup-state.json %s (%v)", stateData, err)
	}
	c.backup = newBackupRunner(dataDir)
	c.backup.now = clock.now
	backupNow()
	if n := len(objects(t, target)); n != 3 {
		t.Errorf("restart with unchanged content backed up again: %d objects", n)
	}
	if s := c.backup.snapshot(); s == nil || s.LastObject != st.Object {
		t.Errorf("status after restart %+v, want the last backup from backup-state.json", s)
	}
	if strings.Count(logs.String(), "Backup written to") != 3 {
		t.Errorf("log:\n%s", logs)
	}
}

func TestBackupWithoutDataDirRunsOncePerStart(t *testing.T) {
	captureLog(t)
	c, target, clock, _ := newBackupTestRunner(t, "")
	for range 2 {
		clock.advance(time.Second)
		c.checkBackup()
		c.backup.wg.Wait()
	}
	c.backup = newBackupRunner("")
	c.backup.now = clock.now
	clock.advance(time.Second)
	c.checkBackup()
	c.backup.wg.Wait()
	if n := len(objects(t, target)); n != 2 {
		t.Errorf("%d objects over two starts without dataDir, want 2", n)
	}
}

func TestBackupBackoff(t *testing.T) {
	logs := captureLog(t)
	c, target, clock, _ := newBackupTestRunner(t, "")
	// the target is a file, so the backup cannot be written
	os.WriteFile(target, nil, 0o600)
	try := func(wait time.Duration) bool {
		clock.advance(wait)
		c.backup.mu.Lock()
		before := c.backup.failures
		c.backup.mu.Unlock()
		c.checkBackup()
		c.backup.wg.Wait()
		if c.backup.failures != before {
			return true
		}
		return false
	}

	if !try(0) {
		t.Fatal("first backup not attempted")
	}
	if s := c.backup.snapshot(); s.LastError == "" {
		t.Error("status shows no error")
	}
	// 3m, then 6m, then 12m between attempts
	for _, wait := range []time.Duration{3 * time.Minute, 6 * time.Minute, 12 * time.Minute} {
		if try(wait - time.Second) {
			t.Errorf("attempted before %s", wait)
		}
		if !try(time.Second) {
			t.Errorf("not attempted after %s", wait)
		}
	}
	// capped at an hour
	c.backup.mu.Lock()
	c.backup.failures = 20
	c.backup.mu.Unlock()
	try(24 * time.Minute)
	if try(time.Hour - time.Second) {
		t.Error("attempted before the 1h cap")
	}
	if !try(time.Second) {
		t.Error("not attempted after the 1h cap")
	}
	if n := strings.Count(logs.String(), "Backup failed"); n != 1 {
		t.Errorf("failure logged %d times in one streak, want once:\n%s", n, logs)
	}

	os.Remove(target)
	clock.advance(time.Hour)
	c.checkBackup()
	c.backup.wg.Wait()
	if s := c.backup.snapshot(); s.LastError != "" || s.LastObject == "" {
		t.Errorf("status after recovering %+v", s)
	}
	if !strings.Contains(logs.String(), "Backup succeeded again after") {
		t.Errorf("recovery not logged:\n%s", logs)
	}
}

func TestNoBackupConfigured(t *testing.T) {
	captureLog(t)
	dir := t.TempDir()
	c := newWorkDirRunner(t, dir, nil)
	c.checkBackup()
	if c.backup.running.Load() || c.backup.snapshot() != nil {
		t.Error("backup ran or shows a status without a backup config")
	}
}

func TestAdminStatusBackup(t *testing.T) {
	a := newTestAdmin()
	if body := get(t, a, "/status").Body.String(); !strings.Contains(body, `"backup":null`) {
		t.Errorf("status without backups: %s", body)
	}
	a.backup = newBackupRunner("")
	a.backup.setEnabled(true)
	a.backup.status = backupStatus{LastTime: time.Now(), LastSize: 1234, LastObject: "vm-01/x.tar.gz.age"}
	body := get(t, a, "/status").Body.String()
	for _, want := range []string{`"lastTime":"`, `"lastSize":1234`, `"lastObject":"vm-01/x.tar.gz.age"`, `"lastError":""`} {
		if !strings.Contains(body, want) {
			t.Errorf("status %s does not contain %s", body, want)
		}
	}
}

func TestDiffConfigBackup(t *testing.T) {
	recipient := testIdentity(t).Recipient().String()
	old, next := testConfig(), testConfig()
	old.Backup = &BackupConfig{Recipients: []string{recipient}, Target: BackupTarget{Kind: "dir", Path: "/b"}}
	next.Backup = &BackupConfig{Recipients: []string{recipient}, Target: BackupTarget{Kind: "dir", Path: "/b"}}
	if d := diffConfig(old, next); d.String() != "no changes" {
		t.Errorf("equal backup settings: %s", d)
	}
	next.Backup.Prefix = "vm-01"
	if d := diffConfig(old, next); d.String() != "changed backup" {
		t.Errorf("changed prefix: %s", d)
	}
	// a config.json without backup parses as before
	t.Chdir(t.TempDir())
	if config, err := parseConfig([]byte(validConfigJSON)); err != nil || config.Backup != nil {
		t.Errorf("config without backup: %+v, %v", config, err)
	}
}
