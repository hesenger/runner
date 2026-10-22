package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/hesenger/runner/proxy"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// logOutput receives runner's own log lines. main also sends them to the
// admin UI's runner log.
var logOutput io.Writer = os.Stdout

// logf prints one of runner's own log lines.
func logf(format string, args ...any) {
	fmt.Fprintf(logOutput, format, args...)
}

type App struct {
	ExternalPort int    `json:"externalPort"`
	Repo         string `json:"repo"`
	Token        string `json:"token"`
	BinName      string `json:"binName"`
	// Branch, when set, only deploys artifacts built from this branch.
	Branch string `json:"branch"`
	// ArtifactName, when set, only deploys artifacts with this name.
	ArtifactName string `json:"artifactName"`
}

type Config struct {
	// GithubAPI is the base URL of the GitHub REST API, overridable for testing.
	GithubAPI string `json:"githubApi"`
	// PollInterval is how long to wait between checks for a new artifact.
	PollInterval Duration `json:"pollInterval"`
	// HealthCheckInterval is how long to wait before each health check attempt.
	HealthCheckInterval Duration `json:"healthCheckInterval"`
	// DrainDelay is how long the old process keeps running after traffic is switched.
	DrainDelay Duration `json:"drainDelay"`
	// APITimeout bounds each GitHub API request, and how long a download may
	// wait for response headers.
	APITimeout Duration `json:"apiTimeout"`
	// DownloadTimeout bounds each artifact download, including the body.
	DownloadTimeout Duration `json:"downloadTimeout"`
	// HealthCheckTimeout bounds each health check attempt.
	HealthCheckTimeout Duration `json:"healthCheckTimeout"`
	// StopTimeout is how long a process gets to exit after SIGTERM before it
	// is killed.
	StopTimeout Duration `json:"stopTimeout"`
	// RestartBackoff is the wait before restarting a running version that
	// exited unexpectedly; it doubles for each consecutive failed restart.
	RestartBackoff Duration `json:"restartBackoff"`
	// RestartBackoffMax caps RestartBackoff. A version that stays up this long
	// after a restart starts over from RestartBackoff when it exits again.
	RestartBackoffMax Duration `json:"restartBackoffMax"`
	// AdminPort, when not 0, serves the read-only monitoring UI on this port
	// over plain HTTP, behind basic auth with AdminUser and AdminPassword.
	AdminPort     int    `json:"adminPort"`
	AdminUser     string `json:"adminUser"`
	AdminPassword string `json:"adminPassword"`
	Apps          []App  `json:"apps"`
}

// Duration is a time.Duration read from a JSON string such as "10s" or "500ms".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func readConfig() (*Config, error) {
	configFile, err := os.ReadFile("config.json")
	if err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}

	// defaults, kept for any field missing from config.json
	config := &Config{
		GithubAPI:           "https://api.github.com",
		PollInterval:        Duration(10 * time.Second),
		HealthCheckInterval: Duration(10 * time.Second),
		DrainDelay:          Duration(10 * time.Second),
		APITimeout:          Duration(30 * time.Second),
		DownloadTimeout:     Duration(10 * time.Minute),
		HealthCheckTimeout:  Duration(5 * time.Second),
		StopTimeout:         Duration(10 * time.Second),
		RestartBackoff:      Duration(time.Second),
		RestartBackoffMax:   Duration(time.Minute),
	}
	decoder := json.NewDecoder(bytes.NewReader(configFile))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(config); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}

	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	return config, nil
}

