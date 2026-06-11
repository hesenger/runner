package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

// DynamicProxy holds the state of our active backend routing target.
type DynamicProxy struct {
	mu           sync.RWMutex
	currentPort  int
	currentProxy *httputil.ReverseProxy
}

// NewDynamicProxy initializes the proxy with an initial target backend port.
func New(initialPort int) (*DynamicProxy, error) {
	dp := &DynamicProxy{}
	if err := dp.UpdateTarget(initialPort); err != nil {
		return nil, err
	}
	return dp, nil
}

// UpdateTarget safely switches all incoming proxy traffic to a new destination port.
func (dp *DynamicProxy) UpdateTarget(newPort int) error {
	// Parse the target URL string
	targetURL, err := url.Parse(fmt.Sprintf("http://localhost:%d", newPort))
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

	// Forward the request to the active backend process
	proxy.ServeHTTP(w, r)
}
