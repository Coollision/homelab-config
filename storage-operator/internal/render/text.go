// Package render produces every object and file the SMB operator writes. All functions are pure
// and golden-tested against output captured from the Python operator (testdata/, see DEV.md).
package render

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"homelab/storage-operator/internal/config"
	"homelab/storage-operator/internal/shares"
)

// Fixed names and paths (Python module constants).
const (
	MountTableConfigMap = "smb-mounts"
	MountTableKey       = "mounts.tsv"
	MountTableDir       = "/etc/smb-mounts"
	MountTablePath      = MountTableDir + "/" + MountTableKey
	MountReconcilerPath = "/usr/local/bin/smb-mount-reconciler.sh"
	SMBConfigMap        = "smb-config"
	SMBConfKey          = "smb.conf"
)

const vetoFiles = "/.DS_Store/._*/.Spotlight-V100/.TemporaryItems/.Trashes/.fseventsd/Thumbs.db/desktop.ini/$RECYCLE.BIN/System Volume Information/lost+found/"

// SMBConf renders smb.conf (generate_smb_config).
func SMBConf(cfg *config.Config, ss []shares.Share) string {
	lines := []string{
		"[global]",
		"workgroup = " + cfg.SMBWorkgroup,
		"server string = " + cfg.SMBServerString,
		"security = user",
		"map to guest = Never",
		"restrict anonymous = 1",
		"guest ok = no",
		"log level = 1",
		"",
		"# idmap — required to avoid 'range not specified' warnings on connect",
		"idmap config * : backend = tdb",
		"idmap config * : range = 1000-9999",
		"",
		"# Performance tuning (Samba 4.21+)",
		"socket options = TCP_NODELAY IPTOS_LOWDELAY SO_RCVBUF=131072 SO_SNDBUF=131072",
		"read raw = yes",
		"write raw = yes",
		"max xmit = 65535",
		"dead time = 15",
		"getwd cache = yes",
		"aio read size = 1",
		"aio write size = 1",
		"use sendfile = yes",
		"server multi channel support = yes",
		"strict locking = no",
		"oplocks = yes",
		"level2 oplocks = yes",
		"",
	}
	for _, s := range ss {
		writable, readOnly, writeList := "yes", "no", cfg.SMBUsername
		if s.ReadOnly() {
			writable, readOnly, writeList = "no", "yes", ""
		}
		lines = append(lines,
			"["+s.Name+"]",
			"path = "+s.Path,
			"available = yes",
			"browseable = yes",
			"public = no",
			"guest ok = no",
			"writable = "+writable,
			"read only = "+readOnly,
			"valid users = "+cfg.SMBUsername,
			"write list = "+writeList,
			"force user = root",
			"force group = root",
			"veto files = "+vetoFiles,
			"delete veto files = yes",
			"create mask = 0755",
			"directory mask = 0755",
			"",
		)
	}
	return strings.Join(lines, "\n")
}

// MountTable renders mounts.tsv (generate_mount_table). Direct shares are excluded.
func MountTable(ss []shares.Share) string {
	lines := []string{
		"# Managed by smb-operator — do not edit.",
		"# name\tpath\tendpoint (empty = detached, reconciler keeps it unmounted)",
	}
	for _, s := range ss {
		if s.MountType == shares.MountDirect {
			continue
		}
		lines = append(lines, s.Name+"\t"+s.Path+"\t"+s.Endpoint())
	}
	return strings.Join(lines, "\n") + "\n"
}

//go:embed scripts/mount-reconciler.sh.tmpl
var reconcilerTmpl string

var reconcilerTemplate = template.Must(template.New("reconciler").Delims("<%", "%>").Parse(reconcilerTmpl))

// MountReconciler renders the in-pod bash NFS mount reconciler (generate_mount_reconciler).
func MountReconciler(cfg *config.Config) string {
	var b bytes.Buffer
	err := reconcilerTemplate.Execute(&b, map[string]any{
		"Table":    MountTablePath,
		"Opts":     cfg.NFSMountOptions,
		"Interval": cfg.MountReconcileInterval,
	})
	if err != nil {
		panic(err) // template is static; only a programming error can fail here
	}
	return b.String()
}

// StartupScript renders the samba container command (generate_startup_script). It contains no
// NFS endpoints by design, so attach/detach never changes the pod spec.
func StartupScript(cfg *config.Config, ss []shares.Share) string {
	u := cfg.SMBUsername
	lines := []string{
		"#!/bin/bash",
		"set -e",
		"echo 'Starting SMB server initialization...'",
		"echo ''",
		"",
		"# Create SMB user for authentication",
		fmt.Sprintf("useradd -M -s /usr/sbin/nologin %s 2>/dev/null || true", u),
		fmt.Sprintf("echo -e \"$SMB_PASSWORD\\n$SMB_PASSWORD\" | smbpasswd -a -s %s", u),
		fmt.Sprintf("echo 'SMB user %s configured'", u),
		"echo ''",
		"",
	}
	for _, s := range ss {
		if s.MountType == shares.MountDirect {
			lines = append(lines, fmt.Sprintf("echo 'Direct CSI mount ready: %s'", s.Name))
		} else {
			lines = append(lines, fmt.Sprintf("echo 'NFS share (reconciler-managed): %s'", s.Name))
		}
		lines = append(lines, "mkdir -p "+s.Path)
	}
	lines = append(lines, "echo ''", "")
	lines = append(lines,
		"# Install and start the NFS mount reconciler",
		fmt.Sprintf("cat > %s <<'SMB_OPERATOR_RECONCILER_EOF'", MountReconcilerPath),
		strings.TrimRight(MountReconciler(cfg), "\n"),
		"SMB_OPERATOR_RECONCILER_EOF",
		"chmod +x "+MountReconcilerPath,
		MountReconcilerPath+" &",
		"echo 'Mount reconciler started'",
		"echo ''",
		"",
	)
	w := cfg.InitialMountWait
	lines = append(lines,
		fmt.Sprintf("echo 'Waiting up to %ds for initial mounts...'", w),
		fmt.Sprintf("for _ in $(seq 1 %d); do", w),
		fmt.Sprintf("  want=$(awk -F'\\t' '$1 !~ /^#/ && NF>=3 && $3 != \"\" {c++} END {print c+0}' %s 2>/dev/null || echo 0)", MountTablePath),
		"  have=$(awk '$3 ~ /^nfs/ && $2 ~ /^\\/shares\\// {c++} END {print c+0}' /proc/self/mounts)",
		"  [ \"$have\" -ge \"$want\" ] && break",
		"  sleep 1",
		"done",
		"echo ''",
		"",
		"echo 'Starting Samba daemon...'",
		"exec /usr/sbin/smbd --foreground --no-process-group",
	)
	return strings.Join(lines, "\n")
}

// PreStopScript renders the preStop hook (generate_prestop_script).
func PreStopScript(ss []shares.Share) string {
	lines := []string{"echo 'Shutting down gracefully...'", "sleep 5"}
	for _, s := range ss {
		if s.MountType == shares.MountDirect {
			continue
		}
		p := s.Path
		lines = append(lines,
			fmt.Sprintf("echo 'Unmounting %s'", p),
			fmt.Sprintf("timeout 5 umount -f %s 2>/dev/null || umount -l %s 2>/dev/null || echo '  already gone: %s'", p, p, p),
		)
	}
	lines = append(lines, "echo 'preStop complete'", "exit 0")
	return strings.Join(lines, "\n")
}
