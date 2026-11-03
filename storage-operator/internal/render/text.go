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
	// SMBConfDir holds the smb-config ConfigMap as a whole-directory mount (never subPath, which
	// kubelet never refreshes). smbd reads it through the /etc/samba/smb.conf symlink the
	// startup script creates, so an updated smb.conf reaches the running smbd without a pod roll.
	SMBConfDir  = "/etc/smb-conf"
	SMBConfPath = SMBConfDir + "/" + SMBConfKey
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
		"Conf":     SMBConfPath,
		"Opts":     cfg.NFSMountOptions,
		"Interval": cfg.MountReconcileInterval,
	})
	if err != nil {
		panic(err) // template is static; only a programming error can fail here
	}
	return b.String()
}

// StartupScript renders the samba container command (generate_startup_script). It contains
// neither share names nor NFS endpoints by design: the mount reconciler creates and mounts every
// share path from the mount table, and smbd re-reads smb.conf on request. Adding or removing a
// share therefore never changes the pod template, so it never restarts smbd or drops clients.
func StartupScript(cfg *config.Config) string {
	u := cfg.SMBUsername
	lines := []string{
		"#!/bin/bash",
		"set -e",
		"echo 'Starting SMB server initialization...'",
		"echo ''",
		"",
		"# smb.conf lives in a ConfigMap directory mount so updates reach the running smbd",
		"mkdir -p /etc/samba",
		fmt.Sprintf("ln -sf %s /etc/samba/smb.conf", SMBConfPath),
		"",
		"# Create SMB user for authentication",
		fmt.Sprintf("useradd -M -s /usr/sbin/nologin %s 2>/dev/null || true", u),
		fmt.Sprintf("echo -e \"$SMB_PASSWORD\\n$SMB_PASSWORD\" | smbpasswd -a -s %s", u),
		fmt.Sprintf("echo 'SMB user %s configured'", u),
		"echo ''",
		"",
	}
	lines = append(lines,
		"# Install and start the NFS mount reconciler (also reloads smbd when smb.conf changes)",
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

// PreStopScript renders the preStop hook (generate_prestop_script). It unmounts whatever NFS
// shares are mounted under /shares at that moment, so it does not depend on the share list.
func PreStopScript() string {
	lines := []string{
		"echo 'Shutting down gracefully...'",
		"sleep 5",
		"while read -r _ mp fstype _; do",
		"  case \"$fstype\" in nfs*) ;; *) continue ;; esac",
		"  case \"$mp\" in /shares/*) ;; *) continue ;; esac",
		"  echo \"Unmounting $mp\"",
		"  timeout 5 umount -f \"$mp\" 2>/dev/null || umount -l \"$mp\" 2>/dev/null || echo \"  already gone: $mp\"",
		"done < /proc/self/mounts",
		"echo 'preStop complete'",
		"exit 0",
	}
	return strings.Join(lines, "\n")
}
