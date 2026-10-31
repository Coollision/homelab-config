// Package shares holds the Share model and the pure decision logic that turns PVCs, Longhorn
// Volumes and migration entries into shares. It mirrors SMBShare / resolve_share_endpoint /
// discover_migration_shares from the Python operator.
package shares

import (
	"fmt"
	"strings"
)

// Mount types.
const (
	MountNFS    = "nfs"
	MountDirect = "direct"
)

// Access modes (value of the smb-access label).
const (
	AccessShared   = "shared"
	AccessReadOnly = "read-only"
)

// Share is one SMB share.
//
//   - MountType nfs: RWX volume, mounted at runtime by the in-pod reconciler from the mount table.
//   - MountType direct: RWO migration volume, CSI-mounted into the pod via a temp PV+PVC.
//
// Attached is false for an RWX volume whose share-manager is down; the share keeps its slot.
type Share struct {
	Name               string `json:"name"`
	Path               string `json:"path"`
	Namespace          string `json:"namespace"`
	PVCName            string `json:"pvc_name"`
	AccessMode         string `json:"access_mode"`
	MountType          string `json:"mount_type"`
	NFSServer          string `json:"nfs_server"`
	NFSPath            string `json:"nfs_path"`
	LonghornVolumeName string `json:"longhorn_volume_name"`
	Attached           bool   `json:"attached"`
}

// New returns a share with the Python dataclass defaults (nfs, attached).
func New(name, namespace, pvcName, accessMode string) Share {
	return Share{
		Name:       name,
		Path:       "/shares/" + name,
		Namespace:  namespace,
		PVCName:    pvcName,
		AccessMode: accessMode,
		MountType:  MountNFS,
		Attached:   true,
	}
}

// ReadOnly reports whether the share is exported read-only.
func (s Share) ReadOnly() bool { return s.AccessMode == AccessReadOnly }

// UniqueID is "<namespace>/<pvc>".
func (s Share) UniqueID() string { return s.Namespace + "/" + s.PVCName }

// Endpoint is "server:/path" for a live NFS share, "" when detached or direct.
func (s Share) Endpoint() string {
	if s.MountType != MountNFS || !s.Attached {
		return ""
	}
	if s.NFSServer == "" || s.NFSPath == "" {
		return ""
	}
	return s.NFSServer + ":" + s.NFSPath
}

// StructuralKey is the identity of the share as it appears in the pod spec. It deliberately
// excludes the endpoint and attachment state.
func (s Share) StructuralKey() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", s.UniqueID(), s.Name, s.Path, s.MountType, s.AccessMode)
}

// MigrationPVName is the temp PV name for a direct share.
func (s Share) MigrationPVName() string { return "migrate-" + s.LonghornVolumeName }

// MigrationPVCName is the temp PVC name for a direct share.
func (s Share) MigrationPVCName() string { return "migrate-" + s.LonghornVolumeName }

// PodVolumeName is the pod-spec volume name (truncated to 63 chars, like Python's [:63]).
func (s Share) PodVolumeName() string {
	n := s.MigrationPVCName()
	if len(n) > 63 {
		return n[:63]
	}
	return n
}

// ParseNFSEndpoint parses "nfs://server/path" into ("server", "/path").
func ParseNFSEndpoint(ep string) (server, path string, ok bool) {
	if ep == "" || !strings.HasPrefix(ep, "nfs://") {
		return "", "", false
	}
	rest := ep[len("nfs://"):]
	server, p, found := strings.Cut(rest, "/")
	if !found {
		return "", "", false
	}
	return server, "/" + p, true
}
