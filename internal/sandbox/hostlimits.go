//go:build linux

package sandbox

import "golang.org/x/sys/unix"

// resolveLimit returns the explicit config limit when set (>0), else the
// auto-detected host value (which may be 0 == unlimited on detection failure).
func resolveLimit(configured, detected float64) float64 {
	if configured > 0 {
		return configured
	}
	return detected
}

// detectHostLimits best-effort reads host CPU (cores), memory (KB), and the
// filesystem total at dataDir (GB). Any failed probe yields 0 (== unlimited).
// Linux-only (the deploy target); see hostlimits_other.go for the fallback.
func detectHostLimits(dataDir string) (cpuCores, memKB, diskGB float64) {
	cpuCores = float64(numCPU())

	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err == nil {
		// Totalram is in units of si.Unit bytes.
		totalBytes := uint64(si.Totalram) * uint64(si.Unit)
		memKB = float64(totalBytes) / 1024
	}

	var st unix.Statfs_t
	if err := unix.Statfs(dataDir, &st); err == nil {
		totalBytes := uint64(st.Bsize) * st.Blocks
		diskGB = float64(totalBytes) / (1 << 30)
	}
	return cpuCores, memKB, diskGB
}

// DetectHostLimits is the exported entry point for host capacity probing
// (callable from packages that cannot use build tags, e.g. node wiring).
func DetectHostLimits(dataDir string) (cpuCores, memKB, diskGB float64) {
	return detectHostLimits(dataDir)
}
