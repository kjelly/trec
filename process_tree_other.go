//go:build !linux

package main

import "os"

// terminateProcessTree preserves the existing hard-kill behavior on platforms
// without Linux procfs descendant discovery.
func terminateProcessTree(root *os.Process) {
	if root != nil {
		_ = root.Kill()
	}
}
