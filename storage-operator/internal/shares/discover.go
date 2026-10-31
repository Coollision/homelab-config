package shares

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// PVC status annotation values.
const (
	StatusPending  = "pending"
	StatusActive   = "active"
	StatusDetached = "detached"
	StatusError    = "error"
)

// Event is an event to record against the PVC being processed.
type Event struct {
	Type    string // Normal | Warning
	Reason  string
	Message string
}

// PVCResult is the outcome for one labelled PVC: an optional share, the annotation to write
// (always written, like Python) and an optional event.
type PVCResult struct {
	Share     *Share
	Status    string
	SharePath string
	Event     *Event
	// Log is a human-readable line describing the decision (for operator logs).
	Log string
	// Warn marks Log as a warning.
	Warn bool
}

// PVCInput is the subset of a PVC the decision needs.
type PVCInput struct {
	Namespace  string
	Name       string
	VolumeName string
	Labels     map[string]string
}

// NormalizeAccess applies the smb-access fallback. ok=false means it fell back to shared.
func NormalizeAccess(labels map[string]string) (mode string, ok bool) {
	mode, present := labels["smb-access"]
	if !present {
		mode = AccessShared
	}
	if mode != AccessShared && mode != AccessReadOnly {
		return AccessShared, false
	}
	return mode, true
}

// ShareName returns the smb-share-name label or the PVC name.
func ShareName(p PVCInput) string {
	if v, ok := p.Labels["smb-share-name"]; ok {
		return v
	}
	return p.Name
}

// ResolvePVC mirrors discover_shares + resolve_share_endpoint for one PVC. volume is the Longhorn
// Volume object (unstructured content) or nil if it was not found. Unbound PVCs must be handled
// by the caller via UnboundResult before looking up a volume.
func ResolvePVC(p PVCInput, volume map[string]any, lbIP string) PVCResult {
	access, _ := NormalizeAccess(p.Labels)
	shareName := ShareName(p)

	if volume == nil {
		return PVCResult{
			Status: StatusError,
			Event:  &Event{"Warning", "VolumeNotFound", fmt.Sprintf("Longhorn volume %s not found", p.VolumeName)},
			Log:    fmt.Sprintf("Longhorn volume %s not found for %s/%s", p.VolumeName, p.Namespace, p.Name),
			Warn:   true,
		}
	}

	status := nestedMap(volume, "status")
	ep := nestedStr(status, "shareEndpoint")
	if ep != "" {
		server, path, ok := ParseNFSEndpoint(ep)
		if !ok {
			return PVCResult{Status: StatusError, Log: "Failed to parse NFS endpoint: " + ep, Warn: true}
		}
		s := New(shareName, p.Namespace, p.Name, access)
		s.NFSServer, s.NFSPath = server, path
		return PVCResult{
			Share:     &s,
			Status:    StatusActive,
			SharePath: fmt.Sprintf("//%s/%s", lbIP, shareName),
			Log:       fmt.Sprintf("  NFS endpoint: %s:%s", server, path),
		}
	}

	if nestedStr(nestedMap(volume, "spec"), "accessMode") == "rwx" {
		s := New(shareName, p.Namespace, p.Name, access)
		s.Attached = false
		return PVCResult{
			Share:  &s,
			Status: StatusDetached,
			Event: &Event{"Normal", "ShareDetached", fmt.Sprintf(
				"Longhorn volume %s is detached; SMB share is present but unmounted until the volume reattaches",
				p.VolumeName)},
			Log: fmt.Sprintf("  Volume %s detached (state=%s, shareState=%s) — keeping share '%s', unmounted until it reattaches",
				p.VolumeName, orDefault(nestedStr(status, "state"), "unknown"),
				orDefault(nestedStr(status, "shareState"), "none"), shareName),
		}
	}

	return PVCResult{
		Status: StatusError,
		Event:  &Event{"Warning", "NoShareEndpoint", "Volume does not have NFS share endpoint (must be RWX)"},
		Log:    fmt.Sprintf("No share endpoint for volume %s and it is not RWX, skipping", p.VolumeName),
		Warn:   true,
	}
}

