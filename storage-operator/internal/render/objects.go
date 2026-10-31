package render

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"homelab/storage-operator/internal/config"
	"homelab/storage-operator/internal/shares"
)

// Labels and annotations.
const (
	LabelManagedBy       = "managed-by"
	ManagedByValue       = "smb-operator"
	LabelMigration       = "smb-operator/migration"
	AnnotVersion         = "smb-operator/version"
	AnnotSharesCount     = "smb-operator/shares-count"
	AnnotShareList       = "smb-operator/share-list"
	AnnotLastUpdated     = "smb-operator/last-updated"
	AnnotPVCStatus       = "smb-operator/status"
	AnnotPVCSharePath    = "smb-operator/share-path"
	AnnotPVCLastUpdate   = "smb-operator/last-update"
	TemplateVersion      = "2.1" // bumping this rolls the smb-server pod
	CredentialsSecret    = "smb-credentials"
	MigrationStorageClas = "longhorn"
	TimeFormat           = "2006-01-02 15:04:05 UTC"
)

// ChildLabels returns the operator Deployment's labels (incl. ArgoCD tracking) plus
// managed-by=smb-operator plus extra (get_child_labels).
func ChildLabels(operatorLabels, extra map[string]string) map[string]string {
	out := make(map[string]string, len(operatorLabels)+len(extra)+1)
	for k, v := range operatorLabels {
		out[k] = v
	}
	out[LabelManagedBy] = ManagedByValue
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func shareList(ss []shares.Share) string {
	names := make([]string, len(ss))
	for i, s := range ss {
		names[i] = s.Name
	}
	return strings.Join(names, ",")
}

func templateAnnotations(ss []shares.Share) map[string]string {
	return map[string]string{
		AnnotVersion:     TemplateVersion,
		AnnotSharesCount: strconv.Itoa(len(ss)),
		AnnotShareList:   shareList(ss),
	}
}

// Deployment renders the smb-server Deployment (generate_deployment). now feeds the
// Deployment-level last-updated annotation only; nothing time-varying is in the pod template.
func Deployment(cfg *config.Config, ss []shares.Share, operatorLabels map[string]string, now time.Time) (*appsv1.Deployment, error) {
	if err := cfg.SMBResources.Validate(); err != nil {
		return nil, err
	}
	app := cfg.SMBDeploymentName
	q := map[string]resource.Quantity{}
	for _, kind := range []string{"requests", "limits"} {
		for _, k := range []string{"cpu", "memory"} {
			v, err := resource.ParseQuantity(cfg.SMBResources[kind][k])
			if err != nil {
				return nil, fmt.Errorf("resources.smbServer.%s.%s: %w", kind, k, err)
			}
			q[kind+"."+k] = v
		}
	}

	mounts := []corev1.VolumeMount{
		{Name: "smb-config", MountPath: "/etc/samba/smb.conf", SubPath: SMBConfKey},
		// Whole directory, never subPath: subPath ConfigMaps are never updated in a running pod.
		{Name: "smb-mounts", MountPath: MountTableDir, ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: "smb-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: SMBConfigMap},
			Items:                []corev1.KeyToPath{{Key: SMBConfKey, Path: SMBConfKey}},
		}}},
		{Name: "smb-mounts", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: MountTableConfigMap},
			Optional:             ptr.To(true),
		}}},
	}
	for _, s := range ss {
		if s.MountType != shares.MountDirect {
			continue
		}
		mounts = append(mounts, corev1.VolumeMount{Name: s.PodVolumeName(), MountPath: s.Path})
	}
	for _, s := range ss {
		if s.MountType != shares.MountDirect {
			continue
		}
		volumes = append(volumes, corev1.Volume{Name: s.PodVolumeName(), VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: s.MigrationPVCName(), ReadOnly: s.ReadOnly()},
		}})
	}

	container := corev1.Container{
		Name:            "samba",
		Image:           cfg.SMBImage,
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
		Command:         []string{"/bin/bash", "-c", StartupScript(cfg, ss)},
		Env: []corev1.EnvVar{{
			Name: "SMB_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: CredentialsSecret},
				Key:                  "password",
			}},
		}},
		Ports: []corev1.ContainerPort{
			{ContainerPort: 445, Protocol: corev1.ProtocolTCP},
			{ContainerPort: 139, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: mounts,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    q["requests.cpu"],
				corev1.ResourceMemory: q["requests.memory"],
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    q["limits.cpu"],
				corev1.ResourceMemory: q["limits.memory"],
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(445)}},
			InitialDelaySeconds: 5, PeriodSeconds: 5, TimeoutSeconds: 3, SuccessThreshold: 1, FailureThreshold: 3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(445)}},
			InitialDelaySeconds: 15, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3,
		},
		Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", PreStopScript(ss)}},
		}},
	}

	depAnnots := templateAnnotations(ss)
	depAnnots[AnnotLastUpdated] = now.UTC().Format(TimeFormat)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        app,
			Namespace:   cfg.Namespace,
			Labels:      ChildLabels(operatorLabels, map[string]string{"app": app}),
			Annotations: depAnnots,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             ptr.To[int32](1),
			Selector:             &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
			Strategy:             appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			RevisionHistoryLimit: ptr.To[int32](3),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": app},
					Annotations: templateAnnotations(ss),
				},
				Spec: corev1.PodSpec{
					Containers:                    []corev1.Container{container},
					Volumes:                       volumes,
					TerminationGracePeriodSeconds: ptr.To[int64](30),
				},
			},
		},
	}, nil
}

