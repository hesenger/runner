package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// adminFields are the settings of the admin UI, which a reload does not
// apply: they need a runner restart.
var adminFields = []string{"adminPort", "adminAddress", "adminUser", "adminPassword"}

// dataFields are the settings of the files in dataDir, which a reload does
// not apply either.
var dataFields = []string{"dataDir", "retentionDays", "accessLog", "accessLogRetentionDays"}

// timeoutFields are the settings the HTTP clients are built from.
var timeoutFields = []string{"apiTimeout", "downloadTimeout", "healthCheckTimeout"}

// appChange is an app whose fields changed on the same externalPort.
type appChange struct {
	// app is the app as it is in the new config
	app App
	// fields are the JSON names of the changed fields
	fields []string
	// envKeys are the names of the changed env variables, never their values
	envKeys []string
}

// configDiff is what changed between two configs. Apps are matched by
// externalPort.
type configDiff struct {
	added   []App
	removed []App
	// replaced apps changed repo or binName, so they are stopped and started
	// again
	replaced []appChange
	// updated apps changed only token, branch, artifactName, env, configDir
	// or configIgnore, which their loop applies: env and configDir through a
	// zero-downtime redeploy of the running version, the others from the next
	// poll without restarting it
	updated []appChange
	// globals are the JSON names of the changed top-level fields
	globals []string
}

// diffConfig compares the config runner runs with a reloaded one.
func diffConfig(old, next *Config) configDiff {
	var d configDiff
	d.globals = changedFields(*old, *next)

	oldApps := map[int]App{}
	for _, app := range old.Apps {
		oldApps[app.ExternalPort] = app
	}
	nextPorts := map[int]bool{}
	for _, app := range next.Apps {
		nextPorts[app.ExternalPort] = true
		was, ok := oldApps[app.ExternalPort]
		if !ok {
			d.added = append(d.added, app)
			continue
		}
		c := appChange{app: app, fields: changedFields(was, app), envKeys: changedEnvKeys(was.Env, app.Env)}
		switch {
		case len(c.fields) == 0:
		case slices.Contains(c.fields, "repo") || slices.Contains(c.fields, "binName"):
			d.replaced = append(d.replaced, c)
		default:
			d.updated = append(d.updated, c)
		}
	}
	for _, app := range old.Apps {
		if !nextPorts[app.ExternalPort] {
			d.removed = append(d.removed, app)
		}
	}
	return d
}

// changedFields returns the JSON names of the fields that differ between two
// structs of the same type, leaving out the apps list.
func changedFields(a, b any) []string {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	var fields []string
	for i := range va.NumField() {
		name := strings.Split(va.Type().Field(i).Tag.Get("json"), ",")[0]
		if name == "apps" {
			continue
		}
		fa, fb := va.Field(i), vb.Field(i)
		if fa.Kind() == reflect.Map || fa.Kind() == reflect.Slice {
			// a missing env or list is the same as an empty one
			if fa.Len() == 0 && fb.Len() == 0 || reflect.DeepEqual(fa.Interface(), fb.Interface()) {
				continue
			}
		} else if fa.Equal(fb) {
			continue
		}
		fields = append(fields, name)
	}
	return fields
}

