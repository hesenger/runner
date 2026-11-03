package main

import (
	"os"
	"strconv"
)

// processRSS returns the resident memory of pid alone, in KB, read from /proc.
func processRSS(pid int) (int64, bool) {
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	return parseVmRSS(string(status))
}

// processTreeRSS returns the resident memory of pid and all its descendants,
// in KB, read from /proc, so an app that forks workers is counted whole.
func processTreeRSS(pid int) (int64, bool) {
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			// the process exited meanwhile
			continue
		}
		if ppid, ok := parsePPID(string(stat)); ok {
			children[ppid] = append(children[ppid], p)
		}
	}

	var total int64
	found := false
	queue := []int{pid}
	for len(queue) > 0 {
		p := queue[0]
		queue = append(queue[1:], children[p]...)
		status, err := os.ReadFile("/proc/" + strconv.Itoa(p) + "/status")
		if err != nil {
			continue
		}
		if kb, ok := parseVmRSS(string(status)); ok {
			total += kb
			found = true
		}
	}
	return total, found
}
