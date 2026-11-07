package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeFile writes content to name under dir, creating its directories.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigIgnored(t *testing.T) {
	ignore := []string{"cache/", "*.tmp", "logs/*.log", "data"}
	for rel, want := range map[string]bool{
		"cache":            true,
		"cache/a/b.json":   true,
		"cachefile":        false,
		"x.tmp":            true,
		"sub/x.tmp":        false,
		"logs/app.log":     true,
		"logs/keep.txt":    false,
		"data":             true,
		"data/inside":      false,
		"appsettings.json": false,
	} {
		if got := configIgnored(rel, ignore); got != want {
			t.Errorf("configIgnored(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestDirHashIgnoresFilesAndTouch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "appsettings.json", `{"a": 1}`)
	writeFile(t, dir, "config/app.yml", "a: 1")
	writeFile(t, dir, "cache/state.bin", "x")
	ignore := []string{"cache/"}

	sig1, err := dirSignature(dir, ignore)
	if err != nil {
		t.Fatal(err)
	}
	hash1, files, err := dirContentHash(dir, ignore)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(files)); !slices.Equal(got, []string{"appsettings.json", "config/app.yml"}) {
		t.Errorf("hashed files %v, want appsettings.json and config/app.yml", got)
	}

	// an ignored file changes nothing
	writeFile(t, dir, "cache/state.bin", "changed")
	writeFile(t, dir, "cache/new.bin", "new")
	if sig, _ := dirSignature(dir, ignore); sig != sig1 {
		t.Error("signature changed with an ignored file")
	}

	// touch changes the signature, not the content hash
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(dir, "appsettings.json"), later, later)
	if sig, _ := dirSignature(dir, ignore); sig == sig1 {
		t.Error("signature unchanged after touch")
	}
	if hash, _, _ := dirContentHash(dir, ignore); hash != hash1 {
		t.Error("content hash changed after touch")
	}

	// a content change changes both, and names the file
	writeFile(t, dir, "config/app.yml", "a: 2")
	hash2, files2, _ := dirContentHash(dir, ignore)
	if hash2 == hash1 {
		t.Error("content hash unchanged after an edit")
	}
	if got := changedFiles(files, files2); !slices.Equal(got, []string{"config/app.yml"}) {
		t.Errorf("changed files %v, want config/app.yml", got)
	}
}

func TestDirSignatureCaps(t *testing.T) {
	dir := t.TempDir()
	for i := range maxConfigFiles + 1 {
		writeFile(t, dir, fmt.Sprintf("many/f%04d", i), "")
	}
	if _, err := dirSignature(dir, nil); err == nil || !strings.Contains(err.Error(), "more than 1000 files") {
		t.Errorf("over %d files: error %v", maxConfigFiles, err)
	}
	// ignored files do not count
	if _, err := dirSignature(dir, []string{"many/"}); err != nil {
		t.Errorf("ignored files counted: %v", err)
	}

	big := t.TempDir()
	f, err := os.Create(filepath.Join(big, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(maxConfigBytes + 1)
	f.Close()
	if _, err := dirSignature(big, nil); err == nil || !strings.Contains(err.Error(), "more than 10 MB") {
		t.Errorf("over 10 MB: error %v", err)
	}
}

func TestValidateConfigDirs(t *testing.T) {
	t.Chdir(t.TempDir())
	cwd, _ := os.Getwd()
	base := func() *Config {
		c := testConfig()
		c.Apps[0].ConfigDir = "/etc/orders-api"
		c.Apps[0].ConfigIgnore = []string{"cache/", "*.tmp"}
		return c
	}
	if err := validateConfig(base()); err != nil {
		t.Fatalf("valid configDir rejected: %v", err)
	}

	for name, tc := range map[string]struct {
		change func(c *Config)
		want   string
	}{
		"relative":      {func(c *Config) { c.Apps[0].ConfigDir = "etc/orders-api" }, "apps[0] (owner/a) configDir must be an absolute path"},
		"inside bin":    {func(c *Config) { c.Apps[0].ConfigDir = filepath.Join(cwd, "bin", "x") }, "configDir must not be inside runner's"},
		"tmp":           {func(c *Config) { c.Apps[0].ConfigDir = filepath.Join(cwd, "tmp") }, "configDir must not be inside runner's"},
		"shared":        {func(c *Config) { c.Apps[1].ConfigDir = "/etc/orders-api/" }, `both use configDir`},
		"ignore alone":  {func(c *Config) { c.Apps[1].ConfigIgnore = []string{"x"} }, "apps[1] (owner/b) configIgnore needs configDir"},
		"bad pattern":   {func(c *Config) { c.Apps[0].ConfigIgnore = []string{"[a-"} }, "apps[0] (owner/a) configIgnore: pattern"},
		"empty pattern": {func(c *Config) { c.Apps[0].ConfigIgnore = []string{"/"} }, "configIgnore: pattern must not be empty"},
		"absolute":      {func(c *Config) { c.Apps[0].ConfigIgnore = []string{"/x"} }, "configIgnore: pattern \"/x\" must be relative"},
		"parent":        {func(c *Config) { c.Apps[0].ConfigIgnore = []string{"../x"} }, "configIgnore: pattern \"../x\" must not contain .."},
	} {
		c := base()
		tc.change(c)
		err := validateConfig(c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, tc.want)
		}
	}
	// a directory next to bin with a similar name is fine
	c := base()
	c.Apps[0].ConfigDir = filepath.Join(cwd, "binaries")
	if err := validateConfig(c); err != nil {
		t.Errorf("configDir next to bin rejected: %v", err)
	}
}

// configDirRunner is an appRunner with one app on port 8000 whose loop is
// only its updates channel, for checkConfigDirs.
type configDirRunner struct {
	*appRunner
	app App
	h   *appHandle
	// settled runs during the settle wait of the next tick
	settled func()
}

func newConfigDirRunner(t *testing.T, dir string, ignore []string) *configDirRunner {
	t.Helper()
	config := testConfig()
	config.Apps = []App{{ExternalPort: 8000, Repo: "owner/orders-api", BinName: "orders-api", ConfigDir: dir, ConfigIgnore: ignore}}
	r := newAppRunner(config, newProcessSet())
	c := &configDirRunner{appRunner: r, app: config.Apps[0]}
	r.configSettle = func() {
		if c.settled != nil {
			c.settled()
			c.settled = nil
		}
	}
	c.h = &appHandle{mon: newAppMonitor(c.app), updates: make(chan appUpdate, 1)}
	r.apps[8000] = c.h
	r.readConfigDir(c.app, c.h.mon)
	return c
}

// tick runs one check and returns the update sent to the app, if any.
func (c *configDirRunner) tick() *appUpdate {
	c.checkConfigDirs()
	select {
	case u := <-c.h.updates:
		return &u
	default:
		return nil
	}
}

func TestCheckConfigDirs(t *testing.T) {
	logs := captureLog(t)
	dir := t.TempDir()
	writeFile(t, dir, "appsettings.json", `{"a": 1}`)
	c := newConfigDirRunner(t, dir, []string{"cache/"})
	first := c.configDirs[8000].hash
	if first == "" || c.h.mon.snapshot().ConfigHash != first[:8] {
		t.Fatalf("initial hash %q not read or not shown (%q)", first, c.h.mon.snapshot().ConfigHash)
	}

	if u := c.tick(); u != nil {
		t.Error("unchanged configDir sent an update")
	}

	// touch without a content change
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(dir, "appsettings.json"), later, later)
	if u := c.tick(); u != nil {
		t.Error("touch sent an update")
	}

	// an ignored file
	writeFile(t, dir, "cache/x", "1")
	if u := c.tick(); u != nil {
		t.Error("an ignored file sent an update")
	}

	// a content change sends exactly one update, with the new hash
	writeFile(t, dir, "appsettings.json", `{"a": 2}`)
	u := c.tick()
	if u == nil || u.configHash == first || u.configHash != c.configDirs[8000].hash {
		t.Fatalf("content change: update %+v, want one with the new hash", u)
	}
	if !strings.Contains(logs.String(), "[owner/orders-api] configDir changed (appsettings.json), redeploying") {
		t.Errorf("change not logged:\n%s", logs)
	}
	if u := c.tick(); u != nil {
		t.Error("the same change sent a second update")
	}
	if s := c.h.mon.snapshot(); s.ConfigHash != u.configHash[:8] || s.ConfigChangedAt.IsZero() {
		t.Errorf("status hash %q changed %v, want the new hash and a time", s.ConfigHash, s.ConfigChangedAt)
	}
}

func TestCheckConfigDirsSkipsChangeBeingWritten(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "appsettings.json", `{"a": 1}`)
	c := newConfigDirRunner(t, dir, nil)

	// the file changes again while the tick waits for it to settle
	writeFile(t, dir, "appsettings.json", `{"a": 2`)
	c.settled = func() { writeFile(t, dir, "appsettings.json", `{"a": 2}`+strings.Repeat(" ", 10)) }
	if u := c.tick(); u != nil {
		t.Error("a change still being written sent an update")
	}
	u := c.tick()
	if u == nil {
		t.Fatal("settled change sent no update on the next tick")
	}
	if c.tick() != nil {
		t.Error("settled change sent a second update")
	}
}

