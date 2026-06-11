package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

type App struct {
	ExternalPort string `json:"externalPort"`
	Repo         string `json:"repo"`
	ArtifactName string `json:"artifactName"`
	BinName      string `json:"binName"`
	Token        string `json:"token"`
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
		fmt.Printf("[MAIN] Starting app %s: %s\n", app.Repo, app.ExternalPort)
		go runApp(app)
	}

	select {}
}

func runApp(app App) {
	// check if previous version was downloaded previously
	files, err := os.ReadDir("bin")
	if err != nil {
		fmt.Printf("[%s] Failed to read bin directory: %v", app.Repo, err)
	}

	path := ""
	for _, file := range files {
		if file.IsDir() && strings.HasPrefix(file.Name(), app.ArtifactName) {
			continue
		}
		fmt.Printf("[%s] Found artifact %s\n", app.Repo, file.Name())
		path = file.Name()
	}

	if path == "" {
		fmt.Printf("[%s] No previous version found, downloading latest artifact\n", app.Repo)
		path, err = DownloadLatestArtifact(app.Repo, app.ArtifactName, app.Token, "tmp")
		if err != nil {
			fmt.Printf("[%s] Failed to download latest artifact: %v", app.Repo, err)
		}
	}

	// decompress zip to bin/{artifact}
	path, err = DecompressArtifact(path, "bin")
	if err != nil {
		fmt.Printf("[%s] Failed to decompress zip file: %v", app.Repo, err)
	}

	fmt.Printf("[%s] Decompressed artifact to %s\n", app.Repo, path)
}
