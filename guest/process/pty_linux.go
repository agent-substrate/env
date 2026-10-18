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

package process

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// openPTY allocates a Linux pseudo-terminal master/slave pair and optionally
// sets its initial window dimensions.
func openPTY(rows, cols uint32) (*os.File, *os.File, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening /dev/ptmx: %w", err)
	}

	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		ptmx.Close()
		return nil, nil, fmt.Errorf("unlocking pty slave (TIOCSPTLCK): %w", errno)
	}

	var ptyNum uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptyNum))); errno != 0 {
		ptmx.Close()
		return nil, nil, fmt.Errorf("querying pty number (TIOCGPTN): %w", errno)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", ptyNum)
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("opening %s: %w", slavePath, err)
	}

	if rows > 0 || cols > 0 {
		if err := setWinsize(ptmx, rows, cols); err != nil {
			slave.Close()
			ptmx.Close()
			return nil, nil, err
		}
	}
	return ptmx, slave, nil
}

// setWinsize updates the terminal window size on ptmx via TIOCSWINSZ, which
// also delivers SIGWINCH to the foreground process group.
func setWinsize(ptmx *os.File, rows, cols uint32) error {
	ws := winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		return fmt.Errorf("setting window size (TIOCSWINSZ): %w", errno)
	}
	return nil
}

func ptySysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
}
