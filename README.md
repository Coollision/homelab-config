# cert-push-image (orphan branch)

Tools-only container image used by the `cert-push` CronJob of the GitOps repo
(`system/default/cert-push`): Alpine + `bash`, `curl`, `openssl`, `jq`, `ca-certificates`, non-root (65532), no `apk`.
The push script itself lives in the GitOps repo (ConfigMap), not here.

Published as `ghcr.io/coollision/cert-push` by `.github/workflows/cert-push-image.yaml` on every push touching
`cert-push-image/**`, and weekly (to pick up Alpine package fixes). The build fails on fixable HIGH/CRITICAL findings.
Consumers pin the image by digest; Renovate bumps the digest.