func TestCheckConfigDirsLoopGuard(t *testing.T) {
	logs := captureLog(t)
	dir := t.TempDir()
	writeFile(t, dir, "appsettings.json", "1")
	c := newConfigDirRunner(t, dir, nil)

	writeFile(t, dir, "appsettings.json", "2")
	if c.tick() == nil {
		t.Fatal("change sent no update")
	}
	// the redeployed app writes into its configDir right away
	writeFile(t, dir, "state.json", "written by the app")
	if u := c.tick(); u != nil {
		t.Error("a change right after a redeploy sent an update")
	}
	if !strings.Contains(logs.String(), "[owner/orders-api] configDir changed right after a redeploy (state.json); is the app writing there? Add those paths to configIgnore") {
		t.Errorf("loop guard not logged:\n%s", logs)
	}
	// that write is not taken for a change later on
	if u := c.tick(); u != nil {
		t.Error("the skipped change sent an update on a later tick")
	}
	// a later change redeploys again; the app's loop never saw state.json
	// so a config.json reload in between must not redeploy either
	c.apply(c.config)
	select {
	case u := <-c.h.updates:
		if u.configHash != c.configDirs[8000].applied {
			t.Error("reload sent the skipped hash")
		}
	default:
	}
	writeFile(t, dir, "appsettings.json", "3")
	if c.tick() == nil {
		t.Error("a later change sent no update")
	}
}

