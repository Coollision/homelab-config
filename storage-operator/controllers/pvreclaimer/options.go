// Package pvreclaimer is the Go port of the pv-reclaimer shell loop: it makes Released
// PersistentVolumes Available again by clearing spec.claimRef.
//
// The defaults reproduce the shell behaviour exactly (every Released PV, any reclaim policy, any
// StorageClass, whole claimRef cleared). Every safety improvement is opt-in through Options.
package pvreclaimer

import (
	"fmt"
	"strings"
)

// PatchMode selects how spec.claimRef is reset.
type PatchMode string

const (
	// PatchModeClear removes the whole claimRef (shell parity): any PVC may bind the PV afterwards.
	PatchModeClear PatchMode = "clear"
	// PatchModeKeepClaim drops only claimRef.uid and claimRef.resourceVersion, so only a PVC with the
	// same namespace/name can bind the PV again.
	PatchModeKeepClaim PatchMode = "keep-claim"
)

// SkipAnnotation on a PV (value "true") excludes it when Options.HonorSkipAnnotation is set.
const SkipAnnotation = "storage-operator.homelab/pv-reclaimer-skip"

// Options configures the reclaimer. The zero value plus Mode=PatchModeClear is shell parity.
type Options struct {
	// DryRun sends patches with dryRun=All and emits a "WouldReclaim" event instead of "Reclaimed".
	DryRun bool
	// RetainOnly limits reclaiming to PVs with persistentVolumeReclaimPolicy=Retain.
	RetainOnly bool
	// StorageClasses, when non-empty, is an allowlist of spec.storageClassName values.
	StorageClasses []string
	// HonorSkipAnnotation skips PVs annotated with SkipAnnotation=true.
	HonorSkipAnnotation bool
	// Mode is the claimRef patch mode; empty means PatchModeClear.
	Mode PatchMode
}

// ParsePatchMode validates a --pv-reclaimer-patch-mode value.
func ParsePatchMode(s string) (PatchMode, error) {
	switch m := PatchMode(s); m {
	case PatchModeClear, PatchModeKeepClaim:
		return m, nil
	case "":
		return PatchModeClear, nil
	}
	return "", fmt.Errorf("unknown pv-reclaimer patch mode %q (want %q or %q)", s, PatchModeClear, PatchModeKeepClaim)
}

// ParseStorageClasses splits a comma-separated allowlist, dropping empty entries.
func ParseStorageClasses(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}
