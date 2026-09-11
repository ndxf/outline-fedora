//go:build linux

package tun

import (
	"fmt"
	"os"
	"os/exec"
)

// PanicScriptPath is the on-disk location of ouf-panic. Overridable via
// the OUF_PANIC_SCRIPT env var so the daemon can find the script in
// dev checkouts without an install step.
var PanicScriptPath = "/usr/local/sbin/ouf-panic"

func init() {
	if p := os.Getenv("OUF_PANIC_SCRIPT"); p != "" {
		PanicScriptPath = p
	}
}

// RunPanicStandalone is a package-external entry point for the daemon
// to invoke the panic script when no Session is live (e.g., after a
// crash where the snapshot is on disk but the previous daemon
// instance is gone).
func RunPanicStandalone(stateDir, resolvPath string) error {
	return runPanicScript(stateDir, resolvPath)
}

// runPanicScript invokes ouf-panic with the same STATE_DIR and
// RESOLV_PATH the session is using. Returns nil if the script exits
// with 0 (clean revert or nothing to revert).
func runPanicScript(stateDir, resolvPath string) error {
	if _, err := os.Stat(PanicScriptPath); err != nil {
		return fmt.Errorf("panic script not found at %s: %w", PanicScriptPath, err)
	}
	cmd := exec.Command(PanicScriptPath)
	cmd.Env = append(os.Environ(),
		"OUF_STATE_DIR="+stateDir,
		"OUF_RESOLV_PATH="+resolvPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ouf-panic failed: %w\noutput:\n%s", err, out)
	}
	return nil
}