// changedEnvKeys returns the sorted names of the variables added, removed or
// changed between two envs.
func changedEnvKeys(a, b map[string]string) []string {
	var keys []string
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			keys = append(keys, key)
		}
	}
	for key := range b {
		if _, ok := a[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// changed reports whether any of fields is in d.globals.
func (d configDiff) changed(fields ...string) bool {
	for _, f := range fields {
		if slices.Contains(d.globals, f) {
			return true
		}
	}
	return false
}

// String summarizes the diff for the log, without any values, so tokens and
// passwords are never logged.
func (d configDiff) String() string {
	var parts []string
	describe := func(verb string, apps []App) {
		for _, app := range apps {
			parts = append(parts, fmt.Sprintf("%s %s:%d", verb, app.Repo, app.ExternalPort))
		}
	}
	describeChanges := func(verb string, changes []appChange) {
		for _, c := range changes {
			fields := slices.Clone(c.fields)
			if i := slices.Index(fields, "env"); i >= 0 {
				fields[i] = "env " + strings.Join(c.envKeys, " ")
			}
			parts = append(parts, fmt.Sprintf("%s %s:%d (%s)", verb, c.app.Repo, c.app.ExternalPort, strings.Join(fields, ", ")))
		}
	}
	describe("added", d.added)
	describe("removed", d.removed)
	describeChanges("restarted", d.replaced)
	describeChanges("updated", d.updated)
	if len(d.globals) > 0 {
		parts = append(parts, "changed "+strings.Join(d.globals, ", "))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, "; ")
}

// configWatcher reads config.json again and reports a new valid config once
// per change of its content.
type configWatcher struct {
	// last is a hash of the content last read, or the error reading it, so
	// an unchanged file is ignored and each bad edit is logged only once
	last string
}

// contentKey identifies the content of config.json.
func contentKey(configFile []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(configFile))
}

// check returns the config read from config.json when its content changed
// since the last check and it is valid, and nil otherwise.
func (w *configWatcher) check() *Config {
	config, configFile, err := readConfig()
	key := "error:"
	if configFile != nil {
		key = contentKey(configFile)
	} else if err != nil {
		key += err.Error()
	}
	if key == w.last {
		return nil
	}
	w.last = key
	if err != nil {
		logf("[MAIN] Failed to reload config, keeping the last good one: %v\n", err)
		return nil
	}
	return config
}

// appHandle is an app runner runs: its monitor and the channels to its
// runApp loop.
type appHandle struct {
	mon     *appMonitor
	updates chan appUpdate
	stop    chan struct{}
	// done is closed once runApp has returned
	done chan struct{}
}

// appRunner runs the apps of the config, by externalPort, and applies a
// reloaded config to them. Only one goroutine uses it: main at startup, then
// the config reload loop.
type appRunner struct {
	config   *Config
	clients  *Clients
	procs    *processSet
	apps     map[int]*appHandle
	monitors *appList
	// data is where apps keep their files, as runner started
	data dataSettings
	// stopTimeout is config.StopTimeout, for main to read on shutdown
	stopTimeout atomic.Int64
	// configDirs is what is known of each app's configDir, by externalPort
	configDirs map[int]*configDirState
	// configSettle waits for a configDir whose metadata changed to settle:
	// it must stay unchanged that long before its content is read, so a
	// change still being written is not picked up. Tests replace it.
	configSettle func()
}

func newAppRunner(config *Config, procs *processSet) *appRunner {
	r := &appRunner{
		config:   config,
		clients:  newClients(config),
		procs:    procs,
		apps:     map[int]*appHandle{},
		monitors: newAppList(),
		data:     newDataSettings(config),

		configDirs:   map[int]*configDirState{},
		configSettle: func() { time.Sleep(2 * time.Second) },
	}
	r.stopTimeout.Store(int64(config.StopTimeout))
	return r
}

// startAll starts every app of the config.
func (r *appRunner) startAll() {
	for _, app := range r.config.Apps {
		r.start(app)
	}
	r.showApps()
}

// start starts app's loop.
func (r *appRunner) start(app App) {
	mon := newAppMonitor(app)
	if r.data.dir != "" {
		mon.keepFiles(r.data)
	}
	h := &appHandle{
		mon:     mon,
		updates: make(chan appUpdate, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	r.apps[app.ExternalPort] = h
	logf("[MAIN] Starting app %s:%d\n", app.Repo, app.ExternalPort)
	// the hash is read before the first start, so the first tick does not
	// take the content the app started with for a change
	r.readConfigDir(app, mon)
	configHash := ""
	if st := r.configDirs[app.ExternalPort]; st != nil {
		configHash = st.applied
	}
	// the loop gets later configs through h.updates only
	config, clients := r.config, r.clients
	go func() {
		defer close(h.done)
		runApp(app, config, clients, r.procs, h.mon, configHash, h.updates, h.stop)
	}()
}

// readConfigDir reads the content of app's configDir as it is now, forgetting
// what was known of it before. A directory that does not exist is only
// reported by the app's loop, which does not start the app.
func (r *appRunner) readConfigDir(app App, mon *appMonitor) {
	if app.ConfigDir == "" {
		delete(r.configDirs, app.ExternalPort)
		mon.update(func(s *appStatus) {
			s.ConfigHash, s.ConfigChangedAt, s.ConfigError = "", time.Time{}, ""
		})
		return
	}
	st, newest, err := readConfigDirState(app)
	r.configDirs[app.ExternalPort] = st
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		logf("[%s] Failed to read configDir, its changes are not detected: %v\n", app.Repo, err)
	}
	mon.update(func(s *appStatus) {
		s.ConfigHash, s.ConfigChangedAt, s.ConfigError = shortHash(st.hash), newest, st.err
	})
}

// sendUpdate gives app's loop the current config, replacing an update it has
// not taken yet: only the latest one matters, and the loop may be busy
// deploying, so it does not wait.
func (r *appRunner) sendUpdate(h *appHandle, app App) {
	configHash := ""
	if st := r.configDirs[app.ExternalPort]; st != nil {
		configHash = st.applied
	}
	select {
	case <-h.updates:
	default:
	}
	h.updates <- appUpdate{app: app, config: r.config, clients: r.clients, configHash: configHash}
}

// stop stops the apps on ports in parallel and returns once they are
// stopped. An app in the middle of a deploy finishes it first.
func (r *appRunner) stop(ports []int) {
	for _, port := range ports {
		close(r.apps[port].stop)
	}
	for _, port := range ports {
		<-r.apps[port].done
		delete(r.apps, port)
		delete(r.configDirs, port)
	}
}

// showApps shows the running apps in the admin UI, in config order.
func (r *appRunner) showApps() {
	monitors := []*appMonitor{}
	for _, app := range r.config.Apps {
		if h, ok := r.apps[app.ExternalPort]; ok {
			monitors = append(monitors, h.mon)
		}
	}
	r.monitors.set(monitors)
}

// apply moves the running apps to next: removed apps and apps whose repo or
// binName changed are stopped, new ones started, and every other app gets the
// new config through its loop.
func (r *appRunner) apply(next *Config) {
	d := diffConfig(r.config, next)
	logf("[MAIN] Reloaded config.json: %s\n", d)
	if d.String() == "no changes" {
		return
	}
	if d.changed(adminFields...) {
		logf("[MAIN] Changes to %s need a runner restart, the admin UI keeps its current settings\n",
			strings.Join(adminFields, ", "))
	}
	if d.changed(dataFields...) {
		logf("[MAIN] Changes to %s need a runner restart, logs and metrics keep their current settings\n",
			strings.Join(dataFields, ", "))
	}

	var stopping []int
	for _, app := range d.removed {
		stopping = append(stopping, app.ExternalPort)
	}
	for _, c := range d.replaced {
		logf("[MAIN] App on external port %d changed %s, restarting it\n", c.app.ExternalPort, strings.Join(c.fields, ", "))
		stopping = append(stopping, c.app.ExternalPort)
	}
	r.stop(stopping)

	if d.changed(timeoutFields...) {
		// a deploy in progress keeps using the old clients until it is done
		old := r.clients
		r.clients = newClients(next)
		old.Download.CloseIdleConnections()
	}
	r.config = next
	r.stopTimeout.Store(int64(next.StopTimeout))

	// a new configDir or configIgnore starts over from its current content
	for _, c := range d.updated {
		if slices.Contains(c.fields, "configDir") || slices.Contains(c.fields, "configIgnore") {
			r.readConfigDir(c.app, r.apps[c.app.ExternalPort].mon)
		}
	}

	for _, app := range next.Apps {
		h, ok := r.apps[app.ExternalPort]
		if !ok {
			r.start(app)
			continue
		}
		r.sendUpdate(h, app)
	}
	r.showApps()
}

// checkConfigDirs redeploys the running version of each app whose configDir
// content changed since the last check. A change of file metadata is only
// read once it stayed the same for 2 seconds, so a file still being
// written is picked up on a later tick, and a file touched without changing
// its content redeploys nothing.
func (r *appRunner) checkConfigDirs() {
	type candidate struct {
		app App
		st  *configDirState
		sig string
		// guard: the app's loop was sent a change on the previous tick
		guard bool
	}
	var changed []candidate
	for _, app := range r.config.Apps {
		st := r.configDirs[app.ExternalPort]
		if app.ConfigDir == "" || st == nil || r.apps[app.ExternalPort] == nil {
			continue
		}
		guard := st.redeployed
		st.redeployed = false
		sig, err := dirSignature(app.ConfigDir, app.ConfigIgnore)
		if err != nil {
			r.configDirFailed(app, st, err)
			continue
		}
		if st.err != "" {
			logf("[%s] configDir can be read again\n", app.Repo)
			st.err = ""
			r.apps[app.ExternalPort].mon.update(func(s *appStatus) { s.ConfigError = "" })
		}
		if sig != st.signature {
			changed = append(changed, candidate{app: app, st: st, sig: sig, guard: guard})
		}
	}
	if len(changed) == 0 {
		return
	}
	r.configSettle()

	for _, c := range changed {
		app, st := c.app, c.st
		mon := r.apps[app.ExternalPort].mon
		sig, err := dirSignature(app.ConfigDir, app.ConfigIgnore)
		if err != nil {
			r.configDirFailed(app, st, err)
			continue
		}
		if sig != c.sig {
			// still being written, read it again on the next tick
			continue
		}
		hash, files, err := dirContentHash(app.ConfigDir, app.ConfigIgnore)
		if err != nil {
			r.configDirFailed(app, st, err)
			continue
		}
		if hash == st.hash {
			// touched, same content
			st.signature = sig
			continue
		}
		names := describeFiles(changedFiles(st.files, files))
		st.signature, st.hash, st.files = sig, hash, files
		mon.update(func(s *appStatus) {
			s.ConfigHash = shortHash(hash)
			s.ConfigChangedAt = time.Now()
		})
		if c.guard {
			// the new process changed its own config, so redeploying for it
			// would start a loop
			logf("[%s] configDir changed right after a redeploy (%s); is the app writing there? Add those paths to configIgnore\n", app.Repo, names)
			continue
		}
		logf("[%s] configDir changed (%s), redeploying\n", app.Repo, names)
		st.applied = hash
		st.redeployed = true
		r.sendUpdate(r.apps[app.ExternalPort], app)
	}
}

// configDirFailed logs an error reading app's configDir once until it
// changes or the dir can be read again.
func (r *appRunner) configDirFailed(app App, st *configDirState, err error) {
	if err.Error() == st.err {
		return
	}
	st.err = err.Error()
	logf("[%s] Failed to read configDir, its changes are not detected: %v\n", app.Repo, err)
	r.apps[app.ExternalPort].mon.update(func(s *appStatus) { s.ConfigError = st.err })
}

// watchConfig reads config.json every configReloadInterval and applies it
// when its content changed, then checks every configDir, until ctx is done.
// configFile is the content runner started with.
func (r *appRunner) watchConfig(ctx context.Context, configFile []byte) {
	interval := time.Duration(r.config.ConfigReloadInterval)
	if interval == 0 {
		return
	}
	w := &configWatcher{last: contentKey(configFile)}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if next := w.check(); next != nil && ctx.Err() == nil {
			r.apply(next)
			if d := time.Duration(next.ConfigReloadInterval); d != interval {
				if d == 0 {
					logf("[MAIN] configReloadInterval is 0, no longer reloading config.json or checking configDirs until runner restarts\n")
					return
				}
				interval = d
				ticker.Reset(d)
			}
		}
		if ctx.Err() == nil {
			r.checkConfigDirs()
		}
	}
}
