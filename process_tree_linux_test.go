//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const processTreeHelperMode = "TREC_PROCESS_TREE_HELPER_MODE"

func TestTerminalSessionCloseTerminatesSetsidDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "processes")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTerminalSessionProcessTreeHelper$")
	cmd.Env = append(os.Environ(),
		processTreeHelperMode+"=root",
		"TREC_PROCESS_TREE_HELPER_PID_FILE="+pidFile,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = cmd.Stdout
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper root: %v", err)
	}

	ts := newTerminalSession(stdin, stdout, cmd, 80, 24, nil, nil, false, nil)
	t.Cleanup(func() {
		_ = ts.close()
	})

	rootPID, descendantPID := waitForHelperPIDs(t, pidFile)
	if rootPID != cmd.Process.Pid {
		t.Fatalf("helper root PID = %d, command PID = %d", rootPID, cmd.Process.Pid)
	}
	if pgid, err := syscall.Getpgid(descendantPID); err != nil {
		t.Fatalf("get descendant process group: %v", err)
	} else if pgid != descendantPID {
		t.Fatalf("descendant process group = %d, want its PID %d (setsid)", pgid, descendantPID)
	}

	closed := make(chan error, 1)
	go func() { closed <- ts.closeWithReason("test teardown") }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close terminal session: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal session close did not finish")
	}

	waitForProcessGone(t, rootPID)
	waitForProcessGone(t, descendantPID)
}

func TestTerminalSessionProcessTreeHelper(t *testing.T) {
	switch os.Getenv(processTreeHelperMode) {
	case "":
		return
	case "descendant":
		select {}
	case "root":
		signal.Ignore(syscall.SIGCHLD)
		child := exec.Command(os.Args[0], "-test.run=^TestTerminalSessionProcessTreeHelper$")
		child.Env = append(os.Environ(), processTreeHelperMode+"=descendant")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start descendant: %v\n", err)
			os.Exit(2)
		}
		contents := fmt.Sprintf("%d %d\n", os.Getpid(), child.Process.Pid)
		if err := os.WriteFile(os.Getenv("TREC_PROCESS_TREE_HELPER_PID_FILE"), []byte(contents), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write PID file: %v\n", err)
			os.Exit(2)
		}
		select {}
	default:
		os.Exit(2)
	}
}

func waitForHelperPIDs(t *testing.T, path string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 2 {
				root, rootErr := strconv.Atoi(fields[0])
				descendant, descendantErr := strconv.Atoi(fields[1])
				if rootErr == nil && descendantErr == nil {
					return root, descendant
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper PIDs were not written to %s", path)
	return 0, 0
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	path := "/proc/" + strconv.Itoa(pid)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if err != nil && os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after terminal session close", pid)
}