// Service renders the smb-server LoadBalancer Service (ensure_service, create path). ownerUID
// "" means no ownerReference (the operator Deployment could not be read).
func Service(cfg *config.Config, operatorLabels map[string]string, ownerName, ownerUID string) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        cfg.SMBServiceName,
			Namespace:   cfg.Namespace,
			Labels:      ChildLabels(operatorLabels, map[string]string{"app": cfg.SMBDeploymentName}),
			Annotations: map[string]string{"service.kubernetes.io/topology-mode": "Auto"},
		},
		Spec: corev1.ServiceSpec{
			Type:           corev1.ServiceTypeLoadBalancer,
			LoadBalancerIP: cfg.SMBLoadBalancerIP,
			Selector:       map[string]string{"app": cfg.SMBDeploymentName},
			Ports: []corev1.ServicePort{
				{Name: "smb", Port: 445, TargetPort: intstr.FromInt32(445), Protocol: corev1.ProtocolTCP},
				{Name: "netbios", Port: 139, TargetPort: intstr.FromInt32(139), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if ownerUID != "" {
		svc.OwnerReferences = []metav1.OwnerReference{{
			APIVersion:         "apps/v1",
			Kind:               "Deployment",
			Name:               ownerName,
			UID:                types.UID(ownerUID),
			Controller:         ptr.To(true),
			BlockOwnerDeletion: ptr.To(true),
		}}
	}
	return svc
}

// MigrationPV renders the temp PV for a direct migration share.
func MigrationPV(s shares.Share, sizeMi string, labels map[string]string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: s.MigrationPVName(), Labels: labels},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(sizeMi)},
			VolumeMode:                    ptr.To(corev1.PersistentVolumeFilesystem),
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              MigrationStorageClas,
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "driver.longhorn.io", FSType: "ext4", VolumeHandle: s.LonghornVolumeName,
			}},
		},
	}
}

// MigrationPVC renders the temp PVC bound to MigrationPV.
func MigrationPVC(s shares.Share, namespace, sizeMi string, labels map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: s.MigrationPVCName(), Namespace: namespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To(MigrationStorageClas),
			VolumeName:       s.MigrationPVName(),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(sizeMi)},
			},
		},
	}
}
