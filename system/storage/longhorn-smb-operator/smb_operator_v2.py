#!/usr/bin/env python3
"""
SMB Operator - Enhanced version with ConfigMap, Retry Logic, and Events

Features:
- ConfigMap-based configuration (no hardcoded values)
- Retry logic with exponential backoff
- Kubernetes event recording
- PVC status annotations
- Deployment annotations
"""

import logging
import time
import sys
import os
import signal
import threading
import yaml
from http.server import HTTPServer, BaseHTTPRequestHandler
from typing import List, Dict, Optional, Set, Any
from dataclasses import dataclass
from datetime import datetime
from functools import wraps
from kubernetes import client, config, watch
from kubernetes.client.rest import ApiException

# Configure logging
LOG_LEVEL = os.getenv('LOG_LEVEL', 'INFO')
logging.basicConfig(
    level=getattr(logging, LOG_LEVEL),
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s',
    handlers=[logging.StreamHandler(sys.stdout)]
)
logger = logging.getLogger('smb-operator')

# Global state
shutdown_event = threading.Event()
healthy = False
ready = False

# Required configuration keys (must be set in ConfigMap)
REQUIRED_CONFIG_KEYS = [
    ('operator', 'namespace'),
]

# Metrics
class Metrics:
    """Simple metrics tracking"""
    def __init__(self):
        self.reconcile_count = 0
        self.reconcile_errors = 0
        self.shares_managed = 0
        self.last_reconcile_time = 0
        self.retry_count = 0
        self.events_sent = 0
    
metrics = Metrics()


@dataclass
class Config:
    """Operator configuration from ConfigMap + env vars for secrets"""
    # Required settings (must be provided in ConfigMap)
    namespace: str
    smb_loadbalancer_ip: str
    
    # SMB credentials (from env vars, sourced from K8s Secret)
    smb_username: str = ''
    smb_password: str = ''
    
    # Operator settings (with defaults)
    reconcile_interval: int = 30
    use_watch_api: bool = True
    log_level: str = 'INFO'
    health_port: int = 8080
    
    # SMB settings (with defaults)
    smb_deployment_name: str = 'smb-server'
    smb_service_name: str = 'smb-server'
    smb_image: str = 'dperson/samba:latest'
    smb_workgroup: str = 'WORKGROUP'
    smb_server_string: str = 'Homelab Storage'
    
    # Longhorn settings
    longhorn_namespace: str = 'storage'
    longhorn_group: str = 'longhorn.io'
    longhorn_version: str = 'v1beta2'
    longhorn_plural: str = 'volumes'
    
    # Retry settings
    max_retries: int = 3
    initial_delay: int = 1
    backoff_multiplier: int = 2
    retryable_status_codes: List[int] = None
    
    # Resource limits
    smb_resources: Dict[str, Any] = None
    operator_resources: Dict[str, Any] = None
    
    def __post_init__(self):
        if self.retryable_status_codes is None:
            self.retryable_status_codes = [409, 429, 500, 503, 504]
        if self.smb_resources is None:
            self.smb_resources = {
                'requests': {'cpu': '100m', 'memory': '128Mi'},
                'limits': {'cpu': '500m', 'memory': '512Mi'}
            }
        if self.operator_resources is None:
            self.operator_resources = {
                'requests': {'cpu': '50m', 'memory': '128Mi'},
                'limits': {'cpu': '200m', 'memory': '256Mi'}
            }
    
    @classmethod
    def load_from_configmap(cls, core_v1: client.CoreV1Api, namespace: str, name: str) -> 'Config':
        """Load configuration from ConfigMap + env vars for secrets."""
        try:
            cm = core_v1.read_namespaced_config_map(name, namespace)
            config_yaml = cm.data.get('config.yaml', '')
            if not config_yaml:
                raise ValueError(f"ConfigMap '{name}' in namespace '{namespace}' is empty. Required keys: {REQUIRED_CONFIG_KEYS}")
            
            data = yaml.safe_load(config_yaml)
            
            # Validate required configuration keys
            missing_keys = []
            for section, key in REQUIRED_CONFIG_KEYS:
                if not data.get(section, {}).get(key):
                    missing_keys.append(f"{section}.{key}")
            
            if missing_keys:
                raise ValueError(f"Missing required configuration keys in ConfigMap '{name}': {', '.join(missing_keys)}")
            
            # Read secrets from environment (injected from K8s Secret)
            smb_loadbalancer_ip = os.getenv('SMB_LOADBALANCER_IP', '')
            smb_username = os.getenv('SMB_USERNAME', '')
            smb_password = os.getenv('SMB_PASSWORD', '')
            
            if not smb_loadbalancer_ip:
                raise ValueError("SMB_LOADBALANCER_IP env var is required (from smb-credentials Secret)")
            if not smb_username or not smb_password:
                raise ValueError("SMB_USERNAME and SMB_PASSWORD env vars are required (from smb-credentials Secret)")
            
            return cls(
                namespace=data['operator']['namespace'],
                smb_loadbalancer_ip=smb_loadbalancer_ip,
                smb_username=smb_username,
                smb_password=smb_password,
                reconcile_interval=data.get('operator', {}).get('reconcileInterval', 30),
                use_watch_api=data.get('operator', {}).get('useWatchAPI', True),
                log_level=data.get('operator', {}).get('logLevel', 'INFO'),
                health_port=data.get('operator', {}).get('healthPort', 8080),
                
                smb_deployment_name=data.get('smb', {}).get('deploymentName', 'smb-server'),
                smb_service_name=data.get('smb', {}).get('serviceName', 'smb-server'),
                smb_image=data.get('smb', {}).get('image', 'dperson/samba:latest'),
                smb_workgroup=data.get('smb', {}).get('workgroup', 'WORKGROUP'),
                smb_server_string=data.get('smb', {}).get('serverString', 'Homelab Storage'),
                
                longhorn_namespace=data.get('longhorn', {}).get('namespace', 'storage'),
                longhorn_group=data.get('longhorn', {}).get('group', 'longhorn.io'),
                longhorn_version=data.get('longhorn', {}).get('version', 'v1beta2'),
                longhorn_plural=data.get('longhorn', {}).get('plural', 'volumes'),
                
                max_retries=data.get('retry', {}).get('maxRetries', 3),
                initial_delay=data.get('retry', {}).get('initialDelay', 1),
                backoff_multiplier=data.get('retry', {}).get('backoffMultiplier', 2),
                retryable_status_codes=data.get('retry', {}).get('retryableStatusCodes', [409, 429, 500, 503, 504]),
                
                smb_resources=data.get('resources', {}).get('smbServer'),
                operator_resources=data.get('resources', {}).get('operator'),
            )
        except ApiException as e:
            if e.status == 404:
                raise ValueError(f"ConfigMap '{name}' not found in namespace '{namespace}'. This ConfigMap is required.")
            raise
        except ValueError:
            raise
        except Exception as e:
            raise ValueError(f"Failed to load ConfigMap '{name}': {e}")


