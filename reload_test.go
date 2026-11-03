package main

import (
	"bytes"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testConfig is a valid config with two apps.
func testConfig() *Config {
	return &Config{
		GithubAPI:            "https://api.github.com",
		PollInterval:         Duration(time.Second),
		HealthCheckInterval:  Duration(time.Second),
		APITimeout:           Duration(time.Second),
		DownloadTimeout:      Duration(time.Second),
		HealthCheckTimeout:   Duration(time.Second),
		StopTimeout:          Duration(time.Second),
		RestartBackoff:       Duration(time.Second),
		RestartBackoffMax:    Duration(time.Minute),
		ConfigReloadInterval: Duration(time.Second),
		Apps: []App{
			{ExternalPort: 8000, Repo: "owner/a", BinName: "a", Token: "t1", Env: map[string]string{"KEEP": "1", "SECRET": "s1"}},
			{ExternalPort: 8001, Repo: "owner/b", BinName: "b", Token: "t1"},
		},
	}
}

// ports returns the external ports of apps.
func ports(apps []App) []int {
	var p []int
	for _, app := range apps {
		p = append(p, app.ExternalPort)
	}
	return p
}

func TestDiffConfigUnchanged(t *testing.T) {
	old, next := testConfig(), testConfig()
	// a missing env is the same as an empty one
	next.Apps[1].Env = map[string]string{}
	d := diffConfig(old, next)
	if got := d.String(); got != "no changes" {
		t.Errorf("diff of equal configs is %q, want no changes", got)
	}
}

func TestDiffConfigApps(t *testing.T) {
	old, next := testConfig(), testConfig()
	// 8000: repo changed, so restarted; 8001 removed; 8002 added; 8003
	// updated in place
	old.Apps = append(old.Apps, App{ExternalPort: 8003, Repo: "owner/d", BinName: "d", Token: "t1", Branch: "main"})
	next.Apps = []App{
		{ExternalPort: 8003, Repo: "owner/d", BinName: "d", Token: "t2", Branch: "", ArtifactName: "d"},
		{ExternalPort: 8000, Repo: "owner/a2", BinName: "a", Token: "t1", Env: map[string]string{"KEEP": "1", "SECRET": "s1"}},
		{ExternalPort: 8002, Repo: "owner/c", BinName: "c", Token: "t1"},
	}

	d := diffConfig(old, next)
	if got := ports(d.added); !slices.Equal(got, []int{8002}) {
		t.Errorf("added %v, want [8002]", got)
	}
	if got := ports(d.removed); !slices.Equal(got, []int{8001}) {
		t.Errorf("removed %v, want [8001]", got)
	}
	if len(d.replaced) != 1 || d.replaced[0].app.ExternalPort != 8000 || !slices.Equal(d.replaced[0].fields, []string{"repo"}) {
		t.Errorf("replaced %+v, want 8000 with repo changed", d.replaced)
	}
	if len(d.updated) != 1 || d.updated[0].app.ExternalPort != 8003 ||
		!slices.Equal(d.updated[0].fields, []string{"token", "branch", "artifactName"}) {
		t.Errorf("updated %+v, want 8003 with token, branch, artifactName changed", d.updated)
	}
	if d.updated[0].app.Token != "t2" {
		t.Errorf("updated app has token %q, want the new one", d.updated[0].app.Token)
	}
	if len(d.globals) != 0 {
		t.Errorf("globals %v, want none", d.globals)
	}
}

func TestDiffConfigBinNameRestartsApp(t *testing.T) {
	old, next := testConfig(), testConfig()
	next.Apps[1].BinName = "b2"
	d := diffConfig(old, next)
	if len(d.replaced) != 1 || d.replaced[0].app.ExternalPort != 8001 || len(d.updated) != 0 {
		t.Errorf("binName change: replaced %+v, updated %+v, want 8001 replaced", d.replaced, d.updated)
	}
}

func TestDiffConfigEnvLogsKeysOnly(t *testing.T) {
	old, next := testConfig(), testConfig()
	next.Apps[0].Env = map[string]string{"KEEP": "1", "SECRET": "s2-new-value", "ADDED": "s3-new-value"}
	d := diffConfig(old, next)
	if len(d.updated) != 1 || !slices.Equal(d.updated[0].fields, []string{"env"}) {
		t.Fatalf("env change: updated %+v, want 8000 with env changed", d.updated)
	}
	if got := d.updated[0].envKeys; !slices.Equal(got, []string{"ADDED", "SECRET"}) {
		t.Errorf("changed env keys %v, want [ADDED SECRET]", got)
	}

	// a removed variable counts as changed too
	if got := changedEnvKeys(map[string]string{"GONE": "x", "KEEP": "1"}, map[string]string{"KEEP": "1"}); !slices.Equal(got, []string{"GONE"}) {
		t.Errorf("changedEnvKeys with a removed variable = %v, want [GONE]", got)
	}

	summary := d.String()
	if summary != "updated owner/a:8000 (env ADDED SECRET)" {
		t.Errorf("summary %q", summary)
	}
	for _, value := range []string{"s1", "s2-new-value", "s3-new-value"} {
		if strings.Contains(summary, value) {
			t.Errorf("summary %q contains the env value %q", summary, value)
		}
	}
}

func TestDiffConfigGlobals(t *testing.T) {
	old, next := testConfig(), testConfig()
	next.PollInterval = Duration(2 * time.Second)
	next.APITimeout = Duration(2 * time.Second)
	next.AdminPassword = "new-password"
	next.ConfigReloadInterval = 0

	d := diffConfig(old, next)
	want := []string{"pollInterval", "apiTimeout", "configReloadInterval", "adminPassword"}
	if !slices.Equal(d.globals, want) {
		t.Errorf("globals %v, want %v", d.globals, want)
	}
	if !d.changed(timeoutFields...) || !d.changed(adminFields...) {
		t.Errorf("diff does not report the timeout and admin changes: %v", d.globals)
	}
	if len(d.added)+len(d.removed)+len(d.replaced)+len(d.updated) != 0 {
		t.Errorf("only globals changed, got app changes %+v", d)
	}
	if strings.Contains(d.String(), "new-password") {
		t.Errorf("summary %q contains the admin password", d.String())
	}
	if d.changed("healthCheckTimeout") {
		t.Error("diff reports an unchanged field")
	}
}

// lockedBuffer is a buffer that app goroutines can log to while the test
// reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const validConfigJSON = `{"apps": [{"externalPort": 8000, "repo": "owner/a", "binName": "a", "token": "t"}]}`

func TestConfigWatcher(t *testing.T) {
	t.Chdir(t.TempDir())
	logs := captureLog(t)
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile("config.json", []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	failures := func() int { return strings.Count(logs.String(), "Failed to reload config") }

	write(validConfigJSON)
	w := &configWatcher{last: contentKey([]byte(validConfigJSON))}
	if w.check() != nil {
		t.Error("check returned a config although the file is unchanged")
	}

	// an invalid edit is logged once, however often it is checked
	write(`{"apps": [`)
	for range 3 {
		if w.check() != nil {
			t.Error("check returned a config for invalid JSON")
		}
	}
	if failures() != 1 {
		t.Errorf("invalid JSON logged %d times, want once:\n%s", failures(), logs)
	}

	// another invalid content is logged again: unknown field, then a value
	// that fails validation
	write(`{"apps": [], "unknownField": 1}`)
	w.check()
	write(`{"pollInterval": "-1s", "apps": []}`)
	w.check()
	if failures() != 3 {
		t.Errorf("logged %d failures for 3 distinct invalid contents, want 3:\n%s", failures(), logs)
	}
	if !strings.Contains(logs.String(), "unknownField") || !strings.Contains(logs.String(), "pollInterval must be greater than 0") {
		t.Errorf("log does not explain the failures:\n%s", logs)
	}

	// a missing file is logged once too
	os.Remove("config.json")
	w.check()
	w.check()
	if failures() != 4 {
		t.Errorf("logged %d failures after removing the file, want 4:\n%s", failures(), logs)
	}

	// back to a valid content: it is returned once, with defaults applied
	write(strings.Replace(validConfigJSON, `"t"`, `"t2"`, 1))
	config := w.check()
	if config == nil {
		t.Fatal("check did not return the valid config")
	}
	if config.Apps[0].Token != "t2" || config.PollInterval != Duration(time.Minute) ||
		config.ConfigReloadInterval != Duration(time.Minute) {
		t.Errorf("config %+v, want token t2 and default intervals", config)
	}
	if w.check() != nil {
		t.Error("check returned the same config twice")
	}
}

// listening reports whether something accepts connections on port.
func listening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func TestAppRunnerAddsAndRemovesApps(t *testing.T) {
	logs := &lockedBuffer{}
	old := logOutput
	logOutput = logs
	t.Cleanup(func() { logOutput = old })
	// an API that always fails, so the apps never deploy anything
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer api.Close()
	portA, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	portB, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	config := testConfig()
	config.GithubAPI = api.URL
	config.Apps = []App{{ExternalPort: portA, Repo: "owner/a", BinName: "a"}}
	procs := newProcessSet()
	r := newAppRunner(config, procs)
	r.startAll()
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s within 2s; log:\n%s", what, logs)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	eventually("app a listening", func() bool { return listening(portA) })

	// add b, remove a
	next := testConfig()
	next.GithubAPI = api.URL
	next.Apps = []App{{ExternalPort: portB, Repo: "owner/b", BinName: "b"}}
	r.apply(next)
	if listening(portA) {
		t.Errorf("port %d of removed app a still accepts connections", portA)
	}
	eventually("added app b listening", func() bool { return listening(portB) })
	if got := r.monitors.get(); len(got) != 1 || got[0].app.ExternalPort != portB {
		t.Errorf("admin UI shows %d apps, want only b", len(got))
	}
	if len(r.apps) != 1 || r.apps[portB] == nil {
		t.Errorf("runner tracks %d apps, want only b", len(r.apps))
	}
	if !strings.Contains(logs.String(), fmt.Sprintf("[owner/a] Stopped app on external port %d", portA)) {
		t.Errorf("log does not show app a stopped:\n%s", logs)
	}

	// a token change goes to the running loop, which keeps its port
	updated := testConfig()
	updated.GithubAPI = api.URL
	updated.Apps = []App{{ExternalPort: portB, Repo: "owner/b", BinName: "b", Token: "new"}}
	r.apply(updated)
	if !listening(portB) {
		t.Errorf("app b stopped listening after a token change")
	}
	if !strings.Contains(logs.String(), fmt.Sprintf("updated owner/b:%d (token)", portB)) {
		t.Errorf("log does not show the token update:\n%s", logs)
	}

	// the proxies log once they stopped, so nothing logs after the test
	r.stop(slices.Collect(maps.Keys(r.apps)))
	for _, port := range []int{portA, portB} {
		eventually("proxy stopped", func() bool {
			return strings.Contains(logs.String(), fmt.Sprintf("Proxy on external port %d stopped", port))
		})
	}
}
