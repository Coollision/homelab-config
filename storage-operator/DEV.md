# storage-operator — Developer Notes

## Layout

```
main.go                    flags, manager, cache restrictions, controller toggles
controllers/smb/           SMB controller (reconciler, predicates, metrics, setup)
internal/config/           config.yaml parsing (pure)
internal/shares/           Share model + discovery decisions (pure)
internal/render/           smb.conf, mounts.tsv, scripts, Deployment/Service/PV/PVC (pure)
internal/render/scripts/   mount-reconciler.sh.tmpl (go:embed, delimiters <% %>)
internal/render/testdata/  golden files captured from the Python operator
hack/golden/capture.py     the capture harness
```

## Tests

```bash
go vet ./... && go test -race ./...
helm lint chart && helm template x chart
```

- `internal/render`: golden parity (text byte-for-byte, objects via `equality.Semantic`) for
  3 configs x 7 share fixtures, plus shellcheck on every rendered script (skipped if not installed).
- `controllers/smb`: fake-client tests: discovery parity with the Python capture (shares,
  annotations, events), detach touches only the mount table, restart re-apply keeps the pod
  template, migration PV/PVC create + GC, replace-vs-patch, predicates, readiness/metrics.
- envtest is not used yet: the assets were not available offline when this was written. The
  fake-client tests cover the same flows except real strategic-merge/defaulting on the apiserver;
  the `--dry-run` run against the cluster covers that.

## Refreshing golden files

The harness stubs `kubernetes` and `yaml`, so it needs no packages:

```bash
python3 -I hack/golden/capture.py <path-to>/smb_operator_v2.py internal/render/testdata
go test ./...
```

Inputs live next to the outputs: `configs.json` (the `py` block feeds the Python dataclass, the
`yaml` block feeds the Go parser; keep them equivalent), `share_fixtures.json`,
`discovery_input.json`. The Python source is the spec: if it changes, re-capture and make Go match.

## Dry run against the cluster (milestone M4)

```bash
SMB_LOADBALANCER_IP=... SMB_USERNAME=... SMB_PASSWORD=... \
  go run . --kubeconfig ~/.kube/config --dry-run --zap-log-level=debug
```

Every write is sent with `dryRun=All`, so the apiserver validates and defaults it but stores
nothing. The log shows `DRY-RUN: smb-server pod template unchanged (no pod roll)` or a diff, and
diffs for `smb.conf` and `mounts.tsv`. Leader election is off in dry-run mode, so it can run next
to the live operator. Expected on the first pass: the pod template is unchanged; only the
Deployment-level `smb-operator/last-updated` annotation differs.

## Design notes

- **Single key.** Every watch (labelled PVCs, the migration ConfigMap, Longhorn Volume status
  tuple changes, a start-up kick) maps to one request, so reconciles are serialised and storms
  collapse. `RequeueAfter: reconcileInterval` is the periodic resync.
- **Live reads.** Reconcile reads go through the uncached API reader like the Python API calls;
  the informer cache only triggers reconciles and is restricted (PVCs with `smb-access`,
  ConfigMaps in the operator namespace, Volumes in the Longhorn namespace).
- **Deployment patch.** The full desired object is sent as a strategic-merge patch (what the
  Python client did), minus `creationTimestamp: null` / `status`. Replace when the strategy,
  volume names or mount paths change, because strategic merge cannot remove list entries.
- **In-memory structure.** The structural key set starts empty, like Python: zero shares on a
  fresh start is "no change" (no Deployment created), and the first non-empty reconcile after a
  restart re-applies the Deployment (no-op for the pod template).
