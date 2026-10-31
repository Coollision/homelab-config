package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellcheck lints every rendered script. Skipped when shellcheck is not installed (CI
// installs it).
func TestShellcheck(t *testing.T) {
	bin, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	cfgs := loadConfigs(t)
	fixtures := loadShares(t)
	dir := t.TempDir()
	var files []string
	for cn, cfg := range cfgs {
		recon := filepath.Join(dir, cn+".reconciler.sh")
		_ = os.WriteFile(recon, []byte(MountReconciler(cfg)), 0o600)
		files = append(files, recon)
		for fn, ss := range fixtures {
			base := filepath.Join(dir, cn+"__"+fn)
			_ = os.WriteFile(base+".startup.sh", []byte(StartupScript(cfg, ss)), 0o600)
			_ = os.WriteFile(base+".prestop.sh", []byte("#!/bin/bash\n"+PreStopScript(ss)), 0o600)
			files = append(files, base+".startup.sh", base+".prestop.sh")
		}
	}
	out, err := exec.Command(bin, append([]string{"-s", "bash"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
