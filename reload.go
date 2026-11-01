package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// adminFields are the settings of the admin UI, which a reload does not
// apply: they need a runner restart.
var adminFields = []string{"adminPort", "adminUser", "adminPassword"}

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
	// updated apps changed only token, branch, artifactName or env, which
	// their loop applies: env through a zero-downtime redeploy of the running
	// version, the others from the next poll without restarting it
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
		if fa.Kind() == reflect.Map {
			// a missing env is the same as an empty one
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
	// stopTimeout is config.StopTimeout, for main to read on shutdown
	stopTimeout atomic.Int64
}

func newAppRunner(config *Config, procs *processSet) *appRunner {
	r := &appRunner{
		config:   config,
		clients:  newClients(config),
		procs:    procs,
		apps:     map[int]*appHandle{},
		monitors: newAppList(),
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
	h := &appHandle{
		mon:     newAppMonitor(app),
		updates: make(chan appUpdate, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	r.apps[app.ExternalPort] = h
	logf("[MAIN] Starting app %s:%d\n", app.Repo, app.ExternalPort)
	// the loop gets later configs through h.updates only
	config, clients := r.config, r.clients
	go func() {
		defer close(h.done)
		runApp(app, config, clients, r.procs, h.mon, h.updates, h.stop)
	}()
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

	for _, app := range next.Apps {
		h, ok := r.apps[app.ExternalPort]
		if !ok {
			r.start(app)
			continue
		}
		// only the latest update matters, and the loop may be busy deploying,
		// so replace an update it has not taken yet rather than wait
		select {
		case <-h.updates:
		default:
		}
		h.updates <- appUpdate{app: app, config: next, clients: r.clients}
	}
	r.showApps()
}

// watchConfig reads config.json every configReloadInterval and applies it
// when its content changed, until ctx is done. configFile is the content
// runner started with.
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
		next := w.check()
		if next == nil || ctx.Err() != nil {
			continue
		}
		r.apply(next)

		if d := time.Duration(next.ConfigReloadInterval); d != interval {
			if d == 0 {
				logf("[MAIN] configReloadInterval is 0, no longer reloading config.json until runner restarts\n")
				return
			}
			interval = d
			ticker.Reset(d)
		}
	}
}
