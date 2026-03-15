#!/usr/bin/env bash
# =============================================================================
# nuke-longhorn.sh
# Completely removes Longhorn from the cluster, preserving replica data on
# /mnt/longhorn so it can be recovered via NFS backup restore afterwards.
#
# Usage:
#   ./nuke-longhorn.sh            # interactive (asks for confirmation)
#   ./nuke-longhorn.sh --yes      # non-interactive
#
# After running this script:
#   - All Longhorn CRDs, namespaces, and cluster resources are gone
#   - Replica dirs on /mnt/longhorn on each node are untouched
#   - NFS backups on your NAS are untouched
#   - Re-apply Longhorn from Git + trigger a SystemRestore to recover
#
# Recovery:
#   cd system/storage
#   kubectl apply -f longhorn && kubectl apply -k longhorn-extra && kubectl apply -k longhorn-smb-operator
#   # Wait for Longhorn manager to be ready, then:
#   kubectl apply -f - <<EOF
#   apiVersion: longhorn.io/v1beta2
#   kind: SystemRestore
#   metadata:
#     name: restore-$(date +%Y%m%d)
#     namespace: storage
#   spec:
#     systemBackup: <name-from-nfs>   # check: kubectl get systembackups -n storage
#   EOF
# =============================================================================

set -euo pipefail

LONGHORN_NAMESPACE="storage"
ARGOCD_NAMESPACE="argocd"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RED='\033[0;31m'
YELLOW='\033[1;33m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${CYAN}[nuke]${NC} $*"; }
ok()   { echo -e "${GREEN}[ok]${NC} $*"; }
warn() { echo -e "${YELLOW}[warn]${NC} $*"; }
die()  { echo -e "${RED}[error]${NC} $*" >&2; exit 1; }

# ── Confirmation ──────────────────────────────────────────────────────────────
if [[ "${1:-}" != "--yes" ]]; then
  echo -e "${RED}"
  echo "  ██╗    ██╗ █████╗ ██████╗ ███╗   ██╗██╗███╗   ██╗ ██████╗ "
  echo "  ██║    ██║██╔══██╗██╔══██╗████╗  ██║██║████╗  ██║██╔════╝ "
  echo "  ██║ █╗ ██║███████║██████╔╝██╔██╗ ██║██║██╔██╗ ██║██║  ███╗"
  echo "  ██║███╗██║██╔══██║██╔══██╗██║╚██╗██║██║██║╚██╗██║██║   ██║"
  echo "  ╚███╔███╔╝██║  ██║██║  ██║██║ ╚████║██║██║ ╚████║╚██████╔╝"
  echo "   ╚══╝╚══╝ ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═══╝╚═╝╚═╝  ╚═══╝ ╚═════╝ "
  echo -e "${NC}"
  echo "  This will COMPLETELY remove Longhorn from the cluster."
  echo "  NFS backups and replica dirs on disk are preserved."
  echo ""
  echo "  Namespace : ${LONGHORN_NAMESPACE}"
  echo "  Cluster   : $(kubectl config current-context 2>/dev/null || echo 'unknown')"
  echo ""
  read -rp "  Type 'nuke' to confirm: " answer
  [[ "$answer" == "nuke" ]] || die "Aborted."
fi

# ── Step 1: Disable ArgoCD self-heal so it doesn't fight us ──────────────────
log "Step 1/9: Disabling ArgoCD self-heal for Longhorn..."
kubectl patch application longhorn -n "${ARGOCD_NAMESPACE}" \
  --type=merge \
  -p '{"spec":{"syncPolicy":{"automated":{"selfHeal":false,"prune":false}}}}' \
  2>/dev/null && ok "ArgoCD self-heal disabled" \
  || warn "ArgoCD application 'longhorn' not found — skipping (manual install?)"

