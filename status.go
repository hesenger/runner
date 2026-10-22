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
	// stateStopped: runner stopped the app because it is shutting down
	stateStopped = "stopped"
)

// maxDeploys is how many recent deploys are kept per app.
const maxDeploys = 20

type deployRecord struct {
	ArtifactID int64     `json:"artifactId"`
	Time       time.Time `json:"time"`
	// Result is "success" or "failed", with the reason in Reason.
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
	Restarts          int            `json:"restarts"`
	LastPoll          time.Time      `json:"lastPoll,omitzero"`
	LastPollError     string         `json:"lastPollError"`
	NextPoll          time.Time      `json:"nextPoll,omitzero"`
	FailedArtifactIDs []int64        `json:"failedArtifactIds"`
	Deploys           []deployRecord `json:"deploys"` // newest first
}

// appMonitor holds an app's status, updated by its runApp loop, and its
// output for the admin UI.
type appMonitor struct {
	app  App
	logs *logBuffer

	mu     sync.Mutex
	status appStatus
}

func newAppMonitor(app App) *appMonitor {
	return &appMonitor{
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

// snapshot returns a copy of the status that is safe to read without the lock.
func (m *appMonitor) snapshot() appStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	s.FailedArtifactIDs = slices.Clone(s.FailedArtifactIDs)
	s.Deploys = slices.Clone(s.Deploys)
	return s
}
