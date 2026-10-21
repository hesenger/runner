package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hesenger/runner/proxy"
)

type App struct {
	ExternalPort int    `json:"externalPort"`
	Repo         string `json:"repo"`
	Token        string `json:"token"`
	BinName      string `json:"binName"`
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
	Apps       []App    `json:"apps"`
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
	return nil
}

func main() {
	flag.Usage = func() {
		fmt.Println("Runner is a reverse proxy that keeps your most recent version with zero downtime.")
		flag.PrintDefaults()
	}
	flag.Parse()
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		flag.Usage()
		os.Exit(0)
	}

	config, err := readConfig()
	if err != nil {
		log.Fatalf("[MAIN]Failed to read config: %v", err)
	}

	// ensures bin and tmp directories exist
	os.MkdirAll("bin", 0755)
	os.MkdirAll("tmp", 0755)

	fmt.Printf("[MAIN] GitHub API %s, poll every %s, health check every %s, drain delay %s\n",
		config.GithubAPI, time.Duration(config.PollInterval),
		time.Duration(config.HealthCheckInterval), time.Duration(config.DrainDelay))

	for _, app := range config.Apps {
		fmt.Printf("[MAIN] Starting app %s:%d\n", app.Repo, app.ExternalPort)
		go runApp(app, config)
	}

	select {}
}

func runApp(app App, config *Config) {
	pollInterval := time.Duration(config.PollInterval)

	startingPort := app.ExternalPort + 10
	currentPort := startingPort
	runningId := int64(0)
	var runningCmd *exec.Cmd
	var runningPath string

	proxy, err := proxy.New(currentPort)
	if err != nil {
		fmt.Printf("[%s] Failed to create proxy: %v\n", app.Repo, err)
		return
	}

	go http.ListenAndServe(fmt.Sprintf(":%d", app.ExternalPort), proxy)

	for {
		id, path, err := DownloadLatestArtifact(config.GithubAPI, app.Repo, app.Token, "tmp")
		if err != nil {
			fmt.Printf("[%s] Failed to download latest artifact: %v\n", app.Repo, err)
			time.Sleep(pollInterval)
			continue
		}

		if id == runningId {
			time.Sleep(pollInterval)
			continue
		}

		// decompress zip to bin/{artifact}
		path, err = DecompressArtifact(path, "bin")
		if err != nil {
			fmt.Printf("[%s] Failed to decompress zip file: %v\n", app.Repo, err)
		}

		fmt.Printf("[%s] Decompressed artifact to %s\n", app.Repo, path)

		// start process using internal port
		internalPort := currentPort + 1
		if internalPort > startingPort+10 {
			internalPort = startingPort
		}

		binPath := filepath.Join(path, app.BinName)
		fmt.Printf("[%s] Starting process on port %d\n", app.Repo, internalPort)

		cmd, err := StartBinaryWithPrefix(app.Repo, binPath, "--port", strconv.Itoa(internalPort))
		if err != nil {
			fmt.Printf("[%s] %v\n", app.Repo, err)
			os.RemoveAll(path)

			time.Sleep(pollInterval)
			continue
		}

		err = checkNewVersionIsUp(app.Repo, internalPort, time.Duration(config.HealthCheckInterval))
		if err != nil {
			fmt.Printf("[%s] Failed health check: %v\n", app.Repo, err)
			cmd.Process.Kill()
			os.RemoveAll(path)

			time.Sleep(pollInterval)
			continue
		}

		fmt.Printf("[%s] New version is up on port %d\n", app.Repo, internalPort)

		runningId = id
		currentPort = internalPort
		proxy.UpdateTarget(currentPort)

		fmt.Printf("[%s] Switched proxy target to port %d, cleaning up old process\n", app.Repo, currentPort)

		if runningCmd != nil {
			// wait some time after re-routing traffic so pending requests complete
			time.Sleep(time.Duration(config.DrainDelay))
			runningCmd.Process.Kill()
		}
		runningCmd = cmd

		if runningPath != "" {
			os.RemoveAll(runningPath)
		}
		runningPath = path

		time.Sleep(pollInterval)
	}
}

func checkNewVersionIsUp(prefix string, port int, interval time.Duration) error {
	for i := 0; i < 5; i++ {
		time.Sleep(interval)

		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", port))
		if err != nil {
			fmt.Printf("[%s] Attempt %d - Failed to check health: %v\n", prefix, i, err)
			continue
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			fmt.Printf("[%s] Attempt %d - Health check failed: %d\n", prefix, i, resp.StatusCode)
			continue
		}

		return nil
	}

	return fmt.Errorf("failed to start")
}
