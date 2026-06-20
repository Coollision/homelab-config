# Longhorn Disaster Recovery Test

Proves that Longhorn data survives complete k8s-side deletion (namespace, PVC, PV)
and can be reattached by re-applying the same file.

## File

`dr-test.yaml` — single file containing namespace, static PV, PVC, and a smart pod.

The pod checks on startup:

- **No file found** → fresh run, writes a timestamp, prints next steps
- **File found** → recovery run, reads the original timestamp, appends a recovery timestamp, prints `✅ DR TEST PASSED`

## Workflow

```bash
# --- Phase 1: Fresh run ---

kubectl apply -f dr-test.yaml
kubectl wait pod/dr-test -n dr-test --for=condition=Ready --timeout=60s
kubectl logs dr-test -n dr-test
# => FRESH RUN — writes timestamp

# --- Phase 2: Destroy everything ---

kubectl delete -f dr-test.yaml
kubectl delete pv dr-test-volume 2>/dev/null

# Verify the Longhorn volume is still alive (detached, not deleted)
kubectl get volumes.longhorn.io dr-test-volume -n storage -o jsonpath='{.status.state}'
# => detached  ✅

# --- Phase 3: Recover ---

kubectl apply -f dr-test.yaml
kubectl wait pod/dr-test -n dr-test --for=condition=Ready --timeout=60s
kubectl logs dr-test -n dr-test
# => RECOVERY RUN — reads original timestamp, appends recovery entry, prints ✅ DR TEST PASSED

# --- Cleanup ---
kubectl delete -f dr-test.yaml
kubectl delete pv dr-test-volume 2>/dev/null
kubectl delete volumes.longhorn.io dr-test-volume -n storage
```