func validateConfig(config *Config) error {
	if config.PollInterval <= 0 {
		return fmt.Errorf("pollInterval must be greater than 0, got %q", time.Duration(config.PollInterval).String())
	}
	if config.HealthCheckInterval <= 0 {
		return fmt.Errorf("healthCheckInterval must be greater than 0, got %q", time.Duration(config.HealthCheckInterval).String())
	}
	if config.DrainDelay < 0 {
		return fmt.Errorf("drainDelay must not be negative, got %q", time.Duration(config.DrainDelay).String())
	}
	if config.APITimeout <= 0 {
		return fmt.Errorf("apiTimeout must be greater than 0, got %q", time.Duration(config.APITimeout).String())
	}
	if config.DownloadTimeout <= 0 {
		return fmt.Errorf("downloadTimeout must be greater than 0, got %q", time.Duration(config.DownloadTimeout).String())
	}
	if config.HealthCheckTimeout <= 0 {
		return fmt.Errorf("healthCheckTimeout must be greater than 0, got %q", time.Duration(config.HealthCheckTimeout).String())
	}
	if config.StopTimeout <= 0 {
		return fmt.Errorf("stopTimeout must be greater than 0, got %q", time.Duration(config.StopTimeout).String())
	}
	if config.RestartBackoff <= 0 {
		return fmt.Errorf("restartBackoff must be greater than 0, got %q", time.Duration(config.RestartBackoff).String())
	}
	if config.RestartBackoffMax < config.RestartBackoff {
		return fmt.Errorf("restartBackoffMax must not be less than restartBackoff (%s), got %q",
			time.Duration(config.RestartBackoff), time.Duration(config.RestartBackoffMax).String())
	}
	if config.AdminPort < 0 || config.AdminPort > 65535 {
		return fmt.Errorf("adminPort must be between 1 and 65535, or 0 to disable the admin UI, got %d", config.AdminPort)
	}
	if config.AdminPort != 0 {
		if config.AdminUser == "" {
			return fmt.Errorf("adminUser is required when adminPort is set")
		}
		if config.AdminPassword == "" {
			return fmt.Errorf("adminPassword is required when adminPort is set")
		}
		for i, app := range config.Apps {
			if app.ExternalPort == config.AdminPort {
				return fmt.Errorf("adminPort %d is also the externalPort of apps[%d] (%s)", config.AdminPort, i, app.Repo)
			}
		}
	}
	// two apps cannot share an external port
	seen := map[int]int{}
	for i, app := range config.Apps {
		if j, ok := seen[app.ExternalPort]; ok {
			return fmt.Errorf("apps[%d] (%s) and apps[%d] (%s) both use externalPort %d",
				j, config.Apps[j].Repo, i, app.Repo, app.ExternalPort)
		}
		seen[app.ExternalPort] = i
	}
	return nil
}

func main() {
	flag.Usage = func() {
		fmt.Println("Runner is a reverse proxy that keeps your most recent version with zero downtime.")
		flag.PrintDefaults()
	}
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		flag.Usage()
		os.Exit(0)
	}
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	// keep the last lines of runner's own log for the admin UI
	runnerLogs := newLogBuffer(logLines)
	logOutput = io.MultiWriter(os.Stdout, runnerLogs)
	log.SetOutput(io.MultiWriter(os.Stderr, runnerLogs))

	config, err := readConfig()
	if err != nil {
		log.Fatalf("[MAIN]Failed to read config: %v", err)
	}

	// ensures bin and tmp directories exist
	os.MkdirAll("bin", 0755)
	os.MkdirAll("tmp", 0755)

	logf("[MAIN] Runner %s\n", version)
	logf("[MAIN] GitHub API %s, poll every %s, health check every %s, drain delay %s\n",
		config.GithubAPI, time.Duration(config.PollInterval),
		time.Duration(config.HealthCheckInterval), time.Duration(config.DrainDelay))
	logf("[MAIN] Timeouts: API %s, download %s, health check %s, stop %s\n",
		time.Duration(config.APITimeout), time.Duration(config.DownloadTimeout),
		time.Duration(config.HealthCheckTimeout), time.Duration(config.StopTimeout))
	logf("[MAIN] Restart backoff %s, up to %s\n",
		time.Duration(config.RestartBackoff), time.Duration(config.RestartBackoffMax))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clients := newClients(config)
	procs := newProcessSet()

	monitors := make([]*appMonitor, len(config.Apps))
	for i, app := range config.Apps {
		monitors[i] = newAppMonitor(app)
	}

	var admin *http.Server
	if config.AdminPort != 0 {
		admin = startAdmin(config, runnerLogs, monitors)
	}

	for i, app := range config.Apps {
		logf("[MAIN] Starting app %s:%d\n", app.Repo, app.ExternalPort)
		go runApp(app, config, clients, procs, monitors[i])
	}

	<-ctx.Done()
	// a second signal terminates runner right away
	stop()
	logf("[MAIN] Shutting down, stopping all app processes\n")
	if admin != nil {
		admin.Close()
	}
	procs.stopAll(time.Duration(config.StopTimeout))
	logf("[MAIN] All app processes stopped\n")
}

