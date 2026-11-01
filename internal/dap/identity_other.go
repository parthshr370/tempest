//go:build !linux && !darwin && !(freebsd && (amd64 || arm64 || riscv64))

package dap

// processStartTime can't establish a process's identity on this platform, so
// attach targets are never signalled here.
func processStartTime(int) (processStart, bool) { return processStart{}, false }
