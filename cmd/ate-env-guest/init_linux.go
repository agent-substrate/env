//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// prSetChildSubreaper is Linux PR_SET_CHILD_SUBREAPER (<linux/prctl.h>).
const prSetChildSubreaper = 36

// forwardedSignals are passed through from the PID 1 init to the supervised
// guest process.
var forwardedSignals = []os.Signal{
	syscall.SIGTERM,
	syscall.SIGINT,
	syscall.SIGHUP,
	syscall.SIGQUIT,
	syscall.SIGUSR1,
	syscall.SIGUSR2,
}

// runIfPID1 runs a minimal init when ate-env-guest is started as PID 1 and
// exits with the supervised guest process's status.
//
// Inside an actor container or microVM, PID 1 inherits every orphaned
// descendant whose parent exits first and is the only process that can reap
// them. Re-executing ate-env-guest as a child process lets PID 1 reap all
// reparented orphans with wait4(-1) without racing os/exec's cmd.Wait() in
// the guest server's ProcessService tracker or sidecar supervisors.
func runIfPID1() {
	if os.Getpid() != 1 {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ate-env-guest init: finding executable: %v\n", err)
		os.Exit(1)
	}
	os.Exit(supervise(exe, os.Args))
}

// supervise starts path as a child, forwards signals to it, reaps all exited
// descendants (both reparented orphans and the direct child), and returns the
// child's shell-style exit code.
func supervise(path string, argv []string) int {
	// A no-op as PID 1, and lets unit tests exercise orphan reaping as a
	// non-root, non-PID-1 process.
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0, 0, 0, 0); errno != 0 {
		fmt.Fprintf(os.Stderr, "ate-env-guest init: setting child subreaper: %v\n", errno)
		return 1
	}
	defer func() {
		_, _, _ = syscall.Syscall6(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0, 0, 0, 0)
	}()

	// Subscribe before starting the child so no SIGCHLD or termination signal
	// is missed.
	sigs := make(chan os.Signal, 32)
	signal.Notify(sigs, append([]os.Signal{syscall.SIGCHLD}, forwardedSignals...)...)
	defer signal.Stop(sigs)

	child, err := os.StartProcess(path, argv, &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ate-env-guest init: starting %s: %v\n", path, err)
		return 1
	}

	if status, exited := reap(child.Pid); exited {
		return exitCode(status)
	}

	for sig := range sigs {
		if sig != syscall.SIGCHLD {
			_ = child.Signal(sig)
		}
		// Reap after every signal: os/signal drops signals when sigs is full
		// and multiple child exits coalesce into a single SIGCHLD.
		if status, exited := reap(child.Pid); exited {
			return exitCode(status)
		}
	}
	panic("unreachable")
}

// reap collects every exited child via wait4(-1, WNOHANG) and reports whether
// pid was among them.
func reap(pid int) (syscall.WaitStatus, bool) {
	var childStatus syscall.WaitStatus
	childExited := false
	for {
		var status syscall.WaitStatus
		got, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || got <= 0 {
			return childStatus, childExited
		}
		if got == pid {
			childStatus, childExited = status, true
		}
	}
}

// exitCode maps a WaitStatus to a shell-style exit code.
func exitCode(status syscall.WaitStatus) int {
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}
