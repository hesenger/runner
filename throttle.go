package main

import (
	"net/netip"
	"sync"
	"time"
)

// Failed admin logins are throttled per client address, so the admin password
// cannot be guessed at network speed.
const (
	// freeFailures is how many failed logins a key gets before it is blocked.
	freeFailures = 5
	// firstBlock is the block after the first failure past freeFailures; each
	// further failure doubles it, up to maxBlock.
	firstBlock = time.Second
	maxBlock   = 5 * time.Minute
	// forgetAfter is how long after its last failure a key starts over.
	forgetAfter = 15 * time.Minute
	// maxThrottleKeys caps the table at about 1 MB (1.2 MB measured with IPv6
	// keys, the longest).
	maxThrottleKeys = 10000
	// sweepInterval is how often expired keys are pruned in passing;
	// fullSweepInterval is how often when the table is full.
	sweepInterval     = time.Minute
	fullSweepInterval = time.Second
)

type throttleEntry struct {
	// failures counts failed logins since the key was last forgotten or
	// logged in successfully.
	failures int
	last     time.Time
	// blockedUntil is when the key may try again.
	blockedUntil time.Time
}

// loginThrottle tracks failed admin logins per key (see throttleKey). The
// first freeFailures failures are free, each one after that blocks the key
// for a doubling time, and a successful login or forgetAfter without failures
// starts it over. A blocked key is refused without checking its password, and
// without sleeping, so a request never holds a goroutine for the block.
//
// Memory is bounded: the table holds at most maxThrottleKeys keys. Expired
// keys are pruned in passing. When the table is full of live keys, every key
// not in it shares a single overflow entry with the same policy. Under an
// attack from many addresses this throttles all new addresses together,
// legitimate ones included, until the table has room again; addresses already
// in the table keep their own entry. That is the price of a fixed memory
// bound, and it only matters while someone is guessing the password.
//
// It is safe for concurrent use. now is the clock, replaceable in tests.
type loginThrottle struct {
	now func() time.Time

	mu        sync.Mutex
	entries   map[string]*throttleEntry
	overflow  throttleEntry
	lastSweep time.Time
}

func newLoginThrottle(now func() time.Time) *loginThrottle {
	return &loginThrottle{now: now, entries: map[string]*throttleEntry{}, lastSweep: now()}
}

// throttleKey is the client address of a request's RemoteAddr: the full
// address for IPv4, and its /64 for IPv6, since one host commonly owns a whole
// /64 and can rotate through it. Headers such as X-Forwarded-For are never
// used: any client can set them.
func throttleKey(remoteAddr string) string {
	addrPort, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	addr := addrPort.Addr().Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	return netip.PrefixFrom(addr, 64).Masked().String()
}

// blocked reports whether key must be refused right now, and for how long.
func (t *loginThrottle) blocked(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e := t.lookup(key, now)
	if e == nil || !now.Before(e.blockedUntil) {
		return 0, false
	}
	return e.blockedUntil.Sub(now), true
}

// failure records a failed login of key. It reports whether the key is now
// blocked, and for how long.
func (t *loginThrottle) failure(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e := t.lookup(key, now)
	if e == nil {
		e = &throttleEntry{}
		t.entries[key] = e
	}
	if now.Sub(e.last) > forgetAfter {
		*e = throttleEntry{}
	}
	e.failures++
	e.last = now
	if e.failures <= freeFailures {
		return 0, false
	}

	block := firstBlock
	for i := freeFailures + 1; i < e.failures && block < maxBlock; i++ {
		block *= 2
	}
	block = min(block, maxBlock)
	e.blockedUntil = now.Add(block)

	// one line per block streak, so failures cannot flood the log
	if e.failures == freeFailures+1 {
		if e == &t.overflow {
			logf("[MAIN] Admin login throttle table is full (%d addresses); throttling all other addresses together after %d failed logins\n",
				len(t.entries), e.failures)
		} else {
			logf("[MAIN] Blocking admin logins from %s after %d failed logins\n", key, e.failures)
		}
	}
	return block, true
}

// success forgets key after a successful login. The overflow entry is not
// cleared: it is shared with addresses that may be guessing.
func (t *loginThrottle) success(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// size is how many keys the table holds.
func (t *loginThrottle) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// lookup returns the entry of key, the overflow entry when key is not in the
// full table, or nil when there is room for key. It prunes expired keys every
// sweepInterval, and every fullSweepInterval while the table is full.
// t.mu must be held.
func (t *loginThrottle) lookup(key string, now time.Time) *throttleEntry {
	if e, ok := t.entries[key]; ok {
		return e
	}
	full := len(t.entries) >= maxThrottleKeys
	since := now.Sub(t.lastSweep)
	if since >= sweepInterval || (full && since >= fullSweepInterval) {
		t.sweep(now)
		full = len(t.entries) >= maxThrottleKeys
	}
	if full {
		return &t.overflow
	}
	return nil
}

// sweep deletes keys whose last failure is older than forgetAfter; their
// block, at most maxBlock, is over too. t.mu must be held.
func (t *loginThrottle) sweep(now time.Time) {
	for key, e := range t.entries {
		if now.Sub(e.last) > forgetAfter {
			delete(t.entries, key)
		}
	}
	t.lastSweep = now
}