# ── Step 2: Kill Longhorn processes before touching finalizers ────────────────
# Must kill manager FIRST — if it's running it re-adds finalizers faster than
# we can remove them, and webhooks will block our patches.
log "Step 2/9: Killing Longhorn manager and driver..."
kubectl delete daemonset longhorn-manager -n "${LONGHORN_NAMESPACE}" --ignore-not-found
kubectl delete deployment longhorn-driver-deployer longhorn-ui -n "${LONGHORN_NAMESPACE}" --ignore-not-found

log "  Waiting for manager pods to terminate..."
kubectl wait --for=delete pod -l app=longhorn-manager \
  -n "${LONGHORN_NAMESPACE}" --timeout=90s 2>/dev/null \
  && ok "Manager pods gone" \
  || warn "Timed out waiting — continuing anyway"

# ── Step 3: Remove webhooks (they block patches when manager is dead) ─────────
log "Step 3/9: Removing admission webhooks..."
kubectl delete mutatingwebhookconfiguration longhorn-webhook-mutator --ignore-not-found
kubectl delete validatingwebhookconfiguration longhorn-webhook-validator --ignore-not-found
ok "Webhooks removed"

# ── Step 3b: Set deleting-confirmation-flag ──────────────────────────────────
# Longhorn's uninstall job won't run without this set to true.
# Must be done AFTER webhooks are removed (webhook would block the patch).
log "Step 3b/9: Setting deleting-confirmation-flag..."
kubectl patch settings.longhorn.io deleting-confirmation-flag \
  -n "${LONGHORN_NAMESPACE}" \
  --type=merge \
  -p '{"value":"true"}' \
  2>/dev/null && ok "deleting-confirmation-flag set to true" \
  || warn "Could not patch setting — may already be gone"

# ── Step 4: Strip finalizers from all Longhorn CRs ───────────────────────────
# Must be done before deleting the namespace — otherwise CRs hang in
# Terminating forever because the controller that handles them is already dead.
log "Step 4/9: Stripping finalizers from Longhorn CRs..."

strip_finalizers() {
  local kind="$1"
  local resources
  resources=$(kubectl get "${kind}" -n "${LONGHORN_NAMESPACE}" \
    --no-headers -o custom-columns=":metadata.name" 2>/dev/null) || return 0
  for name in $resources; do
    kubectl patch "${kind}" "${name}" -n "${LONGHORN_NAMESPACE}" \
      --type=json \
      -p '[{"op":"remove","path":"/metadata/finalizers"}]' \
      2>/dev/null && echo "  stripped ${kind}/${name}" || true
  done
}

strip_finalizers "volumes.longhorn.io"
strip_finalizers "replicas.longhorn.io"
strip_finalizers "engines.longhorn.io"
strip_finalizers "engineimages.longhorn.io"
strip_finalizers "backuptargets.longhorn.io"
strip_finalizers "backupvolumes.longhorn.io"
strip_finalizers "backups.longhorn.io"
strip_finalizers "snapshots.longhorn.io"
strip_finalizers "nodes.longhorn.io"
strip_finalizers "instancemanagers.longhorn.io"
strip_finalizers "sharemanagers.longhorn.io"
strip_finalizers "recurringjobs.longhorn.io"
strip_finalizers "orphans.longhorn.io"
strip_finalizers "systembackups.longhorn.io"
strip_finalizers "systemrestores.longhorn.io"
strip_finalizers "volumeattachments.longhorn.io"
ok "Finalizers stripped"

# ── Step 5: Delete ArgoCD app + extras ───────────────────────────────────────
log "Step 5/9: Deleting ArgoCD Longhorn application..."
kubectl delete application longhorn -n "${ARGOCD_NAMESPACE}" --ignore-not-found
kubectl delete -k "${SCRIPT_DIR}/longhorn-extra" --ignore-not-found 2>/dev/null || true
kubectl delete -k "${SCRIPT_DIR}/longhorn-smb-operator" --ignore-not-found 2>/dev/null || true
ok "ArgoCD app + extras deleted"

