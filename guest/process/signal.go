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

package process

import (
	"syscall"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
)

// signalTable maps API signals to the host's syscall signals by name, so the
// mapping stays correct on platforms whose signal numbers differ from Linux.
var signalTable = map[ateenvv1alpha.Signal]syscall.Signal{
	ateenvv1alpha.Signal_SIGNAL_HUP:    syscall.SIGHUP,
	ateenvv1alpha.Signal_SIGNAL_INT:    syscall.SIGINT,
	ateenvv1alpha.Signal_SIGNAL_QUIT:   syscall.SIGQUIT,
	ateenvv1alpha.Signal_SIGNAL_ILL:    syscall.SIGILL,
	ateenvv1alpha.Signal_SIGNAL_TRAP:   syscall.SIGTRAP,
	ateenvv1alpha.Signal_SIGNAL_ABRT:   syscall.SIGABRT,
	ateenvv1alpha.Signal_SIGNAL_BUS:    syscall.SIGBUS,
	ateenvv1alpha.Signal_SIGNAL_FPE:    syscall.SIGFPE,
	ateenvv1alpha.Signal_SIGNAL_KILL:   syscall.SIGKILL,
	ateenvv1alpha.Signal_SIGNAL_USR1:   syscall.SIGUSR1,
	ateenvv1alpha.Signal_SIGNAL_SEGV:   syscall.SIGSEGV,
	ateenvv1alpha.Signal_SIGNAL_USR2:   syscall.SIGUSR2,
	ateenvv1alpha.Signal_SIGNAL_PIPE:   syscall.SIGPIPE,
	ateenvv1alpha.Signal_SIGNAL_ALRM:   syscall.SIGALRM,
	ateenvv1alpha.Signal_SIGNAL_TERM:   syscall.SIGTERM,
	ateenvv1alpha.Signal_SIGNAL_CHLD:   syscall.SIGCHLD,
	ateenvv1alpha.Signal_SIGNAL_CONT:   syscall.SIGCONT,
	ateenvv1alpha.Signal_SIGNAL_STOP:   syscall.SIGSTOP,
	ateenvv1alpha.Signal_SIGNAL_TSTP:   syscall.SIGTSTP,
	ateenvv1alpha.Signal_SIGNAL_TTIN:   syscall.SIGTTIN,
	ateenvv1alpha.Signal_SIGNAL_TTOU:   syscall.SIGTTOU,
	ateenvv1alpha.Signal_SIGNAL_URG:    syscall.SIGURG,
	ateenvv1alpha.Signal_SIGNAL_XCPU:   syscall.SIGXCPU,
	ateenvv1alpha.Signal_SIGNAL_XFSZ:   syscall.SIGXFSZ,
	ateenvv1alpha.Signal_SIGNAL_VTALRM: syscall.SIGVTALRM,
	ateenvv1alpha.Signal_SIGNAL_PROF:   syscall.SIGPROF,
	ateenvv1alpha.Signal_SIGNAL_WINCH:  syscall.SIGWINCH,
	ateenvv1alpha.Signal_SIGNAL_IO:     syscall.SIGIO,
	ateenvv1alpha.Signal_SIGNAL_SYS:    syscall.SIGSYS,
}

var syscallTable = func() map[syscall.Signal]ateenvv1alpha.Signal {
	m := make(map[syscall.Signal]ateenvv1alpha.Signal, len(signalTable))
	for api, sys := range signalTable {
		m[sys] = api
	}
	return m
}()

// ToSyscallSignal converts an API signal to the host's syscall signal.
// The second result is false for SIGNAL_UNSPECIFIED or unknown values.
func ToSyscallSignal(sig ateenvv1alpha.Signal) (syscall.Signal, bool) {
	sys, ok := signalTable[sig]
	return sys, ok
}

// FromSyscallSignal converts a host syscall signal to the API signal, or
// SIGNAL_UNSPECIFIED if it has no API equivalent.
func FromSyscallSignal(sig syscall.Signal) ateenvv1alpha.Signal {
	return syscallTable[sig]
}
