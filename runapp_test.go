package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// appUnderTest is a runApp loop deploying the test binary in "serve" mode
// from a fake GitHub API, in a temporary working directory.
type appUnderTest struct {
	t    *testing.T
	logs *lockedBuffer
	// published makes the API list the artifact; until then it lists none
	published atomic.Bool
	app       App
	config    *Config
	clients   *Clients
	updates   chan appUpdate
	stop      chan struct{}
	done      chan struct{}
	mon       *appMonitor
	// configHash is the hash last sent to the loop
	configHash string
}

func startAppUnderTest(t *testing.T, env map[string]string) *appUnderTest {
	t.Helper()
	return startAppUnderTestIn(t, env, "")
}

// startAppUnderTestIn starts the app with configDir, which may be empty.
func startAppUnderTestIn(t *testing.T, env map[string]string, configDir string) *appUnderTest {
	t.Helper()
	t.Chdir(t.TempDir())
	os.MkdirAll("bin", 0755)
	os.MkdirAll("tmp", 0755)
	zipPath := zipTestBinary(t)

	a := &appUnderTest{t: t, logs: &lockedBuffer{}}
	old := logOutput
	logOutput = a.logs
	t.Cleanup(func() { logOutput = old })

	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			http.ServeFile(w, r, zipPath)
			return
		}
		artifacts := []map[string]any{}
		if a.published.Load() {
			artifacts = append(artifacts, map[string]any{"id": 1, "name": "app", "archive_download_url": api.URL + "/download"})
		}
		json.NewEncoder(w).Encode(map[string]any{"artifacts": artifacts})
	}))
	t.Cleanup(api.Close)

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	a.config = &Config{
		GithubAPI:           api.URL,
		PollInterval:        Duration(50 * time.Millisecond),
		HealthCheckInterval: Duration(200 * time.Millisecond),
		APITimeout:          Duration(5 * time.Second),
		DownloadTimeout:     Duration(time.Minute),
		HealthCheckTimeout:  Duration(time.Second),
		StopTimeout:         Duration(time.Second),
		RestartBackoff:      Duration(50 * time.Millisecond),
		RestartBackoffMax:   Duration(200 * time.Millisecond),
	}
	a.clients = newClients(a.config)
	a.app = App{ExternalPort: port, Repo: "owner/app", BinName: "app", Env: env, ConfigDir: configDir}
	a.mon = newAppMonitor(a.app)
	if configDir != "" {
		a.configHash, _, _ = dirContentHash(configDir, nil)
	}
	a.updates = make(chan appUpdate, 1)
	a.stop = make(chan struct{})
	a.done = make(chan struct{})
	go func() {
		defer close(a.done)
		runApp(a.app, a.config, a.clients, newProcessSet(), a.mon, a.configHash, a.updates, a.stop)
	}()
	t.Cleanup(a.shutdown)
	return a
}

// zipTestBinary stores the test binary as "app" in a zip, like an artifact.
func zipTestBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	path := filepath.Join(t.TempDir(), "app.zip")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	// stored, not deflated, so a large test binary zips fast
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "app", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, in); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// setEnv sends the app a config update with env.
func (a *appUnderTest) setEnv(env map[string]string) {
	a.app.Env = env
	a.updates <- appUpdate{app: a.app, config: a.config, clients: a.clients, configHash: a.configHash}
}

// configChanged sends the app the current hash of its configDir.
func (a *appUnderTest) configChanged() {
	a.configHash, _, _ = dirContentHash(a.app.ConfigDir, nil)
	a.updates <- appUpdate{app: a.app, config: a.config, clients: a.clients, configHash: a.configHash}
}

// get returns the body of path through the app's proxy, or "" on failure.
func (a *appUnderTest) get(path string) string {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", a.app.ExternalPort, path))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return string(body)
}