# ── Step 6: Delete the namespace (triggers uninstall job) ────────────────────
log "Step 6/9: Deleting namespace '${LONGHORN_NAMESPACE}'..."
kubectl delete namespace "${LONGHORN_NAMESPACE}" --ignore-not-found

log "  Waiting for namespace to terminate (uninstall job runs here, can take ~60s)..."
for i in $(seq 1 60); do
  ns=$(kubectl get namespace "${LONGHORN_NAMESPACE}" --ignore-not-found \
    --no-headers -o custom-columns=":metadata.name" 2>/dev/null)
  [[ -z "$ns" ]] && break

  echo -n "  ."
  sleep 3
done
echo ""
kubectl get namespace "${LONGHORN_NAMESPACE}" --ignore-not-found --no-headers \
  | grep -q . && warn "Namespace still exists — may need manual cleanup" \
  || ok "Namespace gone"

# ── Step 7: Delete cluster-scoped resources (PVs survive namespace deletion) ──
log "Step 7/9: Cleaning up cluster-scoped PVs..."
longhorn_pvs=$(kubectl get pv --no-headers \
  -o custom-columns=":metadata.name,:spec.csi.driver" 2>/dev/null \
  | grep "driver.longhorn.io" | awk '{print $1}') || true

if [[ -n "$longhorn_pvs" ]]; then
  for pv in $longhorn_pvs; do
    warn "  Deleting PV: ${pv}"
    kubectl patch pv "${pv}" --type=json \
      -p '[{"op":"remove","path":"/metadata/finalizers"}]' 2>/dev/null || true
    kubectl delete pv "${pv}" --ignore-not-found
  done
  ok "PVs deleted"
else
  ok "No Longhorn PVs found"
fi

# ── Step 8: Delete all Longhorn CRDs ─────────────────────────────────────────
log "Step 8/9: Deleting Longhorn CRDs..."
longhorn_crds=$(kubectl get crd --no-headers -o custom-columns=":metadata.name" \
  2>/dev/null | grep "longhorn.io") || true

if [[ -n "$longhorn_crds" ]]; then
  for crd in $longhorn_crds; do
    # Strip CRD finalizers first (volumeattachments CRD is a known blocker)
    kubectl patch crd "${crd}" --type=json \
      -p '[{"op":"remove","path":"/metadata/finalizers"}]' 2>/dev/null || true
    kubectl delete crd "${crd}" --ignore-not-found &
  done
  wait
  ok "CRDs deleted"
else
  ok "No Longhorn CRDs found"
fi

# ── Step 9: Re-enable ArgoCD self-heal ───────────────────────────────────────
# (won't do anything since the app is deleted, but good hygiene if app is re-applied)
log "Step 9/9: Done."
echo ""
echo -e "${GREEN}════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Longhorn has been nuked.${NC}"
echo -e "${GREEN}════════════════════════════════════════════════════════${NC}"
echo ""
echo "  Replica dirs on /mnt/longhorn are intact on each node."
echo "  NFS backups on your NAS are intact."
echo ""
echo "  To reinstall Longhorn from Git:"
echo "    cd system/storage"
echo "    kubectl apply -f longhorn"
echo "    kubectl rollout status daemonset/longhorn-manager -n storage --timeout=300s"
echo "    kubectl apply -k longhorn-extra"
echo "    kubectl apply -k longhorn-smb-operator"
echo ""
echo "  To restore from system backup:"
echo "    # Wait for Longhorn manager to be Ready, then:"
echo "    kubectl get systembackups -n storage  # find latest backup name"
echo "    kubectl apply -f - <<EOF"
echo "    apiVersion: longhorn.io/v1beta2"
echo "    kind: SystemRestore"
echo "    metadata:"
echo "      name: restore-\$(date +%Y%m%d)"
echo "      namespace: storage"
echo "    spec:"
echo "      systemBackup: <backup-name-from-above>"
echo "    EOF"
echo ""
