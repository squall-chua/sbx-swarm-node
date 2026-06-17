//go:build !linux

package sandbox

// resolveLimit returns the explicit config limit; host auto-detect is
// Linux-only, so a 0 config means unlimited on other platforms.
func resolveLimit(configured, _ float64) float64 { return configured }

// detectHostLimits is a best-effort no-op (0 == unlimited) on non-Linux
// platforms; only CPU count is portable.
func detectHostLimits(string) (cpuCores, memKB, diskGB float64) {
	return float64(numCPU()), 0, 0
}

// DetectHostLimits is the exported entry point (non-Linux fallback).
func DetectHostLimits(dataDir string) (cpuCores, memKB, diskGB float64) {
	return detectHostLimits(dataDir)
}
