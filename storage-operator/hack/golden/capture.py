#!/usr/bin/env python3
"""Capture golden files from the Python SMB operator (smb_operator_v2.py).

Usage (no third-party packages needed; kubernetes + yaml are stubbed):

    python3 -I hack/golden/capture.py <path/to/smb_operator_v2.py> internal/render/testdata

The Go renderers and discovery code must reproduce these files byte-for-byte
(text outputs) or semantically (JSON objects). See DEV.md "Refreshing golden files".
"""

import importlib.util
import json
import os
import sys
import types
from types import SimpleNamespace as NS

# --------------------------------------------------------------------------- stubs


class ApiException(Exception):
    def __init__(self, status=None, reason=None):
        super().__init__(f"({status}) {reason}")
        self.status = status
        self.reason = reason


SPECIAL_KEYS = {"_exec": "exec", "load_balancer_ip": "loadBalancerIP"}


def camel(key):
    if key in SPECIAL_KEYS:
        return SPECIAL_KEYS[key]
    head, *rest = key.split("_")
    return head + "".join(p[:1].upper() + p[1:] for p in rest)


class Model:
    """Generic stand-in for every kubernetes.client.V1* model."""

    def __init__(self, **kwargs):
        self._kw = kwargs
        for k, v in kwargs.items():
            setattr(self, k, v)


def serialize(obj):
    if isinstance(obj, Model):
        return {camel(k): serialize(v) for k, v in obj._kw.items() if v is not None}
    if isinstance(obj, dict):
        return {k: serialize(v) for k, v in obj.items() if v is not None}
    if isinstance(obj, list):
        return [serialize(v) for v in obj]
    return obj


client_mod = types.ModuleType("kubernetes.client")


def _model_factory(name):
    return type(name, (Model,), {})


class _ClientModule(types.ModuleType):
    def __getattr__(self, name):
        if name.startswith("V1") or name.startswith("CoreV1"):
            cls = _model_factory(name)
            setattr(self, name, cls)
            return cls
        raise AttributeError(name)


client_mod = _ClientModule("kubernetes.client")
rest_mod = types.ModuleType("kubernetes.client.rest")
rest_mod.ApiException = ApiException
client_mod.rest = rest_mod
config_mod = types.ModuleType("kubernetes.config")
config_mod.ConfigException = Exception
watch_mod = types.ModuleType("kubernetes.watch")
k8s_mod = types.ModuleType("kubernetes")
k8s_mod.client = client_mod
k8s_mod.config = config_mod
k8s_mod.watch = watch_mod
yaml_mod = types.ModuleType("yaml")
yaml_mod.safe_load = lambda s: json.loads(s)  # migration fixtures are JSON (a YAML subset)

sys.modules.update({
    "kubernetes": k8s_mod,
    "kubernetes.client": client_mod,
    "kubernetes.client.rest": rest_mod,
    "kubernetes.config": config_mod,
    "kubernetes.watch": watch_mod,
    "yaml": yaml_mod,
})

# --------------------------------------------------------------------------- fake API


class FakeCore:
    """Records writes; reads come from the scenario."""

    def __init__(self, scenario):
        self.s = scenario
        self.annotations = {}
        self.events = []
        self.created = []

    # PVCs
    def list_persistent_volume_claim_for_all_namespaces(self, label_selector=None):
        items = []
        for p in self.s.get("pvcs", []):
            items.append(NS(
                metadata=NS(namespace=p["namespace"], name=p["name"], labels=p.get("labels"),
                            annotations=None),
                spec=NS(volume_name=p.get("volumeName", "")),
            ))
        return NS(items=items)

    def read_namespaced_persistent_volume_claim(self, name, ns):
        if self.s.get("pvcsExist", True) and any(
                p["name"] == name and p["namespace"] == ns for p in self.s.get("pvcs", [])):
            return NS(metadata=NS(annotations=None))
        raise ApiException(404)

    def patch_namespaced_persistent_volume_claim(self, name, ns, body):
        a = dict(body.metadata.annotations)
        a.pop("smb-operator/last-update", None)
        self.annotations[f"{ns}/{name}"] = a

    def create_namespaced_event(self, ns, ev):
        d = serialize(ev)
        self.events.append({
            "namespace": ns,
            "kind": d["involvedObject"]["kind"],
            "name": d["involvedObject"]["name"],
            "reason": d["reason"],
            "message": d["message"],
            "type": d["type"],
            "component": d["source"]["component"],
        })

    def read_namespaced_config_map(self, name, ns):
        if name == "smb-operator-migration" and "migration" in self.s:
            return NS(data={"volumes.yaml": json.dumps(self.s["migration"])})
        raise ApiException(404)

    # creation capture (service / PV / PVC)
    def read_namespaced_service(self, name, ns):
        raise ApiException(404)

    def create_namespaced_service(self, ns, svc):
        self.created.append(("service", serialize(svc)))

    def list_namespaced_persistent_volume_claim(self, ns, label_selector=None):
        return NS(items=[])

    def list_persistent_volume(self, label_selector=None):
        return NS(items=[])

    def read_persistent_volume(self, name):
        raise ApiException(404)

    def create_persistent_volume(self, pv):
        self.created.append(("pv", serialize(pv)))

    def create_namespaced_persistent_volume_claim(self, ns, pvc):
        self.created.append(("pvc", serialize(pvc)))


