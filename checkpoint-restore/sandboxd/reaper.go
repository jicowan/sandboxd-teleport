package main

// sandboxd runs as PID 1 in its (scratch) container. When runsc's detached
// gofer/sentry processes exit, they re-parent to PID 1 (us). If we don't reap
// them, they become zombies that keep the container's cgroup non-empty, which
// makes `runsc delete` fail with "removing cgroup path ... device or resource
// busy" and stall for seconds. Substrate uses a reaper for exactly this reason
// (cmd/ateom-gvisor: go reap.ReapChildren + reapLock).
//
// This is a minimal SIGCHLD-driven reaper: on SIGCHLD, wait4(-1) in a loop to
// collect ALL exited children (non-blocking), so no zombies accumulate.

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func startReaper() {
	// If we're not PID 1 there's usually already an init reaping for us, but
	// running the reaper is harmless (wait4 just returns ECHILD).
	ch := make(chan os.Signal, 64)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		reaped := 0
		for range ch {
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break // no more reapable children right now
				}
				reaped++
				if reaped%50 == 0 {
					log.Printf("reaper: reaped %d child processes", reaped)
				}
			}
		}
	}()
}
