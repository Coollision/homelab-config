package controllers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type SingleStackRunner struct {
	Client        client.Client
	Namespace     string
	ConfigMapName string
	Interval      time.Duration
	ArgoAppName   string
}

const (
	awsConfigVolumeName  = "aws-config"
	awsConfigMountPath   = "/aws-config"
	awsConfigFilePath    = awsConfigMountPath + "/config"
	tunnelStateVolumeName = "tunnel-state"
	tunnelStateMountPath  = "/tmp/tunnel-state"
)

// argoTrackingID builds the argocd.argoproj.io/tracking-id annotation value used
// by ArgoCD's default annotation tracking mode. Format: <app>:<group>/<kind>:<ns>/<name>.
func argoTrackingID(appName, group, kind, namespace, name string) string {
	if group == "" {
		return fmt.Sprintf("%s:/%s:%s/%s", appName, kind, namespace, name)
	}
	return fmt.Sprintf("%s:%s/%s:%s/%s", appName, group, kind, namespace, name)
}

func ProfileKey(profile string) string {
	replacer := strings.NewReplacer("/", "_", ":", "_", " ", "_", "@", "_", "\\", "_")
	return replacer.Replace(profile)
}

func credsSecretName(stackName, profile string) string {
	return fmt.Sprintf("%s-creds-%s", stackName, strings.ToLower(ProfileKey(profile)))
}

func IsCredentialValid(secret *corev1.Secret) bool {
	if secret == nil || secret.Data == nil {
		return false
	}
	expRaw, ok := secret.Data["expiration"]
	if !ok {
		return false
	}
	exp, err := time.Parse(time.RFC3339, string(expRaw))
	if err != nil {
		return false
	}
	return time.Now().UTC().Before(exp)
}

func desiredReplicas(validCreds bool) *int32 {
	if validCreds {
		v := int32(1)
		return &v
	}
	v := int32(0)
	return &v
}

func (r *SingleStackRunner) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	_ = r.reconcileOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = r.reconcileOnce(ctx)
		}
	}
}

