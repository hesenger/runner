//go:build !linux

package main

// processRSS is only implemented on Linux, which has /proc.
func processRSS(pid int) (int64, bool) {
	return 0, false
}

// processTreeRSS is only implemented on Linux, which has /proc.
func processTreeRSS(pid int) (int64, bool) {
	return 0, false
}
