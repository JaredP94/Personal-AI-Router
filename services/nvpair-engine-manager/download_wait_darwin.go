// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func watchDownloadExit(w *downloadWait) error {
	queue, err := unix.Kqueue()
	if err != nil {
		return err
	}
	unix.CloseOnExec(queue)
	event := unix.Kevent_t{
		Ident: uint64(w.cmd.Process.Pid), Filter: unix.EVFILT_PROC,
		Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT,
	}
	for {
		_, err = unix.Kevent(queue, []unix.Kevent_t{event}, nil, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = unix.Close(queue)
		// The child can exit before registration. It is our unreaped child,
		// so ESRCH means exit, not PID reuse; the zombie still anchors pgid.
		if errors.Is(err, unix.ESRCH) {
			close(w.exited)
			return nil
		}
		return err
	}
	go func() {
		defer unix.Close(queue)
		events := make([]unix.Kevent_t, 1)
		for {
			n, err := unix.Kevent(queue, nil, events, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				w.exitErr = err
			} else if n == 0 {
				continue
			} else if events[0].Flags&unix.EV_ERROR != 0 {
				w.exitErr = fmt.Errorf("watch download launcher: %w", unix.Errno(events[0].Data))
			}
			// EVFILT_PROC/NOTE_EXIT observes exit without consuming wait status.
			close(w.exited)
			return
		}
	}()
	return nil
}
