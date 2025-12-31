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
    ('smb', 'loadBalancerIP'),
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
    """Operator configuration from ConfigMap"""
    # Required settings (must be provided in ConfigMap)
    namespace: str
    smb_loadbalancer_ip: str
    
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
    smb_guest_account: str = 'nobody'
    
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
        """Load configuration from ConfigMap. Raises error if required keys are missing."""
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
            
            return cls(
                namespace=data['operator']['namespace'],
                smb_loadbalancer_ip=data['smb']['loadBalancerIP'],
                reconcile_interval=data.get('operator', {}).get('reconcileInterval', 30),
                use_watch_api=data.get('operator', {}).get('useWatchAPI', True),
                log_level=data.get('operator', {}).get('logLevel', 'INFO'),
                health_port=data.get('operator', {}).get('healthPort', 8080),
                
                smb_deployment_name=data.get('smb', {}).get('deploymentName', 'smb-server'),
                smb_service_name=data.get('smb', {}).get('serviceName', 'smb-server'),
                smb_image=data.get('smb', {}).get('image', 'dperson/samba:latest'),
                smb_workgroup=data.get('smb', {}).get('workgroup', 'WORKGROUP'),
                smb_server_string=data.get('smb', {}).get('serverString', 'Homelab Storage'),
                smb_guest_account=data.get('smb', {}).get('guestAccount', 'nobody'),
                
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
    """Represents a single SMB share configuration"""
    name: str
    path: str
    namespace: str
    pvc_name: str
    access_mode: str
    nfs_server: str
    nfs_path: str
    
    @property
    def readonly(self) -> bool:
        return self.access_mode == 'read-only'
    
    @property
    def unique_id(self) -> str:
        return f"{self.namespace}/{self.pvc_name}"


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
        """Get operator deployment UID for owner references"""
        if self.operator_uid:
            return self.operator_uid
        
        try:
            deployment = self.apps_v1.read_namespaced_deployment(
                'smb-operator',
                self.config.namespace
            )
            self.operator_uid = deployment.metadata.uid
            return self.operator_uid
        except Exception as e:
            logger.warning(f"Failed to get operator UID: {e}")
            return None
    
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
        """Generate Samba configuration"""
        lines = [
            "[global]",
            f"workgroup = {self.config.smb_workgroup}",
            f"server string = {self.config.smb_server_string}",
            "security = user",
            "map to guest = Bad User",
            f"guest account = {self.config.smb_guest_account}",
            "log level = 1",
            ""
        ]
        
        for share in shares:
            lines.extend([
                f"[{share.name}]",
                f"path = {share.path}",
                "available = yes",
                "browseable = yes",
                "public = yes",
                "writable = " + ("no" if share.readonly else "yes"),
                "read only = " + ("yes" if share.readonly else "no"),
                "guest ok = yes",
                "force user = root",
                "force group = root",
                "create mask = 0755",
                "directory mask = 0755",
                ""
            ])
        
        return "\n".join(lines)
    
    def generate_startup_script(self, shares: List[SMBShare]) -> str:
        """Generate container startup script with NFS mounts"""
        lines = [
            "#!/bin/bash",
            "set -e",
            "echo 'Starting SMB server initialization...'",
            "echo ''",
            ""
        ]
        
        for share in shares:
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
            "exec /usr/sbin/smbd -F -S --no-process-group"
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
            ports=[
                client.V1ContainerPort(container_port=445, protocol='TCP'),
                client.V1ContainerPort(container_port=139, protocol='TCP')
            ],
            volume_mounts=[
                client.V1VolumeMount(name='smb-config', mount_path='/etc/samba/smb.conf', sub_path='smb.conf')
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
                labels={'app': self.config.smb_deployment_name, 'managed-by': 'smb-operator'},
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
                        labels={'app': self.config.smb_deployment_name},
                        annotations={
                            'service.kubernetes.io/topology-mode': 'Auto'  # Suppresses Endpoints deprecation warning
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
                    metadata=client.V1ObjectMeta(name=cm_name, namespace=self.config.namespace),
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
            
            if existing_strategy and new_strategy and existing_strategy != new_strategy:
                logger.info(f"Strategy change detected: {existing_strategy} -> {new_strategy}, using replace operation")
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
            
            shares = self.discover_shares()
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
        """Watch for PVC changes using Kubernetes Watch API"""
        logger.info("Starting PVC watch...")
        w = watch.Watch()
        
        while not shutdown_event.is_set():
            try:
                for event in w.stream(
                    self.core_v1.list_persistent_volume_claim_for_all_namespaces,
                    label_selector='smb-access',
                    timeout_seconds=self.config.reconcile_interval
                ):
                    if shutdown_event.is_set():
                        break
                    
                    event_type = event['type']
                    pvc = event['object']
                    logger.info(f"PVC event: {event_type} {pvc.metadata.namespace}/{pvc.metadata.name}")
                    
                    if self.reconcile():
                        metrics.reconcile_count += 1
                        metrics.last_reconcile_time = time.time()
                        metrics.shares_managed = len(self.current_shares)
                    
            except ApiException as e:
                if e.status == 410:
                    logger.warning("Watch expired, restarting...")
                    continue
                else:
                    logger.error(f"Watch error: {e}", exc_info=True)
                    time.sleep(10)
            except Exception as e:
                logger.error(f"Unexpected watch error: {e}", exc_info=True)
                time.sleep(10)
    
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
    # Load configuration
    core_v1 = client.CoreV1Api()
    try:
        config.load_incluster_config()
    except:
        config.load_kube_config()
    
    cfg = Config.load_from_configmap(
        core_v1,
        os.getenv('OPERATOR_NAMESPACE', 'storage'),
        os.getenv('CONFIG_MAP_NAME', 'smb-operator-config')
    )
    
    operator = SMBOperator(cfg)
    operator.run()
