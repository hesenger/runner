package main

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

//go:embed admin/index.html
var adminPage []byte

// logPingInterval is how often an idle log stream sends a comment, so a
// client that went away is noticed.
const logPingInterval = 15 * time.Second

// adminServer serves the read-only monitoring UI: the page, the status of
// every app as JSON, and live logs as Server-Sent Events.
type adminServer struct {
	user, password string
	started        time.Time
	runnerLogs     *logBuffer
	apps           []*appMonitor
}

// startAdmin serves the admin UI on config.AdminPort. A port that cannot be
// bound is logged and leaves runner without the UI, like a taken external
// port only stops its app. It returns the server to close on shutdown, or nil.
func startAdmin(config *Config, runnerLogs *logBuffer, apps []*appMonitor) *http.Server {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", config.AdminPort))
	if err != nil {
		logf("[MAIN] Failed to listen on admin port %d, running without the admin UI: %v\n", config.AdminPort, err)
		return nil
	}

	a := &adminServer{
		user:       config.AdminUser,
		password:   config.AdminPassword,
		started:    time.Now(),
		runnerLogs: runnerLogs,
		apps:       apps,
	}
	srv := &http.Server{
		Handler:           a.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// /logs lifts the write timeout for its stream
		WriteTimeout:   10 * time.Second,
		IdleTimeout:    time.Minute,
		MaxHeaderBytes: 16 << 10,
	}
	go func() {
		err := srv.Serve(listener)
		if err != http.ErrServerClosed {
			logf("[MAIN] Admin UI stopped: %v\n", err)
		}
	}()
	logf("[MAIN] Admin UI on :%d (plain HTTP, basic auth)\n", config.AdminPort)
	return srv
}

func (a *adminServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("GET /status", a.status)
	mux.HandleFunc("GET /logs", a.logs)
	return a.requireAuth(mux)
}

// requireAuth answers 401 to any request without the admin user and password.
func (a *adminServer) requireAuth(next http.Handler) http.Handler {
	// compare fixed-size hashes, so the time taken does not reveal the length
	wantUser := sha256.Sum256([]byte(a.user))
	wantPassword := sha256.Sum256([]byte(a.password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		gotUser := sha256.Sum256([]byte(user))
		gotPassword := sha256.Sum256([]byte(password))
		// both are always compared, so the time taken does not reveal which one is wrong
		userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
		passwordOK := subtle.ConstantTimeCompare(gotPassword[:], wantPassword[:])
		if !ok || userOK&passwordOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="runner"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *adminServer) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(adminPage)
}

type runnerStatus struct {
	Version   string      `json:"version"`
	StartedAt time.Time   `json:"startedAt"`
	Now       time.Time   `json:"now"`
	Apps      []appStatus `json:"apps"`
}

func (a *adminServer) status(w http.ResponseWriter, r *http.Request) {
	status := runnerStatus{
		Version:   version,
		StartedAt: a.started,
		Now:       time.Now(),
		Apps:      make([]appStatus, len(a.apps)),
	}
	for i, m := range a.apps {
		status.Apps[i] = m.snapshot()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(status)
}

// logs streams a log as Server-Sent Events: the buffered lines first, then
// each new line, as JSON {"time", "text"}. ?app=<externalPort> picks an app's
// output (the repo is not unique across apps); empty or "runner" picks
// runner's own log. A client that falls behind is disconnected; the browser
// reconnects and gets the buffered lines again.
func (a *adminServer) logs(w http.ResponseWriter, r *http.Request) {
	logs := a.runnerLogs
	if name := r.URL.Query().Get("app"); name != "" && name != "runner" {
		logs = nil
		for _, m := range a.apps {
			if strconv.Itoa(m.app.ExternalPort) == name {
				logs = m.logs
			}
		}
		if logs == nil {
			http.Error(w, "unknown app, use its externalPort or runner", http.StatusNotFound)
			return
		}
	}

	// the stream lasts as long as the client stays
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{})

	replay, lines, cancel := logs.subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	send := func(l logLine) error {
		data, _ := json.Marshal(l)
		_, err := fmt.Fprintf(w, "data: %s\n\n", data)
		return err
	}
	for _, l := range replay {
		if send(l) != nil {
			return
		}
	}
	if rc.Flush() != nil {
		return
	}

	ping := time.NewTicker(logPingInterval)
	defer ping.Stop()
	for {
		select {
		case l, ok := <-lines:
			if !ok || send(l) != nil {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
		if rc.Flush() != nil {
			return
		}
	}
}