// runApp deploys new versions of app and keeps the running one up. All its
// state is owned by this goroutine: a single loop waits for the next poll, an
// unexpected exit of the running process, or a pending restart of it, and
// handles one at a time. It reports its state to mon, and the output of its
// processes to mon.logs.
func runApp(app App, config *Config, clients *Clients, procs *processSet, mon *appMonitor) {
	pollInterval := time.Duration(config.PollInterval)
	restartBackoff := time.Duration(config.RestartBackoff)
	restartBackoffMax := time.Duration(config.RestartBackoffMax)

	runningId := int64(0)
	var runningProc *Process
	var runningPath string
	// runningPort is the internal port the running version listens on
	var runningPort int
	// runningSince is when runningProc started serving
	var runningSince time.Time
	// restarts counts consecutive restarts of the running version; it is reset
	// by a new deploy, or when the version stayed up for restartBackoffMax
	restarts := 0
	// restartTimer is set while the running version is down, waiting to be
	// restarted
	var restartTimer *time.Timer
	// artifacts that failed to extract, start or pass their health check are
	// not deployed again; download failures may be transient and are retried
	failedIds := map[int64]bool{}
	markFailed := func(id int64) {
		failedIds[id] = true
		mon.update(func(s *appStatus) { s.FailedArtifactIDs = append(s.FailedArtifactIDs, id) })
		logf("[%s] Artifact %d failed, skipping it until a newer artifact is published\n", app.Repo, id)
	}

	// answers 503 until the first version is deployed
	proxy := proxy.New()

	// bind before polling so a taken port stops this app right away; other
	// apps keep running
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", app.ExternalPort))
	if err != nil {
		logf("[%s] Failed to listen on external port %d, not running this app: %v\n", app.Repo, app.ExternalPort, err)
		mon.update(func(s *appStatus) {
			s.State = stateFailed
			s.Error = fmt.Sprintf("failed to listen on external port %d: %v", app.ExternalPort, err)
		})
		return
	}
	go func() {
		err := http.Serve(listener, proxy)
		logf("[%s] Proxy on external port %d stopped: %v\n", app.Repo, app.ExternalPort, err)
	}()

	// deployLatest deploys the latest artifact if it is new
	deployLatest := func() {
		artifact, err := FindLatestArtifact(clients.API, config.GithubAPI, app.Repo, app.Token, app.ArtifactName, app.Branch)
		mon.update(func(s *appStatus) {
			s.LastPoll = time.Now()
			s.LastPollError = ""
			if err != nil {
				s.LastPollError = err.Error()
			}
		})
		if err != nil {
			logf("[%s] Failed to find latest artifact: %v\n", app.Repo, err)
			return
		}

		// only download when there is a new version to deploy
		if artifact.ID == runningId || failedIds[artifact.ID] {
			return
		}
		id := artifact.ID

		// reason is why the deploy failed, recorded when it returns
		started := time.Now()
		reason := ""
		mon.update(func(s *appStatus) { s.DeployingID = id })
		defer func() { mon.deployDone(id, started, reason) }()

		logf("[%s] Downloading artifact %s (%d)\n", app.Repo, artifact.Name, id)
		path, err := DownloadArtifact(clients.Download, *artifact, app.Token, "tmp")
		if err != nil {
			logf("[%s] Failed to download artifact: %v\n", app.Repo, err)
			reason = fmt.Sprintf("download: %v", err)
			return
		}

		// decompress zip to bin/{artifact}
		path, err = DecompressArtifact(path, "bin")
		if err != nil {
			logf("[%s] Failed to decompress zip file: %v\n", app.Repo, err)
			reason = fmt.Sprintf("extract: %v", err)
			markFailed(id)
			return
		}

		logf("[%s] Decompressed artifact to %s\n", app.Repo, path)

		// start process on a port the OS reports free; not finding one is
		// not the artifact's fault, so it is retried on the next poll
		internalPort, err := freePort()
		if err != nil {
			logf("[%s] Failed to find a free internal port: %v\n", app.Repo, err)
			reason = fmt.Sprintf("no free internal port: %v", err)
			os.RemoveAll(path)
			return
		}

		binPath := filepath.Join(path, app.BinName)
		logf("[%s] Starting process on port %d\n", app.Repo, internalPort)

		proc, err := procs.start(app.Repo, mon.logs, binPath, "--port", strconv.Itoa(internalPort))
		if err != nil {
			logf("[%s] %v\n", app.Repo, err)
			reason = fmt.Sprintf("start: %v", err)
			os.RemoveAll(path)
			markFailed(id)
			return
		}

		err = checkNewVersionIsUp(clients.Health, app.Repo, internalPort, time.Duration(config.HealthCheckInterval))
		if err != nil {
			logf("[%s] Failed health check: %v\n", app.Repo, err)
			reason = fmt.Sprintf("health check: %v", err)
			proc.Stop(time.Duration(config.StopTimeout))
			os.RemoveAll(path)
			markFailed(id)
			return
		}

		logf("[%s] New version is up on port %d\n", app.Repo, internalPort)

		runningId = id
		runningPort = internalPort
		proxy.UpdateTarget(internalPort)
		mon.update(func(s *appStatus) {
			s.State = stateRunning
			s.ArtifactID = id
			s.ArtifactName = artifact.Name
			s.Branch = artifact.WorkflowRun.HeadBranch
			s.InternalPort = internalPort
			s.PID = proc.Pid()
			s.StartedAt = time.Now()
			s.Restarts = 0
		})

		logf("[%s] Switched proxy target to port %d, cleaning up old process\n", app.Repo, internalPort)

		// a newer version replaces a crashed one waiting to be restarted
		if restartTimer != nil {
			restartTimer.Stop()
			restartTimer = nil
		}
		restarts = 0

		if runningProc != nil {
			// wait some time after re-routing traffic so pending requests complete
			time.Sleep(time.Duration(config.DrainDelay))
			runningProc.Stop(time.Duration(config.StopTimeout))
		}
		runningProc = proc
		runningSince = time.Now()

		if runningPath != "" {
			os.RemoveAll(runningPath)
		}
		runningPath = path
	}

	// restartRunning starts the running version again from its extracted
	// binary, on its port when that is still free, and health checks it
	restartRunning := func() (*Process, int, error) {
		port := runningPort
		if !portFree(port) {
			var err error
			if port, err = freePort(); err != nil {
				return nil, 0, fmt.Errorf("failed to find a free internal port: %w", err)
			}
			logf("[%s] Port %d is taken, restarting on port %d instead\n", app.Repo, runningPort, port)
		}

		binPath := filepath.Join(runningPath, app.BinName)
		logf("[%s] Restarting artifact %d on port %d\n", app.Repo, runningId, port)

		proc, err := procs.start(app.Repo, mon.logs, binPath, "--port", strconv.Itoa(port))
		if err != nil {
			return nil, 0, err
		}

		err = checkNewVersionIsUp(clients.Health, app.Repo, port, time.Duration(config.HealthCheckInterval))
		if err != nil {
			proc.Stop(time.Duration(config.StopTimeout))
			return nil, 0, fmt.Errorf("failed health check: %w", err)
		}
		return proc, port, nil
	}

	poll := time.NewTimer(0)
	for {
		// watch the running process, or wait to restart it once it is down
		var exited <-chan struct{}
		var restartDue <-chan time.Time
		if restartTimer != nil {
			restartDue = restartTimer.C
		} else if runningProc != nil {
			exited = runningProc.Done()
		}

		select {
		case <-poll.C:
			deployLatest()
			poll.Reset(pollInterval)
			mon.update(func(s *appStatus) { s.NextPoll = time.Now().Add(pollInterval) })

		case <-exited:
			// the running process is only stopped when runner shuts down
			if runningProc.Stopping() {
				mon.update(func(s *appStatus) { s.State = stateStopped })
				return
			}
			if time.Since(runningSince) >= restartBackoffMax {
				restarts = 0
			}
			delay := restartDelay(restarts, restartBackoff, restartBackoffMax)
			restarts++
			logf("[%s] Process %d (artifact %d) exited unexpectedly, restarting in %s\n",
				app.Repo, runningProc.Pid(), runningId, delay)
			restartTimer = time.NewTimer(delay)
			mon.update(func(s *appStatus) { s.State = stateCrashed })

		case <-restartDue:
			restartTimer = nil
			mon.update(func(s *appStatus) {
				s.State = stateRestarting
				s.Restarts++
			})
			proc, port, err := restartRunning()
			if err != nil {
				// it is the only version known to work, so keep trying
				delay := restartDelay(restarts, restartBackoff, restartBackoffMax)
				restarts++
				logf("[%s] Failed to restart artifact %d: %v, retrying in %s\n", app.Repo, runningId, err, delay)
				restartTimer = time.NewTimer(delay)
				mon.update(func(s *appStatus) { s.State = stateCrashed })
				continue
			}

			runningProc = proc
			runningSince = time.Now()
			if port != runningPort {
				runningPort = port
				proxy.UpdateTarget(runningPort)
			}
			mon.update(func(s *appStatus) {
				s.State = stateRunning
				s.InternalPort = runningPort
				s.PID = proc.Pid()
				s.StartedAt = runningSince
			})
			logf("[%s] Restarted artifact %d on port %d\n", app.Repo, runningId, runningPort)
		}
	}
}

