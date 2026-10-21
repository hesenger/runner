package main

import (
	"net/http"
	"time"
)

// Clients holds the HTTP clients runner uses, each with its own timeout so a
// hung GitHub call or health check can never block an app's loop forever.
type Clients struct {
	// API lists artifacts on the GitHub API.
	API *http.Client
	// Download fetches artifact zips, which can be large, so its overall timeout
	// is long, but a server that never sends response headers fails fast.
	Download *http.Client
	// Health checks a new version's /health endpoint, per attempt.
	Health *http.Client
}

func newClients(config *Config) *Clients {
	apiTimeout := time.Duration(config.APITimeout)

	downloadTransport := http.DefaultTransport.(*http.Transport).Clone()
	downloadTransport.ResponseHeaderTimeout = apiTimeout

	return &Clients{
		API: &http.Client{Timeout: apiTimeout},
		Download: &http.Client{
			Timeout:   time.Duration(config.DownloadTimeout),
			Transport: downloadTransport,
		},
		Health: &http.Client{Timeout: time.Duration(config.HealthCheckTimeout)},
	}
}