// UnboundResult is the result for a PVC without spec.volumeName.
func UnboundResult(p PVCInput) PVCResult {
	return PVCResult{
		Status: StatusPending,
		Event:  &Event{"Warning", "NotBound", "PVC is not bound to a volume"},
		Log:    fmt.Sprintf("PVC %s/%s is not bound, skipping", p.Namespace, p.Name),
		Warn:   true,
	}
}

// MigrationEntry is one parsed volumes.yaml entry.
type MigrationEntry struct {
	VolumeName string
	ShareName  string
	ReadOnly   bool
}

// ParseMigration parses volumes.yaml. Invalid entries are skipped and reported in warnings.
// Parse errors yield no entries (never fatal), like the Python operator.
func ParseMigration(raw string) (entries []MigrationEntry, warnings []string, err error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil, nil
	}
	var data map[string]any
	if err := yaml.Unmarshal([]byte(raw), &data); err != nil {
		return nil, nil, fmt.Errorf("failed to parse migration ConfigMap volumes.yaml: %w", err)
	}
	list, _ := data["volumes"].([]any)
	for _, item := range list {
		e, _ := item.(map[string]any)
		name := strings.TrimSpace(strOf(e["name"]))
		if name == "" {
			warnings = append(warnings, fmt.Sprintf("Migration entry missing 'name', skipping: %v", item))
			continue
		}
		share := name
		if v, ok := e["shareName"]; ok {
			share = strOf(v)
		}
		share = strings.TrimSpace(share)
		mode := "rw"
		if v, ok := e["smb"]; ok {
			mode = strOf(v)
		}
		mode = strings.ToLower(strings.TrimSpace(mode))
		if mode != "rw" && mode != "ro" {
			warnings = append(warnings, fmt.Sprintf("Invalid smb value '%s' for migration volume %s, defaulting to 'rw'", mode, name))
			mode = "rw"
		}
		entries = append(entries, MigrationEntry{VolumeName: name, ShareName: share, ReadOnly: mode == "ro"})
	}
	return entries, warnings, nil
}

// ResolveMigration turns an entry plus its Longhorn Volume into a share. ok=false means skip
// (volume nil or unparseable endpoint). operatorNS is where synthetic migration ids live.
func ResolveMigration(e MigrationEntry, volume map[string]any, operatorNS string) (Share, bool) {
	if volume == nil {
		return Share{}, false
	}
	access := AccessShared
	if e.ReadOnly {
		access = AccessReadOnly
	}
	s := New(e.ShareName, operatorNS, "migration-"+e.VolumeName, access)
	s.LonghornVolumeName = e.VolumeName
	ep := nestedStr(nestedMap(volume, "status"), "shareEndpoint")
	if ep != "" {
		server, path, ok := ParseNFSEndpoint(ep)
		if !ok {
			return Share{}, false
		}
		s.NFSServer, s.NFSPath = server, path
		return s, true
	}
	s.MountType = MountDirect
	return s, true
}

// SizeMi converts Longhorn spec.size (bytes, usually a string) to "<n>Mi"; "1Gi" on failure.
// A missing size defaults to 1073741824 bytes, as in Python.
func SizeMi(volume map[string]any) string {
	v, ok := nestedMap(volume, "spec")["size"]
	if !ok {
		return "1024Mi"
	}
	var n int64
	switch t := v.(type) {
	case string:
		if _, err := fmt.Sscan(t, &n); err != nil || fmt.Sprint(n) != strings.TrimSpace(t) {
			return "1Gi"
		}
	case int64:
		n = t
	case float64:
		n = int64(t)
	case int:
		n = int64(t)
	default:
		return "1Gi"
	}
	return fmt.Sprintf("%dMi", floorDiv(n, 1024*1024))
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func nestedMap(m map[string]any, k string) map[string]any {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func nestedStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func strOf(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	return fmt.Sprint(v)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