// freePort asks the OS for a free port on 127.0.0.1 and releases it, so an app
// process can bind it. Another program may take the port between the release
// and the app's bind; the app then fails to bind or another server answers,
// and the health check rejects the version.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func checkNewVersionIsUp(client *http.Client, prefix string, port int, interval time.Duration) error {
	for i := 0; i < 5; i++ {
		time.Sleep(interval)

		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err != nil {
			logf("[%s] Attempt %d - Failed to check health: %v\n", prefix, i, err)
			continue
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			logf("[%s] Attempt %d - Health check failed: %d\n", prefix, i, resp.StatusCode)
			continue
		}

		return nil
	}

	return fmt.Errorf("failed to start")
}

// restartDelay is the wait before restart attempt n (0 for the first restart
// after a crash): base doubled for each earlier attempt, capped at limit.
func restartDelay(n int, base, limit time.Duration) time.Duration {
	delay := base
	for i := 0; i < n && delay < limit; i++ {
		delay *= 2
	}
	return min(delay, limit)
}

// portFree reports whether a restarted app process can use port again: nothing
// answers on 127.0.0.1, where runner connects, and the port can be bound on all
// interfaces, like the app does. A 127.0.0.1 bind alone is not enough: on
// macOS it succeeds while another program holds the port on all interfaces.
func portFree(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err == nil {
		conn.Close()
		return false
	}
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	listener.Close()
	return true
}
