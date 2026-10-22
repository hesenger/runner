package main

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// prefixWriter prints each line written to it to os.Stdout prefixed with the
// identifier. A trailing partial line is kept until the next newline or flush.
type prefixWriter struct {
	prefix string
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		fmt.Printf("[%s] %s\n", w.prefix, w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *prefixWriter) flush() {
	if len(w.buf) > 0 {
		fmt.Printf("[%s] %s\n", w.prefix, w.buf)
		w.buf = nil
	}
}

// Process is a running app binary. A single goroutine waits for it to exit,
// so it is always reaped, and logs its exit status.
type Process struct {
	identifier string
	cmd        *exec.Cmd
	// done is closed once the process has exited and been reaped.
	done chan struct{}
	// stopping is set by Stop, so an exit it causes is not logged as unexpected.
	stopping atomic.Bool
}

// StartProcess launches the binary and prefixes all its output lines.
func StartProcess(identifier string, binaryPath string, args ...string) (*Process, error) {
	// 1. Ensure executable permissions
	if err := os.Chmod(binaryPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to apply execute permissions: %w", err)
	}

	// 2. Send stdout and stderr through the same writer; Wait returns only
	// after all output has been copied
	out := &prefixWriter{prefix: identifier}
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	// don't let a left-over grandchild holding the output open block Wait
	cmd.WaitDelay = time.Second

	// 3. Start the background process
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start background process: %w", err)
	}

	// 4. Reap the process when it exits
	p := &Process{identifier: identifier, cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		out.flush()
		if p.stopping.Load() {
			fmt.Printf("[%s] Process %d stopped: %s\n", identifier, cmd.Process.Pid, cmd.ProcessState)
		} else {
			fmt.Printf("[%s] Process %d exited unexpectedly: %s\n", identifier, cmd.Process.Pid, cmd.ProcessState)
		}
		close(p.done)
	}()

	return p, nil
}

// Done is closed once the process has exited and been reaped.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Pid is the process ID.
func (p *Process) Pid() int {
	return p.cmd.Process.Pid
}

// Stopping reports whether Stop was called, so an exit is expected rather
// than a crash.
func (p *Process) Stopping() bool {
	return p.stopping.Load()
}

// Stop sends SIGTERM and kills the process if it has not exited within
// timeout. It returns once the process has been reaped.
func (p *Process) Stop(timeout time.Duration) {
	p.stopping.Store(true)
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		fmt.Printf("[%s] Failed to send SIGTERM to process %d: %v\n", p.identifier, p.cmd.Process.Pid, err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return
	case <-timer.C:
	}

	fmt.Printf("[%s] Process %d still running %s after SIGTERM, killing it\n", p.identifier, p.cmd.Process.Pid, timeout)
	p.cmd.Process.Kill()
	<-p.done
}

// processSet tracks every started process until it is reaped, so runner can
// stop them all when it shuts down.
type processSet struct {
	mu     sync.Mutex
	closed bool
	procs  map[*Process]struct{}
}

func newProcessSet() *processSet {
	return &processSet{procs: map[*Process]struct{}{}}
}

// start launches a process and tracks it; it fails once stopAll was called.
func (s *processSet) start(identifier string, binaryPath string, args ...string) (*Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("runner is shutting down")
	}

	p, err := StartProcess(identifier, binaryPath, args...)
	if err != nil {
		return nil, err
	}
	s.procs[p] = struct{}{}

	go func() {
		<-p.Done()
		s.mu.Lock()
		delete(s.procs, p)
		s.mu.Unlock()
	}()
	return p, nil
}

// stopAll stops every tracked process in parallel and prevents new ones from
// starting. It returns once all of them have been reaped.
func (s *processSet) stopAll(timeout time.Duration) {
	s.mu.Lock()
	s.closed = true
	procs := slices.Collect(maps.Keys(s.procs))
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Go(func() { p.Stop(timeout) })
	}
	wg.Wait()
}
