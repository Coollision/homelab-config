# Copilot Instructions: storage-operator Orphan Branch

Scope: these instructions apply to work on branch `orphan/storage-operator-standalone`.

## Working Scope

- Treat `storage-operator/` as the main product surface.
- Prefer changes inside:
  - `storage-operator/**`
  - `.github/workflows/storage-operator.yaml`
- Do not modify unrelated stacks/charts unless explicitly requested.

## Versioning Rules

- Never use `latest` image tags.
- Use immutable semantic versions for both image and chart.
- Keep these values aligned:
  - `storage-operator/chart/Chart.yaml` `version`
  - `storage-operator/chart/Chart.yaml` `appVersion`
  - `storage-operator/chart/values.yaml` `image.tag` as `vX.Y.Z`
- Release tags must use format: `storage-operator/vX.Y.Z`.

## Semantic Commit Conventions

Use Conventional Commits to drive automated version bumps:

- `feat:` -> minor bump
- `fix:`, `chore:`, `refactor:`, `docs:`, `ci:`, `test:` -> patch bump
- `!` or `BREAKING CHANGE:` -> major bump
- CI enforces this convention on push for operator/workflow related commits.

Examples:

- `feat(smb): add share attach metric`
- `fix(chart): correct serviceAccount labels`
- `feat(config)!: drop smb.security`

## CI/CD Expectations

- Build Go binaries outside Docker for speed (`linux/amd64`, `linux/arm64`).
- Build/push multi-arch image by copying prebuilt binaries into runtime image.
- Publish chart to OCI using semver chart versions (no SHA-only chart versioning).
- Keep image tags immutable and include commit SHA tags for traceability.
- Automatically create release tags in the format `storage-operator/vX.Y.Z` after successful pipeline runs.

## PR/Change Hygiene

- Keep changes focused and minimal.
- When changing behavior, update docs in `storage-operator/README.md`.
- Preserve compatibility of existing chart values unless migration is intentional and documented.

## Parity Rules (SMB controller)

- The SMB controller is a drop-in replacement for the Python `smb_operator_v2.py`. Rendered
  smb.conf, mount table, scripts, Deployment and Service must keep matching the golden files in
  `storage-operator/internal/render/testdata/`.
- Never change anything in the smb-server pod template (it rolls the SMB pod) unless that is the
  explicit goal; `smb-operator/version: "2.1"` stays.
- Behaviour changes go behind a flag that defaults to the current behaviour.
- No private domains, IPs, hostnames or emails anywhere (public repo); use placeholders and
  RFC 5737 documentation IPs in tests.
