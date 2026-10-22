package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

// retryAfterSeconds is the Retry-After sent while no version is deployed yet.
const retryAfterSeconds = "10"

// DynamicProxy holds the state of our active backend routing target.
type DynamicProxy struct {
	mu           sync.RWMutex
	currentPort  int
	currentProxy *httputil.ReverseProxy
}

// New returns a proxy without a target; it answers 503 until the first
// UpdateTarget.
func New() *DynamicProxy {
	return &DynamicProxy{}
}

// UpdateTarget safely switches all incoming proxy traffic to a new destination port.
func (dp *DynamicProxy) UpdateTarget(newPort int) error {
	// Parse the target URL string; 127.0.0.1 rather than localhost, which may
	// resolve to ::1 where the port can belong to another program
	targetURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", newPort))
	if err != nil {
		return fmt.Errorf("invalid target port/url: %w", err)
	}

	// Create Go's native reverse proxy handler
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Write Lock: Safely hot-swap the pointer to the proxy backend
	dp.mu.Lock()
	dp.currentPort = newPort
	dp.currentProxy = proxy
	dp.mu.Unlock()

	return nil
}

// ServeHTTP makes our struct implement the standard http.Handler interface.
func (dp *DynamicProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read Lock: Allow concurrent requests through, but block if an UpdateTarget is in progress
	dp.mu.RLock()
	proxy := dp.currentProxy
	dp.mu.RUnlock()

	if proxy == nil {
		w.Header().Set("Retry-After", retryAfterSeconds)
		http.Error(w, "no version deployed yet", http.StatusServiceUnavailable)
		return
	}

	// Forward the request to the active backend process
	proxy.ServeHTTP(w, r)
}
