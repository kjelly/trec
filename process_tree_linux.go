//go:build linux

package main

import (
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type processTreeEntry struct {
	pid   int
	depth int
}

// terminateProcessTree performs the hard teardown expected by terminalSession.
// Descendants are snapshotted and killed deepest-first before the root process.
// Killing each distinct descendant process group also reaches children that have
// moved into a new session; killing the individual PID covers shared groups.
func terminateProcessTree(root *os.Process) {
	if root == nil || root.Pid <= 0 {
		return
	}

	rootPID := root.Pid
	rootPGID, _ := syscall.Getpgid(rootPID)
	selfPGID := syscall.Getpgrp()
	killedGroups := make(map[int]struct{})

	for _, entry := range linuxDescendants(rootPID) {
		pgid, err := syscall.Getpgid(entry.pid)
		if err == nil && pgid > 1 && pgid != rootPGID && pgid != selfPGID {
			if _, seen := killedGroups[pgid]; !seen {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				killedGroups[pgid] = struct{}{}
			}
		}
		if process, err := os.FindProcess(entry.pid); err == nil {
			_ = process.Kill()
		}
	}

	_ = root.Kill()
	// A child can fork between the snapshot and the root kill. The PTY-created
	// root normally owns a distinct process group, so clean up that exact group
	// after killing the root without ever using command-name matching.
	if rootPGID > 1 && rootPGID != selfPGID {
		_ = syscall.Kill(-rootPGID, syscall.SIGKILL)
	}
}

func linuxDescendants(rootPID int) []processTreeEntry {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	byParent := make(map[int][]int)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		ppid, err := linuxProcessParent(pid)
		if err != nil {
			continue
		}
		byParent[ppid] = append(byParent[ppid], pid)
	}

	var descendants []processTreeEntry
	seen := map[int]struct{}{rootPID: {}}
	var visit func(int, int)
	visit = func(parent, depth int) {
		for _, pid := range byParent[parent] {
			if _, ok := seen[pid]; ok {
				continue
			}
			seen[pid] = struct{}{}
			descendants = append(descendants, processTreeEntry{pid: pid, depth: depth})
			visit(pid, depth+1)
		}
	}
	visit(rootPID, 1)
	sort.Slice(descendants, func(i, j int) bool {
		if descendants[i].depth != descendants[j].depth {
			return descendants[i].depth > descendants[j].depth
		}
		return descendants[i].pid > descendants[j].pid
	})
	return descendants
}

func linuxProcessParent(pid int) (int, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// The comm field is parenthesized and may contain spaces or ')' bytes, so
	// split only after its final closing parenthesis. The following fields are
	// state, ppid, pgrp, ... as documented by proc_pid_stat(5).
	closing := strings.LastIndexByte(string(data), ')')
	if closing < 0 || closing+2 >= len(data) {
		return 0, errors.New("malformed proc stat")
	}
	fields := strings.Fields(string(data[closing+1:]))
	if len(fields) < 2 {
		return 0, errors.New("malformed proc stat")
	}
	return strconv.Atoi(fields[1])
}
