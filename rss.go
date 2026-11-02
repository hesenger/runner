package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// parseVmRSS returns the VmRSS of the content of /proc/<pid>/status, in KB.
// A zombie or kernel thread has none.
func parseVmRSS(status string) (int64, bool) {
	for line := range strings.Lines(status) {
		value, ok := strings.CutPrefix(line, "VmRSS:")
		if !ok {
			continue
		}
		// "VmRSS:	   12345 kB"
		kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
		return kb, err == nil
	}
	return 0, false
}

// parsePPID returns the parent pid from the content of /proc/<pid>/stat. The
// command name in parentheses may hold spaces and parentheses itself, so the
// fields are read after the last ')'.
func parsePPID(stat string) (int, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	// " S 1234 ..." is the state, then the parent pid
	fields := strings.Fields(stat[i+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, err == nil
}

// peakRSSKB is the peak resident memory of an exited process, in KB, or 0 if
// unknown. Maxrss is in KB on Linux but in bytes on macOS.
func peakRSSKB(state *os.ProcessState) int64 {
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || usage == nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(usage.Maxrss) / 1024
	}
	return int64(usage.Maxrss)
}

// formatKB formats a memory size in KB for the log, e.g. "12.3 MB".
func formatKB(kb int64) string {
	if kb < 1024 {
		return strconv.FormatInt(kb, 10) + " KB"
	}
	return strconv.FormatFloat(float64(kb)/1024, 'f', 1, 64) + " MB"
}
