package main

import (
	"slices"
	"sync"
	"time"
)

// App states shown by the admin UI.
const (
	// stateWaiting: no version deployed yet; the proxy answers 503
	stateWaiting = "waiting"
	stateRunning = "running"
	// stateCrashed: the running version exited unexpectedly and waits to be
	// restarted
	stateCrashed = "crashed"
	// stateRestarting: the running version is being restarted and health checked
	stateRestarting = "restarting"
	// stateFailed: the app cannot run at all, e.g. its external port is taken
	stateFailed = "failed"
	// stateStopped: runner stopped the app because it is shutting down, or
	// because it was removed from config.json
	stateStopped = "stopped"
)

// maxDeploys is how many recent deploys are kept per app.
const maxDeploys = 20

type deployRecord struct {
	ArtifactID int64     `json:"artifactId"`
	Time       time.Time `json:"time"`
	// Result is "success" or "failed", with the reason in Reason. A redeploy
	// of the running version for a change of its env or workDir has the
	// reason "env", "config" or "env and config", followed by why it failed.
	Result     string `json:"result"`
	Reason     string `json:"reason"`
	DurationMs int64  `json:"durationMs"`
}

// appStatus is what the admin UI shows of an app.
type appStatus struct {
	Name         string `json:"name"`
	ExternalPort int    `json:"externalPort"`
	State        string `json:"state"`
	// Error explains stateFailed.
	Error string `json:"error"`
	// DeployingID is the artifact being deployed right now, 0 if none.
	DeployingID int64 `json:"deployingArtifactId"`
	// The running version: its artifact, internal port and process.
	ArtifactID   int64     `json:"artifactId"`
	ArtifactName string    `json:"artifactName"`
	Branch       string    `json:"branch"`
	InternalPort int       `json:"internalPort"`
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"startedAt,omitzero"`
	// Restarts counts restart attempts since the running version was deployed.
	Restarts int `json:"restarts"`
	// MemoryKB is the resident memory of the running process and its
	// children, 0 when unknown: it is only read on Linux.
	MemoryKB int64 `json:"memoryKb"`
	// RequestsLastMinute counts the requests of the last full minute, and
	// RequestsTotal those since runner started.
	// WorkDirHash is the first 8 hex digits of the hash of the content of
	// workDir, empty without one; WorkDirChangedAt is when that content
	// last changed, and WorkDirError why workDir cannot be read.
	WorkDirHash        string         `json:"workDirHash"`
	WorkDirChangedAt   time.Time      `json:"workDirChangedAt,omitzero"`
	WorkDirError       string         `json:"workDirError"`
	RequestsLastMinute int64          `json:"requestsLastMinute"`
	RequestsTotal      int64          `json:"requestsTotal"`
	LastPoll           time.Time      `json:"lastPoll,omitzero"`
	LastPollError      string         `json:"lastPollError"`
	NextPoll           time.Time      `json:"nextPoll,omitzero"`
	FailedArtifactIDs  []int64        `json:"failedArtifactIds"`
	Deploys            []deployRecord `json:"deploys"` // newest first
}

// appMonitor holds an app's status, updated by its runApp loop, its output
// and its request metrics for the admin UI.
type appMonitor struct {
	app     App
	logs    *logBuffer
	metrics *recorder

	mu     sync.Mutex
	status appStatus
}

func newAppMonitor(app App) *appMonitor {
	m := &appMonitor{
		app:  app,
		logs: newLogBuffer(logLines),
		status: appStatus{
			Name:              app.Repo,
			ExternalPort:      app.ExternalPort,
			State:             stateWaiting,
			FailedArtifactIDs: []int64{},
			Deploys:           []deployRecord{},
		},
	}
	m.metrics = newRecorder(m)
	return m
}

// keepFiles keeps the app's output and metrics in its directory of dataDir,
// before its loop starts.
func (m *appMonitor) keepFiles(data dataSettings) {
	dir := appDataDir(data.dir, m.app)
	m.logs.file = newDailyFile(dir, "output", ".log", data.retentionDays, logf)
	m.logs.file.maxBytes = maxDailyFileBytes
	m.metrics.keepFiles(dir, data)
}

// update changes the status under the lock.
func (m *appMonitor) update(change func(s *appStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	change(&m.status)
}

// deployDone records the result of deploying artifact id, which started at
// started; an empty reason means it succeeded.
func (m *appMonitor) deployDone(id int64, started time.Time, reason string) {
	d := deployRecord{
		ArtifactID: id,
		Time:       started,
		Result:     "success",
		Reason:     reason,
		DurationMs: time.Since(started).Milliseconds(),
	}
	if reason != "" {
		d.Result = "failed"
	}
	m.update(func(s *appStatus) {
		s.DeployingID = 0
		s.Deploys = append([]deployRecord{d}, s.Deploys[:min(len(s.Deploys), maxDeploys-1)]...)
	})
}

// redeployDone records a redeploy of the running artifact id for a change
// of what ("env", "config" or both), which started at started; an empty
// reason means it succeeded.
func (m *appMonitor) redeployDone(id int64, started time.Time, what, reason string) {
	d := deployRecord{
		ArtifactID: id,
		Time:       started,
		Result:     "success",
		Reason:     what,
		DurationMs: time.Since(started).Milliseconds(),
	}
	if reason != "" {
		d.Result = "failed"
		d.Reason = what + ": " + reason
	}
	m.update(func(s *appStatus) {
		s.Deploys = append([]deployRecord{d}, s.Deploys[:min(len(s.Deploys), maxDeploys-1)]...)
	})
}

// snapshot returns a copy of the status that is safe to read without the lock.
func (m *appMonitor) snapshot() appStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	s.FailedArtifactIDs = slices.Clone(s.FailedArtifactIDs)
	s.Deploys = slices.Clone(s.Deploys)
	return s
}

// appList is the monitors of the apps runner runs, in config order, for the
// admin UI. A config reload replaces it while the UI reads it.
type appList struct {
	mu   sync.Mutex
	apps []*appMonitor
}

func newAppList(apps ...*appMonitor) *appList {
	return &appList{apps: apps}
}

// get returns the current monitors; the slice must not be modified.
func (l *appList) get() []*appMonitor {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.apps
}

// set replaces the monitors.
func (l *appList) set(apps []*appMonitor) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.apps = apps
}
