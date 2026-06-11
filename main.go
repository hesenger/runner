package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

type App struct {
	ExternalPort int    `json:"externalPort"`
	Repo         string `json:"repo"`
	ArtifactName string `json:"artifactName"`
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
	json.NewDecoder(bytes.NewReader(configFile)).Decode(&config)
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
	for {
		id, path, err := DownloadLatestArtifact(app.Repo, app.ArtifactName, app.Token, "tmp")
		if err != nil {
			fmt.Printf("[%s] Failed to download latest artifact: %v", app.Repo, err)
		}

		if id == runningId {
			fmt.Printf("[%s] No new version found, skipping (current: %d)\n", app.Repo, id)
			time.Sleep(10 * time.Second)
			continue
		}

		// decompress zip to bin/{artifact}
		path, err = DecompressArtifact(path, "bin")
		if err != nil {
			fmt.Printf("[%s] Failed to decompress zip file: %v", app.Repo, err)
		}

		fmt.Printf("[%s] Decompressed artifact to %s\n", app.Repo, path)

		// start process using internal port
		internalPort := currentPort + 1
		binPath := filepath.Join(path, app.BinName)
		fmt.Printf("[%s] Starting process on port %d\n", app.Repo, internalPort)

		cmd, err := StartBinaryWithPrefix(app.Repo, binPath, "--port", strconv.Itoa(internalPort))
		if err != nil {
			fmt.Printf("[%s] %v\n", app.Repo, err)
		}

		if runningCmd != nil {
			runningCmd.Process.Kill()
		}
		runningId = id
		runningCmd = cmd
		currentPort = internalPort

		time.Sleep(10 * time.Second)
	}
}