class HealthHandler(BaseHTTPRequestHandler):
    """HTTP handler for health and readiness probes"""
    
    def log_message(self, format, *args):
        """Suppress default logging"""
        pass
    
    def do_GET(self):
        global healthy, ready, metrics
        
        if self.path == '/healthz':
            if healthy:
                self.send_response(200)
                self.send_header('Content-type', 'text/plain')
                self.end_headers()
                self.wfile.write(b'OK')
            else:
                self.send_response(503)
                self.send_header('Content-type', 'text/plain')
                self.end_headers()
                self.wfile.write(b'Not healthy')
        
        elif self.path == '/readyz':
            if ready:
                self.send_response(200)
                self.send_header('Content-type', 'text/plain')
                self.end_headers()
                self.wfile.write(b'Ready')
            else:
                self.send_response(503)
                self.send_header('Content-type', 'text/plain')
                self.end_headers()
                self.wfile.write(b'Not ready')
        
        elif self.path == '/metrics':
            self.send_response(200)
            self.send_header('Content-type', 'text/plain')
            self.end_headers()
            metrics_text = f"""# HELP smb_operator_reconcile_total Total number of reconciliations
# TYPE smb_operator_reconcile_total counter
smb_operator_reconcile_total {metrics.reconcile_count}

# HELP smb_operator_reconcile_errors_total Total number of reconciliation errors
# TYPE smb_operator_reconcile_errors_total counter
smb_operator_reconcile_errors_total {metrics.reconcile_errors}

# HELP smb_operator_shares_managed Current number of shares being managed
# TYPE smb_operator_shares_managed gauge
smb_operator_shares_managed {metrics.shares_managed}

# HELP smb_operator_last_reconcile_timestamp_seconds Timestamp of last reconciliation
# TYPE smb_operator_last_reconcile_timestamp_seconds gauge
smb_operator_last_reconcile_timestamp_seconds {metrics.last_reconcile_time}

# HELP smb_operator_retry_count_total Total number of API call retries
# TYPE smb_operator_retry_count_total counter
smb_operator_retry_count_total {metrics.retry_count}

# HELP smb_operator_events_sent_total Total number of Kubernetes events sent
# TYPE smb_operator_events_sent_total counter
smb_operator_events_sent_total {metrics.events_sent}
"""
            self.wfile.write(metrics_text.encode())
        
        else:
            self.send_response(404)
            self.end_headers()


def start_health_server(port: int):
    """Start HTTP server for health checks in background thread"""
    server = HTTPServer(('0.0.0.0', port), HealthHandler)
    logger.info(f"Health server listening on port {port}")
    
    def serve():
        while not shutdown_event.is_set():
            server.handle_request()
    
    thread = threading.Thread(target=serve, daemon=True)
    thread.start()


def signal_handler(signum, frame):
    """Handle shutdown signals gracefully"""
    global healthy, ready
    logger.info(f"Received signal {signum}, shutting down gracefully...")
    healthy = False
    ready = False
    shutdown_event.set()


def retry_on_error(func):
    """Decorator for exponential backoff retry - extracts config from self"""
    @wraps(func)
    def wrapper(self, *args, **kwargs):
        # Extract config from self (operator instance)
        cfg = self.config if hasattr(self, 'config') else Config()
        
        delay = cfg.initial_delay
        for attempt in range(cfg.max_retries):
            try:
                return func(self, *args, **kwargs)
            except ApiException as e:
                if e.status in cfg.retryable_status_codes:
                    if attempt < cfg.max_retries - 1:
                        logger.warning(
                            f"API error {e.status} in {func.__name__}, "
                            f"retrying in {delay}s... (attempt {attempt+1}/{cfg.max_retries})"
                        )
                        metrics.retry_count += 1
                        time.sleep(delay)
                        delay *= cfg.backoff_multiplier
                    else:
                        logger.error(f"Max retries exceeded for {func.__name__}")
                        raise
                else:
                    raise
            except Exception as e:
                logger.error(f"Non-retryable error in {func.__name__}: {e}")
                raise
        return None
    return wrapper


@dataclass
class SMBShare:
    """Represents a single SMB share configuration.

    mount_type:
      'nfs'    — RWX volume; the startup script mounts via NFS from the Longhorn share endpoint.
      'direct' — RWO volume; a temp PV+PVC is created so the CSI driver mounts it directly
                 into the SMB server pod. No NFS mount in the startup script.
    """
    name: str
    path: str
    namespace: str
    pvc_name: str
    access_mode: str
    mount_type: str = 'nfs'    # 'nfs' | 'direct'
    nfs_server: str = ''
    nfs_path: str = ''
    longhorn_volume_name: str = ''  # set for direct mounts; used to build PV/PVC names

    @property
    def readonly(self) -> bool:
        return self.access_mode == 'read-only'

    @property
    def unique_id(self) -> str:
        return f"{self.namespace}/{self.pvc_name}"

    @property
    def migration_pv_name(self) -> str:
        """Stable name for the temp PV created for a direct migration share."""
        return f"migrate-{self.longhorn_volume_name}"

    @property
    def migration_pvc_name(self) -> str:
        """Stable name for the temp PVC created for a direct migration share."""
        return f"migrate-{self.longhorn_volume_name}"

    @property
    def pod_volume_name(self) -> str:
        """Safe k8s volume name for use inside the pod spec (max 63 chars, no slashes)."""
        return self.migration_pvc_name[:63]


