# Tests

This folder contains applications deployed from the `tests` branch via the `tests` ApplicationSet. It allows rapid iteration without committing to `master`.

## Moving an app from `tests` to `system` or `workload`

When an app is ready to move to production (`system/` or `workload/` on `master`), the ArgoCD Application is **owned** by the `tests` ApplicationSet. The `system-config` or `workloads` ApplicationSet can't adopt it while that ownership exists.

### Steps to transfer without downtime

1. **Copy** the app folder to its new location on `master` (e.g. `system/storage/longhorn/`) and push.

2. **Remove the ownerReference** from the Application so the `tests` ApplicationSet releases control:

   ```bash
   kubectl patch application <app-name> -n argocd --type=json \
     -p='[{"op": "remove", "path": "/metadata/ownerReferences"}]'
   ```

3. The target ApplicationSet (`system-config` or `workloads`) will automatically adopt the orphaned Application on its next reconciliation (~3 minutes). No resources are deleted or recreated.

4. **Clean up** the old folder on the `tests` branch — either delete it or rename it to `disabled-<name>`. Since ownership was already transferred, this won't affect the running resources.

### Example: moving longhorn to system

```bash
# 1. Copy the folder to system/storage/longhorn on master and push

# 2. Remove ownership from the tests ApplicationSet
kubectl patch application longhorn -n argocd --type=json \
  -p='[{"op": "remove", "path": "/metadata/ownerReferences"}]'

# 3. system-config adopts it automatically

# 4. Verify new ownership
kubectl get application longhorn -n argocd \
  -o jsonpath='{.metadata.ownerReferences[0].name}'
# Should output: system-config

# 5. Remove or disable tests/storage/longhorn on the tests branch
```

## Maintenance mode

All three ApplicationSets (`system-config`, `workloads`, `tests`) have `ignoreApplicationDifferences` set for `.spec.syncPolicy`. This means you can **disable auto-sync directly in the ArgoCD UI** and it will stick — the ApplicationSet won't overwrite it back.
