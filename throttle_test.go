package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a clock that only moves when told to.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newThrottledAdmin returns a test admin server whose throttle uses a fake clock.
func newThrottledAdmin() (*adminServer, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	a := newTestAdmin()
	a.throttle = newLoginThrottle(clock.now)
	return a, clock
}

// login sends GET /status from remoteAddr with the given password (none when
// empty) and returns the response.
func login(a *adminServer, remoteAddr, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/status", nil)
	req.RemoteAddr = remoteAddr
	if password != "" {
		req.SetBasicAuth("admin", password)
	}
	rec := httptest.NewRecorder()
	a.handler().ServeHTTP(rec, req)
	return rec
}

// failLogins sends n logins with a wrong password from remoteAddr and fails
// the test unless all of them answer want.
func failLogins(t *testing.T, a *adminServer, remoteAddr string, n, want int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if rec := login(a, remoteAddr, "wrong"); rec.Code != want {
			t.Fatalf("wrong login %d from %s answered %d, want %d", i, remoteAddr, rec.Code, want)
		}
	}
}

// captureLog sends runner's own log lines to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	old := logOutput
	logOutput = &buf
	t.Cleanup(func() { logOutput = old })
	return &buf
}

func TestThrottleBlocksAfterFreeFailures(t *testing.T) {
	logs := captureLog(t)
	a, clock := newThrottledAdmin()
	const ip = "203.0.113.7:50000"

	failLogins(t, a, ip, freeFailures, http.StatusUnauthorized)

	rec := login(a, ip, "wrong")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("6th wrong login answered %d with Retry-After %q, want 429 with 1",
			rec.Code, rec.Header().Get("Retry-After"))
	}
	// while blocked, the right password is not even checked
	if rec := login(a, ip, "secret"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("right password while blocked answered %d, want 429", rec.Code)
	}
	// another address is not affected
	if rec := login(a, "203.0.113.8:50000", "secret"); rec.Code != http.StatusOK {
		t.Fatalf("other address answered %d, want 200", rec.Code)
	}

	// each further failure doubles the block, up to maxBlock
	for _, want := range []string{"2", "4", "8"} {
		clock.advance(time.Minute)
		if rec := login(a, ip, "wrong"); rec.Header().Get("Retry-After") != want {
			t.Fatalf("Retry-After %q, want %s", rec.Header().Get("Retry-After"), want)
		}
	}
	for range 20 {
		clock.advance(maxBlock)
		login(a, ip, "wrong")
	}
	if wait, _ := a.throttle.blocked(throttleKey(ip)); wait != maxBlock {
		t.Fatalf("block of %s after many failures, want %s", wait, maxBlock)
	}

	// the block lifts once it is over, and the right password then works
	clock.advance(maxBlock)
	if rec := login(a, ip, "secret"); rec.Code != http.StatusOK {
		t.Fatalf("right password after the block answered %d, want 200", rec.Code)
	}

	// one log line for the whole block streak
	if n := strings.Count(logs.String(), "Blocking admin logins from 203.0.113.7"); n != 1 {
		t.Errorf("logged the block %d times, want once:\n%s", n, logs)
	}
}

func TestThrottleIgnoresRequestsWithoutCredentials(t *testing.T) {
	a, _ := newThrottledAdmin()
	// a browser's first request, before it shows the login prompt
	for range 2 * freeFailures {
		if rec := login(a, "203.0.113.7:50000", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("request without credentials answered %d, want 401", rec.Code)
		}
	}
	if n := a.throttle.size(); n != 0 {
		t.Fatalf("requests without credentials created %d throttle keys", n)
	}
}

func TestThrottleSuccessResetsKey(t *testing.T) {
	a, _ := newThrottledAdmin()
	const ip = "203.0.113.7:50000"
	failLogins(t, a, ip, freeFailures, http.StatusUnauthorized)
	if rec := login(a, ip, "secret"); rec.Code != http.StatusOK {
		t.Fatalf("right password answered %d, want 200", rec.Code)
	}
	if n := a.throttle.size(); n != 0 {
		t.Fatalf("throttle holds %d keys after a successful login", n)
	}
	// all the free failures are available again
	failLogins(t, a, ip, freeFailures, http.StatusUnauthorized)
}