class SMBOperator:
    """Main operator class with ConfigMap configuration and retry logic"""
    
    def __init__(self, cfg: Config):
        try:
            config.load_incluster_config()
            logger.info("Loaded in-cluster Kubernetes config")
        except config.ConfigException:
            config.load_kube_config()
            logger.info("Loaded local Kubernetes config")
        
        self.core_v1 = client.CoreV1Api()
        self.apps_v1 = client.AppsV1Api()
        self.custom_api = client.CustomObjectsApi()
        self.config = cfg
        self.current_shares: Set[str] = set()
        self.operator_uid: Optional[str] = None
        self.operator_labels: Dict[str, str] = {}  # cached from own deployment

        logger.info("SMB Operator initialized")
    
    def record_event(self, obj_kind: str, obj_name: str, obj_namespace: str, 
                    reason: str, message: str, event_type: str = 'Normal'):
        """Record a Kubernetes event"""
        try:
            timestamp = datetime.utcnow().isoformat() + 'Z'
            event_name = f"{obj_name}.{int(time.time())}"
            
            event = client.CoreV1Event(
                metadata=client.V1ObjectMeta(
                    name=event_name,
                    namespace=obj_namespace
                ),
                involved_object=client.V1ObjectReference(
                    kind=obj_kind,
                    name=obj_name,
                    namespace=obj_namespace
                ),
                reason=reason,
                message=message,
                type=event_type,
                first_timestamp=timestamp,
                last_timestamp=timestamp,
                count=1,
                source=client.V1EventSource(component='smb-operator')
            )
            
            self.core_v1.create_namespaced_event(obj_namespace, event)
            metrics.events_sent += 1
            logger.debug(f"Event recorded: {reason} for {obj_kind}/{obj_name}")
        except Exception as e:
            logger.debug(f"Failed to record event: {e}")
    
    def update_pvc_annotations(self, namespace: str, pvc_name: str, status: str, share_path: str = ""):
        """Update PVC with SMB status annotations"""
        try:
            pvc = self.core_v1.read_namespaced_persistent_volume_claim(pvc_name, namespace)
            annotations = pvc.metadata.annotations or {}
            
            annotations['smb-operator/status'] = status
            annotations['smb-operator/share-path'] = share_path
            annotations['smb-operator/last-update'] = datetime.utcnow().strftime('%Y-%m-%d %H:%M:%S UTC')
            
            pvc.metadata.annotations = annotations
            self.core_v1.patch_namespaced_persistent_volume_claim(pvc_name, namespace, pvc)
            logger.debug(f"Updated annotations for PVC {namespace}/{pvc_name}")
        except Exception as e:
            logger.warning(f"Failed to update PVC annotations: {e}")
    
    def get_operator_uid(self) -> Optional[str]:
        """Get operator deployment UID and cache its labels for propagation to child resources."""
        if self.operator_uid:
            return self.operator_uid

        try:
            deployment = self.apps_v1.read_namespaced_deployment(
                'smb-operator',
                self.config.namespace
            )
            self.operator_uid = deployment.metadata.uid
            # Cache all labels from the operator deployment — includes ArgoCD tracking labels
            self.operator_labels = dict(deployment.metadata.labels or {})
            logger.debug(f"Cached operator labels: {self.operator_labels}")
            return self.operator_uid
        except Exception as e:
            logger.warning(f"Failed to get operator UID: {e}")
            return None

    def get_child_labels(self, extra: Optional[Dict[str, str]] = None) -> Dict[str, str]:
        """Return labels to apply to all operator-managed child resources.

        Inherits the operator's own labels (which include ArgoCD tracking labels
        like argocd.argoproj.io/app-name) so resources appear in the ArgoCD UI
        under the same application — without hardcoding any app name.
        """
        self.get_operator_uid()  # ensure labels are cached
        labels = dict(self.operator_labels)
        labels['managed-by'] = 'smb-operator'
        if extra:
            labels.update(extra)
        return labels
    
    def get_labeled_pvcs(self) -> List:
        """Get all PVCs with smb-access label with retry"""
        pvcs = self.core_v1.list_persistent_volume_claim_for_all_namespaces(
            label_selector='smb-access'
        )
        return pvcs.items
    
    def get_longhorn_volume(self, volume_name: str) -> Optional[Dict]:
        """Get Longhorn volume CRD to extract NFS endpoint"""
        for ns in [self.config.longhorn_namespace, 'longhorn-system']:
            try:
                volume = self.custom_api.get_namespaced_custom_object(
                    group=self.config.longhorn_group,
                    version=self.config.longhorn_version,
                    namespace=ns,
                    plural=self.config.longhorn_plural,
                    name=volume_name
                )
                return volume
            except ApiException:
                continue
        return None
    
    def parse_nfs_endpoint(self, share_endpoint: str) -> Optional[tuple]:
        """Parse NFS endpoint from format: nfs://server/path"""
        if not share_endpoint or not share_endpoint.startswith('nfs://'):
            return None
        
        try:
            endpoint = share_endpoint[6:]
            parts = endpoint.split('/', 1)
            if len(parts) != 2:
                return None
            server = parts[0]
            path = '/' + parts[1]
            return (server, path)
        except Exception as e:
            logger.error(f"Failed to parse NFS endpoint {share_endpoint}: {e}")
            return None
    
    def discover_migration_shares(self) -> List[SMBShare]:
        """Discover migration shares from smb-operator-migration ConfigMap.

        ConfigMap format (data key: volumes.yaml):
            volumes:
              - name: my-longhorn-volume   # Longhorn volume name (required)
                shareName: my-share        # SMB share name (optional)
                smb: rw                    # 'rw' (default) or 'ro'

        RWX volumes  → NFS share endpoint is used (same as PVC-based shares).
        RWO volumes  → a temporary PV+PVC (migrate-<name>) is created in the
                       storage namespace so the CSI driver mounts it directly
                       into the SMB server pod. No NFS mount needed.
        """
        migration_cm_name = 'smb-operator-migration'
        shares = []

        try:
            cm = self.core_v1.read_namespaced_config_map(migration_cm_name, self.config.namespace)
        except ApiException as e:
            if e.status == 404:
                return []
            logger.warning(f"Failed to read migration ConfigMap: {e}")
            return []
        except Exception as e:
            logger.warning(f"Failed to read migration ConfigMap: {e}")
            return []

        raw = cm.data.get('volumes.yaml', '') if cm.data else ''
        if not raw.strip():
            return []

        try:
            data = yaml.safe_load(raw) or {}
        except Exception as e:
            logger.error(f"Failed to parse migration ConfigMap volumes.yaml: {e}")
            return []

        volume_entries = data.get('volumes', [])
        if not volume_entries:
            return []

        logger.info(f"Migration ConfigMap: found {len(volume_entries)} volume(s)")

        for entry in volume_entries:
            volume_name = entry.get('name', '').strip()
            if not volume_name:
                logger.warning(f"Migration entry missing 'name', skipping: {entry}")
                continue

            share_name = entry.get('shareName', volume_name).strip()
            smb_mode = entry.get('smb', 'rw').strip().lower()
            if smb_mode not in ('rw', 'ro'):
                logger.warning(f"Invalid smb value '{smb_mode}' for migration volume {volume_name}, defaulting to 'rw'")
                smb_mode = 'rw'
            access_mode = 'read-only' if smb_mode == 'ro' else 'shared'

            logger.info(f"Migration: resolving Longhorn volume '{volume_name}' -> SMB share '{share_name}' ({smb_mode})")

            volume = self.get_longhorn_volume(volume_name)
            if not volume:
                logger.warning(f"Migration: Longhorn volume '{volume_name}' not found, skipping")
                continue

            lh_access_mode = volume.get('spec', {}).get('accessMode', '').lower()  # 'rwo' or 'rwx'
            share_endpoint = volume.get('status', {}).get('shareEndpoint', '')

            if share_endpoint:
                # RWX path — mount via NFS share endpoint (same as PVC-based shares)
                parsed = self.parse_nfs_endpoint(share_endpoint)
                if not parsed:
                    logger.error(f"Migration: failed to parse NFS endpoint '{share_endpoint}' for '{volume_name}'")
                    continue
                nfs_server, nfs_path = parsed
                logger.info(f"  RWX — NFS endpoint: {nfs_server}:{nfs_path}")
                share = SMBShare(
                    name=share_name,
                    path=f"/shares/{share_name}",
                    namespace=self.config.namespace,
                    pvc_name=f"migration-{volume_name}",
                    access_mode=access_mode,
                    mount_type='nfs',
                    nfs_server=nfs_server,
                    nfs_path=nfs_path,
                    longhorn_volume_name=volume_name,
                )
            else:
                # RWO path — mount via temp PV+PVC so CSI attaches it directly
                lh_size = volume.get('spec', {}).get('size', '1073741824')
                logger.info(f"  RWO ({lh_access_mode}) — will use direct CSI mount via temp PV+PVC "
                            f"(migrate-{volume_name}), size={lh_size}")
                share = SMBShare(
                    name=share_name,
                    path=f"/shares/{share_name}",
                    namespace=self.config.namespace,
                    pvc_name=f"migration-{volume_name}",
                    access_mode=access_mode,
                    mount_type='direct',
                    longhorn_volume_name=volume_name,
                )

            shares.append(share)

        logger.info(f"Migration: resolved {len(shares)} share(s)")
        return shares

    def ensure_migration_pvcs(self, migration_shares: List[SMBShare]):
        """Create temp PV+PVC for RWO direct-mount migration shares; delete orphaned ones.

        Resources are named  migrate-<longhorn-volume-name>  and labelled
        smb-operator/migration=true  so they can be identified and cleaned up.
        """
        LABEL = 'smb-operator/migration'
        ns = self.config.namespace

        # Labels applied to all migration PV/PVCs — includes ArgoCD tracking labels
        migration_labels = self.get_child_labels({LABEL: 'true'})
        desired: Dict[str, SMBShare] = {
            s.migration_pvc_name: s
            for s in migration_shares
            if s.mount_type == 'direct'
        }

        # ── Garbage-collect orphaned PVCs ─────────────────────────────────────
        try:
            existing_pvcs = self.core_v1.list_namespaced_persistent_volume_claim(
                ns, label_selector=f'{LABEL}=true'
            )
            for pvc in existing_pvcs.items:
                pvc_name = pvc.metadata.name
                if pvc_name not in desired:
                    logger.info(f"Migration cleanup: deleting orphaned PVC {ns}/{pvc_name}")
                    try:
                        self.core_v1.delete_namespaced_persistent_volume_claim(pvc_name, ns)
                    except ApiException as e:
                        if e.status != 404:
                            logger.warning(f"Failed to delete orphaned PVC {pvc_name}: {e}")
        except Exception as e:
            logger.warning(f"Failed to list migration PVCs for cleanup: {e}")

        # ── Garbage-collect orphaned PVs ──────────────────────────────────────
        try:
            existing_pvs = self.core_v1.list_persistent_volume(
                label_selector=f'{LABEL}=true'
            )
            for pv in existing_pvs.items:
                pv_name = pv.metadata.name
                if pv_name not in desired:
                    logger.info(f"Migration cleanup: deleting orphaned PV {pv_name}")
                    try:
                        self.core_v1.delete_persistent_volume(pv_name)
                    except ApiException as e:
                        if e.status != 404:
                            logger.warning(f"Failed to delete orphaned PV {pv_name}: {e}")
        except Exception as e:
            logger.warning(f"Failed to list migration PVs for cleanup: {e}")

        # ── Ensure PV+PVC exist for each desired direct share ─────────────────
        for pvc_name, share in desired.items():
            lh_vol = self.get_longhorn_volume(share.longhorn_volume_name)
            if not lh_vol:
                logger.warning(f"Migration: cannot create PV for '{share.longhorn_volume_name}' — volume not found")
                continue

            size_bytes = lh_vol.get('spec', {}).get('size', '1073741824')
            # Convert bytes string → k8s quantity (Mi)
            try:
                size_mi = f"{int(size_bytes) // (1024 * 1024)}Mi"
            except (ValueError, TypeError):
                size_mi = '1Gi'

            pv_name = share.migration_pv_name

            # Create PV if missing
            try:
                self.core_v1.read_persistent_volume(pv_name)
                logger.debug(f"Migration PV {pv_name} already exists")
            except ApiException as e:
                if e.status == 404:
                    logger.info(f"Migration: creating PV {pv_name} ({size_mi}) for volume '{share.longhorn_volume_name}'")
                    pv = client.V1PersistentVolume(
                        metadata=client.V1ObjectMeta(name=pv_name, labels=migration_labels),
                        spec=client.V1PersistentVolumeSpec(
                            capacity={'storage': size_mi},
                            volume_mode='Filesystem',
                            access_modes=['ReadWriteOnce'],
                            persistent_volume_reclaim_policy='Retain',
                            storage_class_name='longhorn',
                            csi=client.V1CSIPersistentVolumeSource(
                                driver='driver.longhorn.io',
                                fs_type='ext4',
                                volume_handle=share.longhorn_volume_name,
                            ),
                        ),
                    )
                    try:
                        self.core_v1.create_persistent_volume(pv)
                    except ApiException as ce:
                        logger.error(f"Migration: failed to create PV {pv_name}: {ce}")
                        continue
                else:
                    logger.warning(f"Migration: failed to read PV {pv_name}: {e}")
                    continue

            # Create PVC if missing
            try:
                self.core_v1.read_namespaced_persistent_volume_claim(pvc_name, ns)
                logger.debug(f"Migration PVC {ns}/{pvc_name} already exists")
            except ApiException as e:
                if e.status == 404:
                    logger.info(f"Migration: creating PVC {ns}/{pvc_name}")
                    storage_class = 'longhorn'
                    pvc = client.V1PersistentVolumeClaim(
                        metadata=client.V1ObjectMeta(
                            name=pvc_name,
                            namespace=ns,
                            labels=migration_labels,
                        ),
                        spec=client.V1PersistentVolumeClaimSpec(
                            storage_class_name=storage_class,
                            volume_name=pv_name,
                            access_modes=['ReadWriteOnce'],
                            resources=client.V1ResourceRequirements(
                                requests={'storage': size_mi}
                            ),
                        ),
                    )
                    try:
                        self.core_v1.create_namespaced_persistent_volume_claim(ns, pvc)
                    except ApiException as ce:
                        logger.error(f"Migration: failed to create PVC {pvc_name}: {ce}")
                else:
                    logger.warning(f"Migration: failed to read PVC {pvc_name}: {e}")

    def discover_shares(self) -> List[SMBShare]:
        """Discover all SMB shares from labeled PVCs"""
        shares = []
        pvcs = self.get_labeled_pvcs()

        for pvc in pvcs:
            try:
                namespace = pvc.metadata.namespace
                pvc_name = pvc.metadata.name
                volume_name = pvc.spec.volume_name
                
                if not volume_name:
                    logger.warning(f"PVC {namespace}/{pvc_name} is not bound, skipping")
                    self.update_pvc_annotations(namespace, pvc_name, 'pending', '')
                    self.record_event('PersistentVolumeClaim', pvc_name, namespace,
                                    'NotBound', 'PVC is not bound to a volume', 'Warning')
                    continue
                
                labels = pvc.metadata.labels or {}
                access_mode = labels.get('smb-access', 'shared')
                share_name = labels.get('smb-share-name', pvc_name)
                
                if access_mode not in ['shared', 'read-only']:
                    logger.warning(f"Invalid access mode '{access_mode}' for {namespace}/{pvc_name}, defaulting to 'shared'")
                    access_mode = 'shared'
                
                logger.info(f"Processing PVC: {namespace}/{pvc_name} -> {share_name} ({access_mode})")
                
                volume = self.get_longhorn_volume(volume_name)
                if not volume:
                    logger.warning(f"Longhorn volume {volume_name} not found for {namespace}/{pvc_name}")
                    self.update_pvc_annotations(namespace, pvc_name, 'error', '')
                    self.record_event('PersistentVolumeClaim', pvc_name, namespace,
                                    'VolumeNotFound', f'Longhorn volume {volume_name} not found', 'Warning')
                    continue
                
                share_endpoint = volume.get('status', {}).get('shareEndpoint', '')
                if not share_endpoint:
                    logger.warning(f"No share endpoint for volume {volume_name} (not RWX?)")
                    self.update_pvc_annotations(namespace, pvc_name, 'error', '')
                    self.record_event('PersistentVolumeClaim', pvc_name, namespace,
                                    'NoShareEndpoint', 'Volume does not have NFS share endpoint (must be RWX)', 'Warning')
                    continue
                
                parsed = self.parse_nfs_endpoint(share_endpoint)
                if not parsed:
                    logger.error(f"Failed to parse NFS endpoint: {share_endpoint}")
                    self.update_pvc_annotations(namespace, pvc_name, 'error', '')
                    continue
                
                nfs_server, nfs_path = parsed
                logger.info(f"  NFS endpoint: {nfs_server}:{nfs_path}")
                
                share = SMBShare(
                    name=share_name,
                    path=f"/shares/{share_name}",
                    namespace=namespace,
                    pvc_name=pvc_name,
                    access_mode=access_mode,
                    nfs_server=nfs_server,
                    nfs_path=nfs_path
                )
                shares.append(share)
                
                # Update PVC with success status
                share_path = f"//{self.config.smb_loadbalancer_ip}/{share_name}"
                self.update_pvc_annotations(namespace, pvc_name, 'active', share_path)
                
            except Exception as e:
                logger.error(f"Error processing PVC {namespace}/{pvc_name}: {e}", exc_info=True)
                self.update_pvc_annotations(namespace, pvc_name, 'error', '')
                self.record_event('PersistentVolumeClaim', pvc_name, namespace,
                                'ProcessingError', f'Failed to process PVC: {str(e)}', 'Warning')
                continue
        
        logger.info(f"Discovered {len(shares)} shares from {len(pvcs)} PVCs")
        return shares
    
    def generate_smb_config(self, shares: List[SMBShare]) -> str:
        """Generate Samba configuration with user authentication and performance tuning"""
        lines = [
            "[global]",
            f"workgroup = {self.config.smb_workgroup}",
            f"server string = {self.config.smb_server_string}",
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
        ]

        veto_files = "/.DS_Store/._*/.Spotlight-V100/.TemporaryItems/.Trashes/.fseventsd/Thumbs.db/desktop.ini/$RECYCLE.BIN/System Volume Information/lost+found/"
        
        for share in shares:
            lines.extend([
                f"[{share.name}]",
                f"path = {share.path}",
                "available = yes",
                "browseable = yes",
                "public = no",
                "guest ok = no",
                "writable = " + ("no" if share.readonly else "yes"),
                "read only = " + ("yes" if share.readonly else "no"),
                f"valid users = {self.config.smb_username}",
                f"write list = " + ("" if share.readonly else self.config.smb_username),
                "force user = root",
                "force group = root",
                f"veto files = {veto_files}",
                "delete veto files = yes",
                "create mask = 0755",
                "directory mask = 0755",
                ""
            ])
        
        return "\n".join(lines)
    
    def generate_startup_script(self, shares: List[SMBShare]) -> str:
        """Generate container startup script with NFS mounts and SMB user setup"""
        lines = [
            "#!/bin/bash",
            "set -e",
            "echo 'Starting SMB server initialization...'",
            "echo ''",
            "",
            "# Create SMB user for authentication",
            f"useradd -M -s /usr/sbin/nologin {self.config.smb_username} 2>/dev/null || true",
            f"echo -e \"$SMB_PASSWORD\\n$SMB_PASSWORD\" | smbpasswd -a -s {self.config.smb_username}",
            f"echo 'SMB user {self.config.smb_username} configured'",
            "echo ''",
            ""
        ]
        
        for share in shares:
            if share.mount_type == 'direct':
                # CSI mounts the volume directly into the pod at share.path —
                # just ensure the directory exists (it will already be mounted).
                lines.extend([
                    f"echo 'Direct CSI mount ready: {share.name}'",
                    f"mkdir -p {share.path}",
                    "echo ''",
                    ""
                ])
            else:
                lines.extend([
                    f"echo 'Mounting share: {share.name}'",
                    f"mkdir -p {share.path}",
                    f"mount -t nfs -o vers=4.2,noresvport {share.nfs_server}:{share.nfs_path} {share.path} || {{",
                    f"  echo 'Failed to mount {share.name}'",
                    "  exit 1",
                    "}",
                    f"echo '  Mounted: {share.nfs_server}:{share.nfs_path} -> {share.path}'",
                    "echo ''",
                    ""
                ])
        
        lines.extend([
            "echo 'Starting Samba daemon...'",
            "exec /usr/sbin/smbd --foreground --no-process-group"
        ])
        
        return "\n".join(lines)
    
    @retry_on_error
    def generate_deployment(self, shares: List[SMBShare]) -> client.V1Deployment:
        """Generate SMB server deployment with retry"""
        smb_config = self.generate_smb_config(shares)
        startup_script = self.generate_startup_script(shares)
        
        container = client.V1Container(
            name='samba',
            image=self.config.smb_image,
            security_context=client.V1SecurityContext(privileged=True),
            command=['/bin/bash', '-c', startup_script],
            env=[
                client.V1EnvVar(
                    name='SMB_PASSWORD',
                    value_from=client.V1EnvVarSource(
                        secret_key_ref=client.V1SecretKeySelector(
                            name='smb-credentials',
                            key='password'
                        )
                    )
                )
            ],
            ports=[
                client.V1ContainerPort(container_port=445, protocol='TCP'),
                client.V1ContainerPort(container_port=139, protocol='TCP')
            ],
            volume_mounts=[
                client.V1VolumeMount(name='smb-config', mount_path='/etc/samba/smb.conf', sub_path='smb.conf')
            ] + [
                # Direct CSI mounts for RWO migration volumes
                client.V1VolumeMount(
                    name=share.pod_volume_name,
                    mount_path=share.path,
                )
                for share in shares
                if share.mount_type == 'direct'
            ],
            resources=client.V1ResourceRequirements(
                requests={
                    'cpu': self.config.smb_resources['requests']['cpu'],
                    'memory': self.config.smb_resources['requests']['memory']
                },
                limits={
                    'cpu': self.config.smb_resources['limits']['cpu'],
                    'memory': self.config.smb_resources['limits']['memory']
                }
            ),
            # Readiness probe to ensure SMB is ready before routing traffic
            readiness_probe=client.V1Probe(
                tcp_socket=client.V1TCPSocketAction(port=445),
                initial_delay_seconds=5,
                period_seconds=5,
                timeout_seconds=3,
                success_threshold=1,
                failure_threshold=3
            ),
            # Liveness probe to detect if SMB daemon crashes
            liveness_probe=client.V1Probe(
                tcp_socket=client.V1TCPSocketAction(port=445),
                initial_delay_seconds=15,
                period_seconds=10,
                timeout_seconds=3,
                failure_threshold=3
            ),
            # Graceful shutdown - give time for connections to drain
            lifecycle=client.V1Lifecycle(
                pre_stop=client.V1LifecycleHandler(
                    _exec=client.V1ExecAction(
                        command=['/bin/bash', '-c', 'echo "Shutting down gracefully..." && sleep 5']
                    )
                )
            )
        )
        
        template = client.V1PodTemplateSpec(
            metadata=client.V1ObjectMeta(
                labels={'app': self.config.smb_deployment_name},
                annotations={
                    'smb-operator/version': '2.0',
                    'smb-operator/shares-count': str(len(shares)),
                    'smb-operator/share-list': ','.join([s.name for s in shares]),
                    'smb-operator/last-updated': datetime.utcnow().strftime('%Y-%m-%d %H:%M:%S UTC'),
                }
            ),
            spec=client.V1PodSpec(
                containers=[container],
                volumes=[
                    client.V1Volume(
                        name='smb-config',
                        config_map=client.V1ConfigMapVolumeSource(
                            name='smb-config',
                            items=[client.V1KeyToPath(key='smb.conf', path='smb.conf')]
                        )
                    )
                ] + [
                    # PVC-backed volumes for RWO migration direct mounts
                    client.V1Volume(
                        name=share.pod_volume_name,
                        persistent_volume_claim=client.V1PersistentVolumeClaimVolumeSource(
                            claim_name=share.migration_pvc_name,
                            read_only=share.readonly,
                        )
                    )
                    for share in shares
                    if share.mount_type == 'direct'
                ],
                termination_grace_period_seconds=30  # Allow 30s for graceful shutdown
            )
        )
        
        spec = client.V1DeploymentSpec(
            replicas=1,
            selector=client.V1LabelSelector(match_labels={'app': self.config.smb_deployment_name}),
            strategy=client.V1DeploymentStrategy(type='Recreate'),
            template=template
        )
        
        deployment = client.V1Deployment(
            api_version='apps/v1',
            kind='Deployment',
            metadata=client.V1ObjectMeta(
                name=self.config.smb_deployment_name,
                namespace=self.config.namespace,
                labels=self.get_child_labels({'app': self.config.smb_deployment_name}),
                annotations={
                    'smb-operator/version': '2.0',
                    'smb-operator/shares-count': str(len(shares)),
                    'smb-operator/share-list': ','.join([s.name for s in shares]),
                }
            ),
            spec=spec
        )
        
        return deployment
    
    @retry_on_error
    def ensure_service(self):
        """Ensure SMB LoadBalancer service exists with owner reference and retry"""
        try:
            self.core_v1.read_namespaced_service(self.config.smb_service_name, self.config.namespace)
            logger.debug("SMB service already exists")
        except ApiException as e:
            if e.status == 404:
                logger.info("Creating SMB service with owner reference...")
                
                operator_uid = self.get_operator_uid()
                owner_refs = []
                
                if operator_uid:
                    owner_refs = [client.V1OwnerReference(
                        api_version='apps/v1',
                        kind='Deployment',
                        name='smb-operator',
                        uid=operator_uid,
                        controller=True,
                        block_owner_deletion=True
                    )]
                
                service = client.V1Service(
                    metadata=client.V1ObjectMeta(
                        name=self.config.smb_service_name,
                        namespace=self.config.namespace,
                        labels=self.get_child_labels({'app': self.config.smb_deployment_name}),
                        annotations={
                            'service.kubernetes.io/topology-mode': 'Auto'
                        },
                        owner_references=owner_refs if owner_refs else None
                    ),
                    spec=client.V1ServiceSpec(
                        type='LoadBalancer',
                        load_balancer_ip=self.config.smb_loadbalancer_ip,
                        selector={'app': self.config.smb_deployment_name},
                        ports=[
                            client.V1ServicePort(name='smb', port=445, target_port=445, protocol='TCP'),
                            client.V1ServicePort(name='netbios', port=139, target_port=139, protocol='TCP')
                        ]
                    )
                )
                self.core_v1.create_namespaced_service(self.config.namespace, service)
                logger.info(f"SMB service created at {self.config.smb_loadbalancer_ip}")
                self.record_event('Service', self.config.smb_service_name, self.config.namespace,
                                'Created', f'SMB service created with LoadBalancer IP {self.config.smb_loadbalancer_ip}')
            else:
                logger.error(f"Failed to check service: {e}")
    
    @retry_on_error
    def update_smb_config(self, smb_config: str):
        """Update or create SMB ConfigMap with retry"""
        cm_name = 'smb-config'
        try:
            cm = self.core_v1.read_namespaced_config_map(cm_name, self.config.namespace)
            cm.data = {'smb.conf': smb_config}
            self.core_v1.patch_namespaced_config_map(cm_name, self.config.namespace, cm)
            logger.info("Updated SMB configuration ConfigMap")
        except ApiException as e:
            if e.status == 404:
                cm = client.V1ConfigMap(
                    metadata=client.V1ObjectMeta(
                        name=cm_name,
                        namespace=self.config.namespace,
                        labels=self.get_child_labels(),
                    ),
                    data={'smb.conf': smb_config}
                )
                self.core_v1.create_namespaced_config_map(self.config.namespace, cm)
                logger.info("Created SMB configuration ConfigMap")
            else:
                raise
    
    @retry_on_error
    def apply_deployment(self, deployment: client.V1Deployment) -> bool:
        """Apply or update the deployment with retry"""
        try:
            existing = self.apps_v1.read_namespaced_deployment(self.config.smb_deployment_name, self.config.namespace)
            logger.info("Updating existing SMB deployment...")
            
            # Check if we need to change strategy type - requires replace, not patch
            existing_strategy = existing.spec.strategy.type if existing.spec.strategy else None
            new_strategy = deployment.spec.strategy.type if deployment.spec.strategy else None
            # Strategic merge patch will NOT remove list entries (e.g. volumes/volumeMounts)
            # if those entries are no longer in the new spec. Prefer replace in those cases.
            existing_volumes = {v.name for v in (existing.spec.template.spec.volumes or [])}
            new_volumes = {v.name for v in (deployment.spec.template.spec.volumes or [])}
            existing_mounts = set()
            new_mounts = set()
            if existing.spec.template.spec.containers:
                existing_mounts = {m.mount_path for m in (existing.spec.template.spec.containers[0].volume_mounts or [])}
            if deployment.spec.template.spec.containers:
                new_mounts = {m.mount_path for m in (deployment.spec.template.spec.containers[0].volume_mounts or [])}

            replace_needed = False
            if existing_strategy and new_strategy and existing_strategy != new_strategy:
                replace_needed = True
            if existing_volumes != new_volumes:
                replace_needed = True
            if existing_mounts != new_mounts:
                replace_needed = True

            if replace_needed:
                logger.info("Detected significant deployment change (strategy/volumes/volumeMounts); using replace operation")
                self.apps_v1.replace_namespaced_deployment(self.config.smb_deployment_name, self.config.namespace, deployment)
            else:
                self.apps_v1.patch_namespaced_deployment(self.config.smb_deployment_name, self.config.namespace, deployment)
            
            logger.info("SMB deployment updated successfully")
            self.record_event('Deployment', self.config.smb_deployment_name, self.config.namespace,
                            'Updated', 'SMB deployment updated with new configuration')
            return True
        except ApiException as e:
            if e.status == 404:
                logger.info("Creating new SMB deployment...")
                self.apps_v1.create_namespaced_deployment(self.config.namespace, deployment)
                logger.info("SMB deployment created successfully")
                self.record_event('Deployment', self.config.smb_deployment_name, self.config.namespace,
                                'Created', 'SMB deployment created')
                return True
            else:
                logger.error(f"Failed to apply deployment: {e}")
                return False
    
    def reconcile(self) -> bool:
        """Main reconciliation logic"""
        try:
            logger.info("Starting reconciliation...")

            pvc_shares = self.discover_shares()
            migration_shares = self.discover_migration_shares()

            # Merge — migration shares are identified by their synthetic pvc_name prefix
            # so they won't collide with real PVC-based shares in current_shares tracking
            shares = pvc_shares + migration_shares

            if migration_shares:
                logger.info(f"Including {len(migration_shares)} migration share(s) in SMB server: "
                            f"{[s.name for s in migration_shares]}")

            new_shares = {s.unique_id for s in shares}
            
            if new_shares == self.current_shares:
                logger.info(f"No changes detected ({len(shares)} shares)")
                return True
            
            logger.info("Share configuration changed:")
            logger.info(f"  Previous: {len(self.current_shares)} shares")
            logger.info(f"  Current:  {len(new_shares)} shares")
            
            added = new_shares - self.current_shares
            removed = self.current_shares - new_shares
            
            if added:
                logger.info(f"  Added: {', '.join(sorted(added))}")
            if removed:
                logger.info(f"  Removed: {', '.join(sorted(removed))}")
            
            # Generate and apply configuration
            smb_config = self.generate_smb_config(shares)
            self.update_smb_config(smb_config)

            # Ensure temp PV+PVCs exist for RWO direct-mount migration shares,
            # and clean up any that were removed from the ConfigMap
            self.ensure_migration_pvcs(migration_shares)

            deployment = self.generate_deployment(shares)
            success = self.apply_deployment(deployment)
            
            if success:
                self.ensure_service()
                self.current_shares = new_shares
                logger.info("Reconciliation completed successfully")
                
                # Record events for added/removed shares
                for share_id in added:
                    ns, pvc = share_id.split('/', 1)
                    self.record_event('PersistentVolumeClaim', pvc, ns,
                                    'ShareAdded', f'SMB share added: {pvc}')
                for share_id in removed:
                    ns, pvc = share_id.split('/', 1)
                    self.record_event('PersistentVolumeClaim', pvc, ns,
                                    'ShareRemoved', 'SMB share removed')
            
            return success
            
        except Exception as e:
            logger.error(f"Reconciliation error: {e}", exc_info=True)
            metrics.reconcile_errors += 1
            return False
    
    def watch_pvcs(self):
        """Watch for PVC changes and migration ConfigMap changes using Kubernetes Watch API"""
        logger.info("Starting PVC + migration ConfigMap watch...")

        def watch_pvcs_stream():
            w = watch.Watch()
            while not shutdown_event.is_set():
                try:
                    for event in w.stream(
                        self.core_v1.list_persistent_volume_claim_for_all_namespaces,
                        label_selector='smb-access',
                        timeout_seconds=self.config.reconcile_interval
                    ):
                        if shutdown_event.is_set():
                            return
                        event_type = event['type']
                        pvc = event['object']
                        logger.info(f"PVC event: {event_type} {pvc.metadata.namespace}/{pvc.metadata.name}")
                        if self.reconcile():
                            metrics.reconcile_count += 1
                            metrics.last_reconcile_time = time.time()
                            metrics.shares_managed = len(self.current_shares)
                except ApiException as e:
                    if e.status == 410:
                        logger.warning("PVC watch expired, restarting...")
                        continue
                    else:
                        logger.error(f"PVC watch error: {e}", exc_info=True)
                        time.sleep(10)
                except Exception as e:
                    logger.error(f"Unexpected PVC watch error: {e}", exc_info=True)
                    time.sleep(10)

        def watch_migration_configmap_stream():
            w = watch.Watch()
            while not shutdown_event.is_set():
                try:
                    for event in w.stream(
                        self.core_v1.list_namespaced_config_map,
                        namespace=self.config.namespace,
                        field_selector='metadata.name=smb-operator-migration',
                        timeout_seconds=self.config.reconcile_interval
                    ):
                        if shutdown_event.is_set():
                            return
                        event_type = event['type']
                        logger.info(f"Migration ConfigMap event: {event_type} smb-operator-migration")
                        if self.reconcile():
                            metrics.reconcile_count += 1
                            metrics.last_reconcile_time = time.time()
                            metrics.shares_managed = len(self.current_shares)
                except ApiException as e:
                    if e.status == 410:
                        logger.warning("Migration ConfigMap watch expired, restarting...")
                        continue
                    else:
                        logger.error(f"Migration ConfigMap watch error: {e}", exc_info=True)
                        time.sleep(10)
                except Exception as e:
                    logger.error(f"Unexpected migration ConfigMap watch error: {e}", exc_info=True)
                    time.sleep(10)

        # Run both watchers in parallel threads
        pvc_thread = threading.Thread(target=watch_pvcs_stream, daemon=True, name='watch-pvcs')
        migration_thread = threading.Thread(target=watch_migration_configmap_stream, daemon=True, name='watch-migration-cm')

        pvc_thread.start()
        migration_thread.start()

        # Block until shutdown
        while not shutdown_event.is_set():
            shutdown_event.wait(timeout=5)

        pvc_thread.join(timeout=10)
        migration_thread.join(timeout=10)

    def run(self):
        """Main operator loop with graceful shutdown"""
        global healthy, ready
        
        logger.info("SMB Operator starting...")
        logger.info("Configuration loaded:")
        logger.info(f"  Namespace: {self.config.namespace}")
        logger.info(f"  LoadBalancer IP: {self.config.smb_loadbalancer_ip}")
        logger.info(f"  Reconcile Interval: {self.config.reconcile_interval}s")
        logger.info(f"  Longhorn Namespace: {self.config.longhorn_namespace}")
        logger.info(f"  Health Port: {self.config.health_port}")
        logger.info(f"  Use Watch API: {self.config.use_watch_api}")
        logger.info(f"  Max Retries: {self.config.max_retries}")
        logger.info(f"  Log Level: {self.config.log_level}")
        
        # Register signal handlers
        signal.signal(signal.SIGTERM, signal_handler)
        signal.signal(signal.SIGINT, signal_handler)
        
        # Start health server
        start_health_server(self.config.health_port)
        healthy = True
        
        # Initial reconciliation
        if self.reconcile():
            metrics.reconcile_count += 1
            metrics.last_reconcile_time = time.time()
            metrics.shares_managed = len(self.current_shares)
            ready = True
        
        # Choose between watch API and polling
        if self.config.use_watch_api:
            logger.info("Using Kubernetes Watch API for real-time updates")
            try:
                self.watch_pvcs()
            except KeyboardInterrupt:
                pass
        else:
            logger.info("Using polling mode")
            while not shutdown_event.is_set():
                try:
                    shutdown_event.wait(self.config.reconcile_interval)
                    if not shutdown_event.is_set():
                        if self.reconcile():
                            metrics.reconcile_count += 1
                            metrics.last_reconcile_time = time.time()
                            metrics.shares_managed = len(self.current_shares)
                except KeyboardInterrupt:
                    break
                except Exception as e:
                    logger.error(f"Unexpected error in main loop: {e}", exc_info=True)
                    metrics.reconcile_errors += 1
        
        logger.info("Operator shutdown complete")


if __name__ == '__main__':
    # Load Kubernetes configuration FIRST
    try:
        config.load_incluster_config()
        logger.info("Loaded in-cluster Kubernetes config")
    except config.ConfigException:
        config.load_kube_config()
        logger.info("Loaded local Kubernetes config")
    
    # Now create API client (after config is loaded)
    core_v1 = client.CoreV1Api()
    
    cfg = Config.load_from_configmap(
        core_v1,
        os.getenv('OPERATOR_NAMESPACE', 'storage'),
        os.getenv('CONFIG_MAP_NAME', 'smb-operator-config')
    )
    
    operator = SMBOperator(cfg)
    operator.run()
