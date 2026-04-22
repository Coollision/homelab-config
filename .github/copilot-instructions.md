# Copilot Instructions: aws-tunnels-operator Orphan Branch

Scope: these instructions apply to work on branch `orphan/aws-tunnels-operator-standalone`.

## Working Scope

- Treat `aws-tunnels-operator/` as the main product surface.
- Prefer changes inside:
  - `aws-tunnels-operator/**`
  - `.github/workflows/aws-tunnels-operator.yaml`
- Do not modify unrelated stacks/charts unless explicitly requested.

## Versioning Rules

- Never use `latest` image tags.
- Use immutable semantic versions for both image and chart.
- Keep these values aligned:
  - `aws-tunnels-operator/chart/Chart.yaml` `version`
  - `aws-tunnels-operator/chart/Chart.yaml` `appVersion`
  - `aws-tunnels-operator/chart/values.yaml` `image.tag` as `vX.Y.Z`
- Release tags must use format: `aws-tunnels-operator/vX.Y.Z`.

## Semantic Commit Conventions

Use Conventional Commits to drive automated version bumps:

- `feat:` -> minor bump
- `fix:`, `chore:`, `refactor:`, `docs:`, `ci:`, `test:` -> patch bump
- `!` or `BREAKING CHANGE:` -> major bump

Examples:

- `feat(operator): add arm64 tunnel scheduler support`
- `fix(chart): correct serviceAccount labels`
- `feat(api)!: rename auth payload fields`

## CI/CD Expectations

- Build Go binaries outside Docker for speed (`linux/amd64`, `linux/arm64`).
- Build/push multi-arch image by copying prebuilt binaries into runtime image.
- Publish chart to OCI using semver chart versions (no SHA-only chart versioning).
- Keep image tags immutable and include commit SHA tags for traceability.

## PR/Change Hygiene

- Keep changes focused and minimal.
- When changing behavior, update docs in `aws-tunnels-operator/README.md`.
- Preserve compatibility of existing chart values unless migration is intentional and documented.