func (r *SingleStackRunner) reconcileOnce(ctx context.Context) error {
	cfg, err := LoadStackConfig(ctx, r.Client, r.Namespace, r.ConfigMapName)
	if err != nil {
		return err
	}
	stackName := cfg.Name
	stack := cfg.Spec
	awsConfigMapName, awsConfigText, _, err := SyncAWSConfigMap(ctx, r.Client, cfg)
	if err != nil {
		return err
	}

	profiles := cfg.DefinedAWSProfiles()

	for _, profile := range profiles {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credsSecretName(stackName, profile), Namespace: cfg.Namespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.Labels == nil {
				secret.Labels = map[string]string{}
			}
			secret.Labels["app.kubernetes.io/managed-by"] = "aws-tunnels-operator"
			secret.Labels["proxies.homelab.io/stack"] = stackName
			if r.ArgoAppName != "" {
				secret.Labels["app.kubernetes.io/instance"] = r.ArgoAppName
				if secret.Annotations == nil {
					secret.Annotations = map[string]string{}
				}
				secret.Annotations["argocd.argoproj.io/tracking-id"] = argoTrackingID(r.ArgoAppName, "", "Secret", cfg.Namespace, secret.Name)
			}
			secret.Type = corev1.SecretTypeOpaque
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			if _, ok := secret.Data["expiration"]; !ok {
				secret.Data["expiration"] = []byte("1970-01-01T00:00:00Z")
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	for _, tunnel := range stack.Tunnels {
		profile := tunnel.AWSProfile
		if profile == "" {
			profile = stack.AWS.Profile
		}
		profileSecretName := credsSecretName(stackName, profile)

		credSecret := &corev1.Secret{}
		err := r.Client.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: profileSecretName}, credSecret)
		validCreds := err == nil && IsCredentialValid(credSecret)

		tunnelName := fmt.Sprintf("%s-%s", stackName, tunnel.Name)
		svcPort := tunnel.ServicePort
		if svcPort == 0 {
			svcPort = stack.TunnelDefaults.ServicePort
		}
		if svcPort == 0 {
			svcPort = 8080
		}

		image := tunnel.Image
		if image == "" {
			image = stack.TunnelDefaults.Image
		}
		if image == "" {
			image = "amazon/aws-cli:2.27.9"
		}

		proxyImage := tunnel.ProxyImage
		if proxyImage == "" {
			proxyImage = stack.TunnelDefaults.ProxyImage
		}
		if proxyImage == "" {
			proxyImage = "alpine/socat:1.8.0.3"
		}

		awsRegion := tunnel.AWSRegion
		if awsRegion == "" {
			awsRegion = stack.AWS.Region
		}

		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: cfg.Namespace}}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
			labels := map[string]string{
				"app.kubernetes.io/name":    tunnelName,
				"app.kubernetes.io/part-of": stackName,
				"proxies.homelab.io/stack":  stackName,
			}
			if r.ArgoAppName != "" {
				labels["app.kubernetes.io/instance"] = r.ArgoAppName
			}
			dep.Labels = labels
			if r.ArgoAppName != "" {
				if dep.Annotations == nil {
					dep.Annotations = map[string]string{}
				}
				dep.Annotations["argocd.argoproj.io/tracking-id"] = argoTrackingID(r.ArgoAppName, "apps", "Deployment", cfg.Namespace, tunnelName)
			}
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": tunnelName}}
			dep.Spec.Replicas = desiredReplicas(validCreds)
			dep.Spec.Template.ObjectMeta.Labels = map[string]string{"app": tunnelName, "proxies.homelab.io/stack": stackName}
			if dep.Spec.Template.ObjectMeta.Annotations == nil {
				dep.Spec.Template.ObjectMeta.Annotations = map[string]string{}
			}
			dep.Spec.Template.ObjectMeta.Annotations["proxies.homelab.io/awsConfigHash"] = fmt.Sprintf("%x", sha256.Sum256([]byte(awsConfigText)))[:12]

			resources := tunnel.Resources
			if resources.Requests == nil && resources.Limits == nil {
				resources = stack.TunnelDefaults.Resources
			}
			proxyResources := tunnel.ProxyResources
			if proxyResources.Requests == nil && proxyResources.Limits == nil {
				proxyResources = stack.TunnelDefaults.ProxyResources
			}

			livenessInitialDelay := stack.TunnelDefaults.LivenessProbe.InitialDelaySeconds
			if livenessInitialDelay == 0 {
				livenessInitialDelay = 30
			}
			livenessPeriod := stack.TunnelDefaults.LivenessProbe.PeriodSeconds
			if livenessPeriod == 0 {
				livenessPeriod = 30
			}
			livenessFailureThreshold := stack.TunnelDefaults.LivenessProbe.FailureThreshold
			if livenessFailureThreshold == 0 {
				livenessFailureThreshold = 3
			}

			if dep.Spec.Template.Spec.Containers == nil || len(dep.Spec.Template.Spec.Containers) < 2 {
				dep.Spec.Template.Spec.Containers = []corev1.Container{{}, {}}
			}

			dep.Spec.Template.Spec.Containers[0] = corev1.Container{
				Name:            "tunnel",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env: []corev1.EnvVar{
					{Name: "AWS_REGION", Value: awsRegion},
					{Name: "AWS_PROFILE", Value: profile},
					{Name: "AWS_SDK_LOAD_CONFIG", Value: "1"},
					{Name: "AWS_CONFIG_FILE", Value: awsConfigFilePath},
					{Name: "BASTION_NAME", Value: tunnel.BastionName},
					{Name: "REMOTE_HOST", Value: tunnel.RemoteHost},
					{Name: "REMOTE_PORT", Value: tunnel.RemotePort},
					{Name: "LOCAL_PORT", Value: fmt.Sprintf("%d", tunnel.LocalPort)},
					{Name: "RDS_INSTANCE_PREFIX", Value: tunnel.RDS.InstancePrefix},
					{Name: "RDS_CLUSTER_PREFIX", Value: tunnel.RDS.ClusterPrefix},				{Name: "TUNNEL_NAME", Value: tunnelName},
				{Name: "HOME", Value: "/root"},				},
				EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: profileSecretName}}}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: awsConfigVolumeName, MountPath: awsConfigMountPath, ReadOnly: true},
					{Name: tunnelStateVolumeName, MountPath: tunnelStateMountPath},
				},
				Command:   []string{"/usr/local/bin/tunnel-runner"},
				Resources: resources,
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "[ -s /tmp/tunnel-state/state ]"}},
					},
					InitialDelaySeconds: livenessInitialDelay,
					PeriodSeconds:       livenessPeriod,
					FailureThreshold:    livenessFailureThreshold,
				},
			}

			dep.Spec.Template.Spec.Containers[1] = corev1.Container{
				Name:            "proxy",
				Image:           proxyImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{fmt.Sprintf("tcp-listen:%d,fork,reuseaddr,bind=0.0.0.0", svcPort), fmt.Sprintf("tcp:127.0.0.1:%d", tunnel.LocalPort)},
				Ports:           []corev1.ContainerPort{{Name: "tunnel", ContainerPort: svcPort, Protocol: corev1.ProtocolTCP}},
				Resources:       proxyResources,
			}
			dep.Spec.Template.Spec.Volumes = upsertVolume(dep.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: tunnelStateVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
			dep.Spec.Template.Spec.Volumes = upsertVolume(dep.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: awsConfigVolumeName,
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: awsConfigMapName},
					Items:                []corev1.KeyToPath{{Key: awsConfigDataKey, Path: "config"}},
				}},
			})

			if stack.NodeAffinity.ExcludedType != "" {
				dep.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "type", Operator: corev1.NodeSelectorOpNotIn, Values: []string{stack.NodeAffinity.ExcludedType}}}}}}}}
			}
			return nil
		})
		if err != nil {
			return err
		}

		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: cfg.Namespace}}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			svc.Labels = map[string]string{"proxies.homelab.io/stack": stackName}
			if r.ArgoAppName != "" {
				svc.Labels["app.kubernetes.io/instance"] = r.ArgoAppName
				if svc.Annotations == nil {
					svc.Annotations = map[string]string{}
				}
				svc.Annotations["argocd.argoproj.io/tracking-id"] = argoTrackingID(r.ArgoAppName, "", "Service", cfg.Namespace, tunnelName)
			}
			svc.Spec.Selector = map[string]string{"app": tunnelName}
			svc.Spec.Ports = []corev1.ServicePort{{Name: "tunnel", Port: svcPort, TargetPort: intstr.FromInt32(svcPort)}}
			return nil
		})
		if err != nil {
			return err
		}

		ingressMode := tunnel.IngressMode
		if ingressMode == "" {
			ingressMode = "http"
		}
		if ingressMode == "tcp" {
			ing := &unstructured.Unstructured{}
			ing.SetAPIVersion("traefik.io/v1alpha1")
			ing.SetKind("IngressRouteTCP")
			ing.SetNamespace(cfg.Namespace)
			ing.SetName(tunnelName)
			_, _ = controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
				if r.ArgoAppName != "" {
					ing.SetLabels(map[string]string{"app.kubernetes.io/instance": r.ArgoAppName})
					ing.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": argoTrackingID(r.ArgoAppName, "traefik.io", "IngressRouteTCP", cfg.Namespace, tunnelName)})
				}
				ing.Object["spec"] = map[string]any{
					"entryPoints": []any{"websecure"},
					"routes":      []any{map[string]any{"match": fmt.Sprintf("HostSNI(`%s`)", tunnel.Host), "services": []any{map[string]any{"name": tunnelName, "namespace": cfg.Namespace, "port": svcPort}}}},
					"tls":         map[string]any{"passthrough": tunnel.TLS.Passthrough},
				}
				return nil
			})
		} else {
			ing := &unstructured.Unstructured{}
			ing.SetAPIVersion("traefik.io/v1alpha1")
			ing.SetKind("IngressRoute")
			ing.SetNamespace(cfg.Namespace)
			ing.SetName(tunnelName)
			_, _ = controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
				if r.ArgoAppName != "" {
					ing.SetLabels(map[string]string{"app.kubernetes.io/instance": r.ArgoAppName})
					ing.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": argoTrackingID(r.ArgoAppName, "traefik.io", "IngressRoute", cfg.Namespace, tunnelName)})
				}
				ing.Object["spec"] = map[string]any{
					"entryPoints": []any{"websecure"},
					"routes":      []any{map[string]any{"kind": "Rule", "match": fmt.Sprintf("Host(`%s`)", tunnel.Host), "services": []any{map[string]any{"kind": "Service", "name": tunnelName, "namespace": cfg.Namespace, "port": svcPort, "scheme": "http"}}}},
					"tls":         map[string]any{},
				}
				return nil
			})
		}
	}

	return nil
}

func upsertVolume(volumes []corev1.Volume, volume corev1.Volume) []corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == volume.Name {
			volumes[i] = volume
			return volumes
		}
	}
	return append(volumes, volume)
}
