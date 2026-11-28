//go:build !windows && !darwin

package bench

import (
	"bytes"
	"context"
	"os/exec"
	"syscall"
	"time"
)

// CurrentPeakRSSBytes returns the current process maximum resident set size in bytes.
func CurrentPeakRSSBytes() uint64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if ru.Maxrss <= 0 {
		return 0
	}
	// On Linux and BSDs, ru_maxrss is reported in KiB.
	return uint64(ru.Maxrss) * 1024
}

func runMeasuredCommand(ctx context.Context, dir, name string, args ...string) (uint64, error) {
	peak, _, err := RunMeasuredCapture(ctx, dir, 0, name, args...)
	return peak, err
}

// RunMeasuredCapture executes a subprocess and returns its peak RSS in bytes alongside combined stdout/stderr.
func RunMeasuredCapture(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (uint64, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	var peak uint64
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok && ru != nil && ru.Maxrss > 0 {
			peak = uint64(ru.Maxrss) * 1024
		}
	}
	return peak, buf.String(), err
}
