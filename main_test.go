package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunAppReturnsWhenExternalPortIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	// an API that always fails, so runApp never deploys anything if it does not return
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer api.Close()

	config := &Config{
		GithubAPI:           api.URL,
		PollInterval:        Duration(10 * time.Millisecond),
		HealthCheckInterval: Duration(10 * time.Millisecond),
		APITimeout:          Duration(time.Second),
		DownloadTimeout:     Duration(time.Second),
		HealthCheckTimeout:  Duration(time.Second),
		StopTimeout:         Duration(time.Second),
	}
	app := App{ExternalPort: taken.Addr().(*net.TCPAddr).Port, Repo: "test/app", BinName: "app"}

	done := make(chan struct{})
	go func() {
		runApp(app, config, newClients(config), newProcessSet(), newAppMonitor(app))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runApp kept running although external port %d is taken", app.ExternalPort)
	}
}

func TestFreePortCanBeBound(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if port <= 0 {
		t.Fatalf("freePort returned port %d", port)
	}

	// apps bind the port on all interfaces, e.g. ":<port>"
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("port %d from freePort cannot be bound: %v", port, err)
	}
	listener.Close()
}

func TestValidateConfigRejectsDuplicateExternalPorts(t *testing.T) {
	config := &Config{
		PollInterval:        Duration(time.Second),
		HealthCheckInterval: Duration(time.Second),
		APITimeout:          Duration(time.Second),
		DownloadTimeout:     Duration(time.Second),
		HealthCheckTimeout:  Duration(time.Second),
		StopTimeout:         Duration(time.Second),
		RestartBackoff:      Duration(time.Second),
		RestartBackoffMax:   Duration(time.Minute),
		Apps: []App{
			{ExternalPort: 8000, Repo: "owner/a"},
			{ExternalPort: 8001, Repo: "owner/b"},
			{ExternalPort: 8000, Repo: "owner/c"},
		},
	}

	err := validateConfig(config)
	if err == nil {
		t.Fatal("validateConfig accepted two apps on externalPort 8000")
	}
	want := "apps[0] (owner/a) and apps[2] (owner/c) both use externalPort 8000"
	if err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}

	config.Apps[2].ExternalPort = 8002
	if err := validateConfig(config); err != nil {
		t.Errorf("validateConfig rejected unique external ports: %v", err)
	}
}

func TestRestartDelay(t *testing.T) {
	base, limit := time.Second, 10*time.Second
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for n, w := range want {
		if got := restartDelay(n, base, limit); got != w {
			t.Errorf("restartDelay(%d, %s, %s) = %s, want %s", n, base, limit, got, w)
		}
	}
	// many consecutive failures must not overflow past the cap
	if got := restartDelay(1000, base, limit); got != limit {
		t.Errorf("restartDelay(1000, %s, %s) = %s, want %s", base, limit, got, limit)
	}
}

func TestValidateConfigRestartBackoff(t *testing.T) {
	valid := Config{
		PollInterval:        Duration(time.Second),
		HealthCheckInterval: Duration(time.Second),
		APITimeout:          Duration(time.Second),
		DownloadTimeout:     Duration(time.Second),
		HealthCheckTimeout:  Duration(time.Second),
		StopTimeout:         Duration(time.Second),
		RestartBackoff:      Duration(time.Second),
		RestartBackoffMax:   Duration(time.Minute),
	}
	if err := validateConfig(&valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	for name, modify := range map[string]func(*Config){
		"zero restartBackoff":                 func(c *Config) { c.RestartBackoff = 0 },
		"negative restartBackoff":             func(c *Config) { c.RestartBackoff = Duration(-time.Second) },
		"restartBackoffMax below the backoff": func(c *Config) { c.RestartBackoffMax = Duration(500 * time.Millisecond) },
	} {
		config := valid
		modify(&config)
		if err := validateConfig(&config); err == nil {
			t.Errorf("%s: config accepted, want an error", name)
		}
	}
}

func TestValidateConfigAdmin(t *testing.T) {
	valid := Config{
		PollInterval:        Duration(time.Second),
		HealthCheckInterval: Duration(time.Second),
		APITimeout:          Duration(time.Second),
		DownloadTimeout:     Duration(time.Second),
		HealthCheckTimeout:  Duration(time.Second),
		StopTimeout:         Duration(time.Second),
		RestartBackoff:      Duration(time.Second),
		RestartBackoffMax:   Duration(time.Minute),
		Apps:                []App{{ExternalPort: 8000, Repo: "owner/a"}},
	}
	// no admin settings at all: the UI is disabled
	if err := validateConfig(&valid); err != nil {
		t.Fatalf("config without admin UI rejected: %v", err)
	}

	valid.AdminPort = 8080
	valid.AdminUser = "admin"
	valid.AdminPassword = "secret"
	if err := validateConfig(&valid); err != nil {
		t.Fatalf("valid admin config rejected: %v", err)
	}

	for name, tc := range map[string]struct {
		modify func(*Config)
		field  string
	}{
		"missing adminUser":        {func(c *Config) { c.AdminUser = "" }, "adminUser"},
		"missing adminPassword":    {func(c *Config) { c.AdminPassword = "" }, "adminPassword"},
		"negative adminPort":       {func(c *Config) { c.AdminPort = -1 }, "adminPort"},
		"adminPort out of range":   {func(c *Config) { c.AdminPort = 70000 }, "adminPort"},
		"adminPort used by an app": {func(c *Config) { c.AdminPort = 8000 }, "adminPort"},
	} {
		config := valid
		tc.modify(&config)
		err := validateConfig(&config)
		if err == nil {
			t.Errorf("%s: config accepted, want an error", name)
		} else if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error %q does not name %s", name, err, tc.field)
		}
	}

	// user and password are not required while the UI is disabled
	config := valid
	config.AdminPort = 0
	config.AdminPassword = ""
	if err := validateConfig(&config); err != nil {
		t.Errorf("disabled admin UI without a password rejected: %v", err)
	}
}
