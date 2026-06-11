package main

import (
	"fmt"
	"os"
	"os/exec"
)

// StartBinary launches the process in the background and returns the command pointer.
// Remember to call os.Chmod on the binary path before passing it here!
func StartBinary(binaryPath string, args ...string) (*exec.Cmd, error) {
	fi, err := os.Stat(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("file does not exist: %w", err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("PATH IS A DIRECTORY, NOT A BINARY: %s", binaryPath)
	}

	// 1. Ensure the executable bit is flipped (+x)
	if err := os.Chmod(binaryPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to apply execute permissions: %w", err)
	}

	// 2. Prepare the command
	cmd := exec.Command(binaryPath, args...)

	// Highly recommended: Connect the binary's output to your main program's terminal
	// so you can actually see logs or errors happening in the background.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 3. Start the process asynchronously
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start background process: %w", err)
	}

	// Return the command handle so the caller can control it
	return cmd, nil
}
