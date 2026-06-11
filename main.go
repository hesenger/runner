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
	Apps []App `json:"apps"`
}

func readConfig() (*Config, error) {
	configFile, err := os.ReadFile("config.json")
	if err != nil {
		return nil, err
	}

	config := &Config{}
	json.NewDecoder(bytes.NewReader(configFile)).Decode(config)
	return config, nil
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

	for _, app := range config.Apps {
		fmt.Printf("[MAIN] Starting app %s:%d\n", app.Repo, app.ExternalPort)
		go runApp(app)
	}

	select {}
}

func runApp(app App) {
	startingPort := app.ExternalPort + 10
	currentPort := startingPort
	runningId := int64(0)
	var runningCmd *exec.Cmd

	proxy, err := proxy.New(currentPort)
	if err != nil {
		fmt.Printf("[%s] Failed to create proxy: %v\n", app.Repo, err)
		return
	}

	go http.ListenAndServe(fmt.Sprintf(":%d", app.ExternalPort), proxy)

	for {
		id, path, err := DownloadLatestArtifact(app.Repo, app.Token, "tmp")
		if err != nil {
			fmt.Printf("[%s] Failed to download latest artifact: %v\n", app.Repo, err)
			time.Sleep(10 * time.Second)
			continue
		}

		if id == runningId {
			time.Sleep(10 * time.Second)
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
		}

		err = checkNewVersionIsUp(app.Repo, internalPort)
		if err != nil {
			fmt.Printf("[%s] Failed health check: %v\n", app.Repo, err)
			cmd.Process.Kill()

			time.Sleep(10 * time.Second)
			continue
		}

		fmt.Printf("[%s] New version is up on port %d, killing old process\n", app.Repo, internalPort)

		if runningCmd != nil {
			runningCmd.Process.Kill()
		}
		runningId = id
		runningCmd = cmd
		currentPort = internalPort
		proxy.UpdateTarget(currentPort)
		fmt.Printf("[%s] Switched proxy target to port %d\n", app.Repo, currentPort)

		time.Sleep(10 * time.Second)
	}
}

func checkNewVersionIsUp(prefix string, port int) error {
	for i := 0; i < 5; i++ {
		time.Sleep(10 * time.Second)

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