// waitFor fails the test unless cond holds within 10s.
func (a *appUnderTest) waitFor(what string, cond func() bool) {
	a.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			a.t.Fatalf("%s within 10s; log:\n%s", what, a.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (a *appUnderTest) logged(text string) func() bool {
	return func() bool { return strings.Contains(a.logs.String(), text) }
}

// shutdown stops the loop and waits until nothing logs any more.
func (a *appUnderTest) shutdown() {
	select {
	case <-a.done:
		// it returned on its own before serving, e.g. without its configDir
		return
	default:
	}
	close(a.stop)
	<-a.done
	a.waitFor("proxy stopped", a.logged("Proxy on external port"))
}

func TestEnvChangeBeforeFirstDeployIsUsedByIt(t *testing.T) {
	a := startAppUnderTest(t, map[string]string{"RUNNER_TEST_CHILD": "serve", "GREETING": "old-value"})
	// nothing is published, so the app waits for its first version
	a.waitFor("first poll", a.logged("Failed to find latest artifact"))

	a.setEnv(map[string]string{"RUNNER_TEST_CHILD": "serve", "GREETING": "new-value"})
	a.waitFor("env change logged", a.logged("[owner/app] Env changed (GREETING), applying it on the next start"))

	a.published.Store(true)
	a.waitFor("first version served", func() bool { return a.get("/env?key=GREETING") != "" })
	if got := a.get("/env?key=GREETING"); got != "new-value" {
		t.Errorf("first deployed version has GREETING %q, want new-value", got)
	}
	if strings.Contains(a.logs.String(), "-value") {
		t.Errorf("log contains an env value:\n%s", a.logs)
	}
}

func TestEnvChangeWhileWaitingToRestartIsUsedByTheRestart(t *testing.T) {
	crashFile := filepath.Join(t.TempDir(), "crash")
	env := map[string]string{"RUNNER_TEST_CHILD": "serve", "RUNNER_TEST_CRASH_FILE": crashFile, "GREETING": "old-value"}
	a := startAppUnderTest(t, env)
	a.published.Store(true)
	a.waitFor("first version served", func() bool { return a.get("/env?key=GREETING") == "old-value" })

	// crash it, with every restart crashing on start
	os.WriteFile(crashFile, nil, 0644)
	a.get("/crash")
	a.waitFor("a restart crashing on start", a.logged("Failed to restart artifact 1"))

	a.setEnv(map[string]string{"RUNNER_TEST_CHILD": "serve", "RUNNER_TEST_CRASH_FILE": crashFile, "GREETING": "new-value"})
	a.waitFor("env change logged", a.logged("[owner/app] Env changed (GREETING), applying it on the next start"))
	if a.logged("redeploying artifact")() {
		t.Error("runner redeployed although no process was running")
	}

	os.Remove(crashFile)
	a.waitFor("restart with the new env", func() bool { return a.get("/env?key=GREETING") == "new-value" })
	// the same artifact, restarted rather than deployed again; the proxy
	// reaches it before the restart is logged
	a.waitFor("restart logged", a.logged("Restarted artifact 1"))
	if strings.Count(a.logs.String(), "Downloading artifact") != 1 {
		t.Errorf("new env not served by a restart of artifact 1; log:\n%s", a.logs)
	}
	if strings.Contains(a.logs.String(), "-value") {
		t.Errorf("log contains an env value:\n%s", a.logs)
	}
}

func TestConfigDirChangeRedeploysRunningVersion(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.txt"), []byte("v1"), 0644)
	a := startAppUnderTestIn(t, map[string]string{"RUNNER_TEST_CHILD": "serve"}, dir)
	a.published.Store(true)
	a.waitFor("v1 served from configDir", func() bool { return a.get("/config") == "v1" })
	pid := a.mon.snapshot().PID

	os.WriteFile(filepath.Join(dir, "config.txt"), []byte("v2"), 0644)
	a.configChanged()
	a.waitFor("v2 served", func() bool { return a.get("/config") == "v2" })
	a.waitFor("redeploy recorded", func() bool {
		d := a.mon.snapshot().Deploys
		return len(d) == 2 && d[0].Reason == "config" && d[0].Result == "success"
	})
	if a.mon.snapshot().PID == pid {
		t.Error("config change served by the same process")
	}
	if !a.logged("[owner/app] configDir changed, redeploying artifact 1 with it")() ||
		!a.logged("Starting artifact 1 with the new config on port")() {
		t.Errorf("redeploy not logged:\n%s", a.logs)
	}
	if a.logged("Env changed")() {
		t.Errorf("config change logged as an env change:\n%s", a.logs)
	}
	if strings.Count(a.logs.String(), "Downloading artifact") != 1 {
		t.Errorf("config change downloaded the artifact again:\n%s", a.logs)
	}
}

func TestConfigDirChangeBeforeFirstDeployIsUsedByIt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.txt"), []byte("v1"), 0644)
	a := startAppUnderTestIn(t, map[string]string{"RUNNER_TEST_CHILD": "serve"}, dir)
	a.waitFor("first poll", a.logged("Failed to find latest artifact"))

	os.WriteFile(filepath.Join(dir, "config.txt"), []byte("v2"), 0644)
	a.configChanged()
	a.waitFor("change logged", a.logged("[owner/app] configDir changed, applying it on the next start"))
	a.published.Store(true)
	a.waitFor("first version served", func() bool { return a.get("/config") == "v2" })
}

func TestMissingConfigDirStopsOnlyThatApp(t *testing.T) {
	a := startAppUnderTestIn(t, map[string]string{"RUNNER_TEST_CHILD": "serve"}, filepath.Join(t.TempDir(), "missing"))
	select {
	case <-a.done:
	case <-time.After(2 * time.Second):
		t.Fatal("runApp kept running with a missing configDir")
	}
	if s := a.mon.snapshot(); s.State != stateFailed || !strings.Contains(s.Error, "cannot use configDir") {
		t.Errorf("state %q error %q, want failed with the configDir error", s.State, s.Error)
	}
	if !a.logged("[owner/app] Cannot use configDir, not running this app")() {
		t.Errorf("missing configDir not logged:\n%s", a.logs)
	}
}
