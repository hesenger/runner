package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// pipeWithPrefix captures an io.ReadCloser stream and prints each line to os.Stdout
// prefixed with the identifier.
func pipeWithPrefix(prefix string, reader io.ReadCloser) {
	scanner := bufio.NewScanner(reader)

	// Read line-by-line until the process closes the stream
	for scanner.Scan() {
		fmt.Printf("[%s] %s\n", prefix, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		fmt.Printf("[%s] Error reading output stream: %v\n", prefix, err)
	}
}

// StartBinaryWithPrefix launches the process and prefixes all its output lines.
func StartBinaryWithPrefix(identifier string, binaryPath string, args ...string) (*exec.Cmd, error) {
	// 1. Ensure executable permissions
	if err := os.Chmod(binaryPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to apply execute permissions: %w", err)
	}

	cmd := exec.Command(binaryPath, args...)

	// 2. Grab the stdout and stderr pipe closures before running the command
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// 3. Start the background process
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start background process: %w", err)
	}

	// 4. Spin off concurrent background workers to handle log streaming
	go pipeWithPrefix(identifier, stdoutPipe)
	go pipeWithPrefix(identifier, stderrPipe)

	return cmd, nil
}
