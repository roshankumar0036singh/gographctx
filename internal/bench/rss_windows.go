//go:build windows

package bench

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

var (
	modPsapi              = syscall.NewLazyDLL("psapi.dll")
	modKernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetProcessMemInfo = modPsapi.NewProc("GetProcessMemoryInfo")
	procGetCurrentProcess = modKernel32.NewProc("GetCurrentProcess")
	procOpenProcess       = modKernel32.NewProc("OpenProcess")
	procCloseHandle       = modKernel32.NewProc("CloseHandle")
)

const processQueryLimitedInformation = 0x1000

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// CurrentPeakRSSBytes returns the current process peak working set in bytes.
func CurrentPeakRSSBytes() uint64 {
	if procGetCurrentProcess.Find() != nil || procGetProcessMemInfo.Find() != nil {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.Sys
	}
	h, _, _ := procGetCurrentProcess.Call()
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	ok, _, _ := procGetProcessMemInfo.Call(h, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	if ok == 0 {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.Sys
	}
	return uint64(pmc.peakWorkingSetSize)
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
	if err := cmd.Start(); err != nil {
		return 0, "", err
	}
	var procHandle uintptr
	if procOpenProcess.Find() == nil {
		h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(cmd.Process.Pid))
		procHandle = h
	}
	err := cmd.Wait()
	var peak uint64
	if procHandle != 0 {
		if procGetProcessMemInfo.Find() == nil {
			var pmc processMemoryCounters
			pmc.cb = uint32(unsafe.Sizeof(pmc))
			if ok, _, _ := procGetProcessMemInfo.Call(procHandle, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb)); ok != 0 {
				peak = uint64(pmc.peakWorkingSetSize)
			}
		}
		_, _, _ = procCloseHandle.Call(procHandle)
	}
	return peak, buf.String(), err
}