func TestCheckConfigDirsUnreadable(t *testing.T) {
	logs := captureLog(t)
	dir := t.TempDir()
	writeFile(t, dir, "appsettings.json", "1")
	c := newConfigDirRunner(t, dir, nil)

	for range 3 {
		for i := range maxConfigFiles {
			writeFile(t, dir, fmt.Sprintf("many/f%04d", i), "")
		}
		c.tick()
	}
	if n := strings.Count(logs.String(), "Failed to read configDir"); n != 1 {
		t.Errorf("error logged %d times, want once:\n%s", n, logs)
	}
	if c.h.mon.snapshot().ConfigError == "" {
		t.Error("status shows no configError")
	}
	os.RemoveAll(filepath.Join(dir, "many"))
	if u := c.tick(); u != nil {
		t.Error("readable again with the same content sent an update")
	}
	if c.h.mon.snapshot().ConfigError != "" {
		t.Error("configError not cleared")
	}
}

func TestApplyConfigDirChangeResetsState(t *testing.T) {
	captureLog(t)
	dir, other := t.TempDir(), t.TempDir()
	writeFile(t, dir, "a.json", "1")
	writeFile(t, other, "a.json", "other")
	c := newConfigDirRunner(t, dir, nil)

	next := testConfig()
	next.Apps = []App{c.app}
	next.Apps[0].ConfigDir = other
	d := diffConfig(c.config, next)
	if len(d.updated) != 1 || !slices.Equal(d.updated[0].fields, []string{"configDir"}) || len(d.replaced) != 0 {
		t.Fatalf("configDir change: updated %+v, replaced %+v, want an update", d.updated, d.replaced)
	}
	c.apply(next)
	u := <-c.h.updates
	want, _, _ := dirContentHash(other, nil)
	if u.configHash != want || u.app.ConfigDir != other {
		t.Errorf("update has configDir %q hash %q, want %q %q", u.app.ConfigDir, u.configHash, other, want)
	}

	// configIgnore alone is an update too
	next2 := testConfig()
	next2.Apps = []App{next.Apps[0]}
	next2.Apps[0].ConfigIgnore = []string{"cache/"}
	if d := diffConfig(next, next2); len(d.updated) != 1 || !slices.Equal(d.updated[0].fields, []string{"configIgnore"}) {
		t.Errorf("configIgnore change: updated %+v", d.updated)
	}
}

func TestConfigWithoutConfigDirIsUnchanged(t *testing.T) {
	t.Chdir(t.TempDir())
	// a config.json from before configDir existed
	config, err := parseConfig([]byte(validConfigJSON))
	if err != nil {
		t.Fatal(err)
	}
	if config.Apps[0].ConfigDir != "" || config.Apps[0].ConfigIgnore != nil {
		t.Fatalf("app has configDir %q ignore %v, want none", config.Apps[0].ConfigDir, config.Apps[0].ConfigIgnore)
	}
	again, _ := parseConfig([]byte(validConfigJSON))
	if d := diffConfig(config, again); d.String() != "no changes" {
		t.Errorf("diff %q, want no changes", d)
	}

	r := newAppRunner(config, newProcessSet())
	r.configSettle = func() { t.Error("settle wait without a configDir") }
	h := &appHandle{mon: newAppMonitor(config.Apps[0]), updates: make(chan appUpdate, 1)}
	r.apps[8000] = h
	r.readConfigDir(config.Apps[0], h.mon)
	r.checkConfigDirs()
	if len(r.configDirs) != 0 || len(h.updates) != 0 {
		t.Errorf("app without configDir has state %v or got an update", r.configDirs)
	}
	if s := h.mon.snapshot(); s.ConfigHash != "" || s.ConfigError != "" {
		t.Errorf("status shows config %q %q", s.ConfigHash, s.ConfigError)
	}
}
