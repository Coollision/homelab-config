---
paths:
  - "applications/**"
  - "argocd/**"
  - "tests/README.md"
---

# ArgoCD / ApplicationSets

- ApplicationSets discover dirs by glob; app name = dir basename, namespace = parent dir. Do not hand-write Application CRs for discoverable apps.
- `ignoreApplicationDifferences` on `.spec.syncPolicy` lets maintenance mode (auto-sync off in UI) stick.
- The `scale-to-zero autogen` region is generated: edit app `values.yaml`, run `scripts/gen-scale-to-zero-ignores.sh`.
- ArgoCD repo-server is memory-tight (1 replica, bounded controller processors). Prefer OCI chart deps over classic Helm repo indexes; never raise render concurrency.
- An app can be owned by only one ApplicationSet: moving `tests/` → master requires removing the ownerReference (skill `promote-test-app`).
- Changes here are cluster-wide blast radius: show the diff and expected effect, wait for OK before pushing.