class FakeCustom:
    def __init__(self, scenario):
        self.vols = scenario.get("volumes", {})

    def get_namespaced_custom_object(self, group, version, namespace, plural, name):
        v = self.vols.get(name)
        if v is None or v.get("namespace", "storage") != namespace:
            raise ApiException(404)
        return {"spec": v.get("spec", {}), "status": v.get("status", {})}


# --------------------------------------------------------------------------- helpers


def load_operator(path):
    spec = importlib.util.spec_from_file_location("smb_operator_v2", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def make_op(mod, cfg, scenario=None):
    op = object.__new__(mod.SMBOperator)
    op.config = cfg
    op.current_shares = set()
    op.current_structure = set()
    op.operator_uid = "00000000-0000-0000-0000-00000000beef"
    op.operator_labels = {
        "app": "smb-operator",
        "app.kubernetes.io/part-of": "longhorn-smb-operator",
    }
    scenario = scenario or {}
    op.core_v1 = FakeCore(scenario)
    op.custom_api = FakeCustom(scenario)
    return op


def write(outdir, name, content):
    with open(os.path.join(outdir, name), "w", newline="") as f:
        f.write(content)


def dump_json(obj):
    return json.dumps(obj, indent=2, sort_keys=True) + "\n"


def main():
    src, outdir = sys.argv[1], sys.argv[2]
    mod = load_operator(src)
    os.makedirs(outdir, exist_ok=True)

    with open(os.path.join(outdir, "configs.json")) as f:
        configs = json.load(f)
    with open(os.path.join(outdir, "share_fixtures.json")) as f:
        fixtures = json.load(f)

    for cfg_name, c in configs.items():
        cfg = mod.Config(
            namespace=c["namespace"],
            smb_loadbalancer_ip=c["lbIP"],
            smb_username=c["username"],
            smb_password="unused",
            **{k: v for k, v in c.get("py", {}).items()},
        )
        for fx_name, fx in fixtures.items():
            shares = [mod.SMBShare(**s) for s in fx]
            op = make_op(mod, cfg)
            base = f"{cfg_name}__{fx_name}"
            write(outdir, base + ".smb.conf", op.generate_smb_config(shares))
            write(outdir, base + ".mounts.tsv", op.generate_mount_table(shares))
            write(outdir, base + ".startup.sh", op.generate_startup_script(shares))
            write(outdir, base + ".prestop.sh", op.generate_prestop_script(shares))
            dep = serialize(op.generate_deployment(shares))
            dep["metadata"]["annotations"].pop("smb-operator/last-updated")
            write(outdir, base + ".deployment.json", dump_json(dep))
        # Service (create path) + migration PV/PVC (create path)
        op = make_op(mod, cfg, {"volumes": {"vol-direct": {"spec": {"size": "5368709120"}},
                                            "vol-badsize": {"spec": {"size": "abc"}}}})
        op.ensure_service()
        direct = [mod.SMBShare(name="d", path="/shares/d", namespace=cfg.namespace,
                               pvc_name="migration-vol-direct", access_mode="shared",
                               mount_type="direct", longhorn_volume_name="vol-direct"),
                  mod.SMBShare(name="b", path="/shares/b", namespace=cfg.namespace,
                               pvc_name="migration-vol-badsize", access_mode="read-only",
                               mount_type="direct", longhorn_volume_name="vol-badsize")]
        op.ensure_migration_pvcs(direct)
        for i, (kind, obj) in enumerate(op.core_v1.created):
            write(outdir, f"{cfg_name}__create_{i:02d}_{kind}.json", dump_json(obj))

    # Discovery scenarios
    with open(os.path.join(outdir, "discovery_input.json")) as f:
        scenarios = json.load(f)
    cfg = mod.Config(namespace="storage", smb_loadbalancer_ip="192.0.2.10",
                     smb_username="smbuser", smb_password="unused")
    for sc_name, sc in scenarios.items():
        op = make_op(mod, cfg, sc)
        shares = op.discover_shares() + op.discover_migration_shares()
        out = {
            "shares": [{
                "name": s.name, "path": s.path, "namespace": s.namespace,
                "pvc_name": s.pvc_name, "access_mode": s.access_mode,
                "mount_type": s.mount_type, "nfs_server": s.nfs_server,
                "nfs_path": s.nfs_path, "longhorn_volume_name": s.longhorn_volume_name,
                "attached": s.attached, "structural_key": s.structural_key,
                "endpoint": s.endpoint,
            } for s in shares],
            "annotations": op.core_v1.annotations,
            "events": op.core_v1.events,
        }
        write(outdir, f"discovery__{sc_name}.json", dump_json(out))


if __name__ == "__main__":
    main()