func TestThrottleForgetsOldFailures(t *testing.T) {
	a, clock := newThrottledAdmin()
	const ip = "203.0.113.7:50000"
	failLogins(t, a, ip, freeFailures, http.StatusUnauthorized)
	failLogins(t, a, ip, 1, http.StatusTooManyRequests)

	// failures older than forgetAfter no longer count
	clock.advance(forgetAfter + time.Second)
	failLogins(t, a, ip, freeFailures, http.StatusUnauthorized)
	failLogins(t, a, ip, 1, http.StatusTooManyRequests)

	// and the periodic sweep drops the key
	clock.advance(forgetAfter + time.Second)
	a.throttle.blocked("198.51.100.1")
	if n := a.throttle.size(); n != 0 {
		t.Fatalf("throttle holds %d keys after they expired", n)
	}
}

func TestThrottleKey(t *testing.T) {
	for remoteAddr, want := range map[string]string{
		"203.0.113.7:50000":            "203.0.113.7",
		"[::ffff:203.0.113.7]:50000":   "203.0.113.7",
		"[2001:db8:1:2:aaaa::1]:50000": "2001:db8:1:2::/64",
		"[2001:db8:1:2:bbbb::9]:443":   "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:50000":      "2001:db8:1:3::/64",
		"[fe80::1%en0]:50000":          "fe80::/64",
		"not an address":               "not an address",
	} {
		if got := throttleKey(remoteAddr); got != want {
			t.Errorf("throttleKey(%q) = %q, want %q", remoteAddr, got, want)
		}
	}
}

func TestThrottleGroupsIPv6By64(t *testing.T) {
	a, _ := newThrottledAdmin()
	// a host rotating through its /64 shares one key
	for i := 1; i <= freeFailures; i++ {
		failLogins(t, a, fmt.Sprintf("[2001:db8:1:2::%x]:50000", i), 1, http.StatusUnauthorized)
	}
	failLogins(t, a, "[2001:db8:1:2:ffff::1]:50000", 1, http.StatusTooManyRequests)
	// the next /64 is another key
	if rec := login(a, "[2001:db8:1:3::1]:50000", "secret"); rec.Code != http.StatusOK {
		t.Fatalf("address in another /64 answered %d, want 200", rec.Code)
	}
}

func TestThrottleTableIsBounded(t *testing.T) {
	logs := captureLog(t)
	a, clock := newThrottledAdmin()
	ip := func(i int) string { return fmt.Sprintf("10.%d.%d.%d:50000", i>>16&255, i>>8&255, i&255) }

	// fill the table, one failure per address
	for i := range maxThrottleKeys {
		failLogins(t, a, ip(i), 1, http.StatusUnauthorized)
	}
	// more addresses than fit share the overflow entry
	for i := maxThrottleKeys; i < maxThrottleKeys+freeFailures; i++ {
		failLogins(t, a, ip(i), 1, http.StatusUnauthorized)
	}
	if n := a.throttle.size(); n > maxThrottleKeys {
		t.Fatalf("throttle holds %d keys, more than %d", n, maxThrottleKeys)
	}
	failLogins(t, a, ip(maxThrottleKeys+freeFailures), 1, http.StatusTooManyRequests)
	// any new address is throttled with them, even with the right password
	if rec := login(a, "192.0.2.1:50000", "secret"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("new address while the overflow is blocked answered %d, want 429", rec.Code)
	}
	// an address in the table keeps its own entry
	if rec := login(a, ip(0), "secret"); rec.Code != http.StatusOK {
		t.Fatalf("address in the table answered %d, want 200", rec.Code)
	}
	if n := a.throttle.size(); n > maxThrottleKeys {
		t.Fatalf("throttle holds %d keys, more than %d", n, maxThrottleKeys)
	}
	if n := strings.Count(logs.String(), "throttle table is full"); n != 1 {
		t.Errorf("logged the full table %d times, want once:\n%s", n, logs)
	}

	// once the entries expire, a sweep makes room for new addresses again
	clock.advance(forgetAfter + time.Second)
	if rec := login(a, "192.0.2.1:50000", "secret"); rec.Code != http.StatusOK {
		t.Fatalf("new address after the entries expired answered %d, want 200", rec.Code)
	}
	if n := a.throttle.size(); n != 0 {
		t.Fatalf("throttle holds %d keys after they expired", n)
	}
}

func TestThrottleConcurrentUse(t *testing.T) {
	a := newTestAdmin()
	handler := a.handler()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 50 {
				req := httptest.NewRequest("GET", "/status", nil)
				req.RemoteAddr = fmt.Sprintf("192.0.2.%d:50000", (w*50+i)%4)
				password := "wrong"
				if i%7 == 0 {
					password = "secret"
				}
				req.SetBasicAuth("admin", password)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				switch rec.Code {
				case http.StatusOK, http.StatusUnauthorized, http.StatusTooManyRequests:
				default:
					t.Errorf("answered %d", rec.Code)
				}
			}
		})
	}
	wg.Wait()
}
