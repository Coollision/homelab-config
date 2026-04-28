package controllers

import (
	"context"
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

	profiles := []string{}
	if stack.AWS.Profile != "" {
		profiles = append(profiles, stack.AWS.Profile)
	}
	for _, p := range stack.AWS.ExtraProfile {
		if p.Name != "" {
			profiles = append(profiles, p.Name)
		}
	}

	for _, profile := range profiles {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credsSecretName(stackName, profile), Namespace: cfg.Namespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.Labels == nil {
				secret.Labels = map[string]string{}
			}
			secret.Labels["app.kubernetes.io/managed-by"] = "aws-tunnels-operator"
			secret.Labels["proxies.homelab.io/stack"] = stackName
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
			dep.Labels = labels
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": tunnelName}}
			dep.Spec.Replicas = desiredReplicas(validCreds)
			dep.Spec.Template.ObjectMeta.Labels = map[string]string{"app": tunnelName, "proxies.homelab.io/stack": stackName}

			resources := tunnel.Resources
			if resources.Requests == nil && resources.Limits == nil {
				resources = stack.TunnelDefaults.Resources
			}
			proxyResources := tunnel.ProxyResources
			if proxyResources.Requests == nil && proxyResources.Limits == nil {
				proxyResources = stack.TunnelDefaults.ProxyResources
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
					{Name: "BASTION_NAME", Value: tunnel.BastionName},
					{Name: "REMOTE_HOST", Value: tunnel.RemoteHost},
					{Name: "REMOTE_PORT", Value: tunnel.RemotePort},
					{Name: "LOCAL_PORT", Value: fmt.Sprintf("%d", tunnel.LocalPort)},
					{Name: "RDS_INSTANCE_PREFIX", Value: tunnel.RDS.InstancePrefix},
					{Name: "RDS_CLUSTER_PREFIX", Value: tunnel.RDS.ClusterPrefix},
				},
				EnvFrom:   []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: profileSecretName}}}},
				Command:   []string{"/bin/sh", "-ec"},
				Args:      []string{`if [ -n "$RDS_CLUSTER_PREFIX" ]; then REMOTE_HOST=$(aws rds describe-db-clusters --region "$AWS_REGION" --query "DBClusters[?Status=='available'].[DBClusterIdentifier,Endpoint]" --output text | awk -v p="$RDS_CLUSTER_PREFIX" '$1 ~ "^"p {print $2}' | tail -n1); fi; if [ -z "$REMOTE_HOST" ] && [ -n "$RDS_INSTANCE_PREFIX" ]; then REMOTE_HOST=$(aws rds describe-db-instances --region "$AWS_REGION" --query "DBInstances[?DBInstanceStatus=='available'].[DBInstanceIdentifier,Endpoint.Address]" --output text | awk -v p="$RDS_INSTANCE_PREFIX" '$1 ~ "^"p {print $2}' | tail -n1); fi; INSTANCE_ID=$(aws ec2 describe-instances --region "$AWS_REGION" --filters "Name=tag:Name,Values=${BASTION_NAME}" "Name=instance-state-name,Values=running" --query "Reservations[0].Instances[0].InstanceId" --output text); if [ -z "$INSTANCE_ID" ] || [ "$INSTANCE_ID" = "None" ]; then echo "no running bastion"; exit 1; fi; if [ -z "${REMOTE_HOST}" ] || [ "$REMOTE_HOST" = "None" ]; then echo "REMOTE_HOST is required or resolvable via RDS prefix"; exit 1; fi; exec aws ssm start-session --region "$AWS_REGION" --target "$INSTANCE_ID" --document-name AWS-StartPortForwardingSessionToRemoteHost --parameters "{\"host\":[\"${REMOTE_HOST}\"],\"portNumber\":[\"${REMOTE_PORT}\"],\"localPortNumber\":[\"${LOCAL_PORT}\"]}"`},
				Resources: resources,
			}

			dep.Spec.Template.Spec.Containers[1] = corev1.Container{
				Name:            "proxy",
				Image:           proxyImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{fmt.Sprintf("tcp-listen:%d,fork,reuseaddr,bind=0.0.0.0", svcPort), fmt.Sprintf("tcp:127.0.0.1:%d", tunnel.LocalPort)},
				Ports:           []corev1.ContainerPort{{Name: "tunnel", ContainerPort: svcPort, Protocol: corev1.ProtocolTCP}},
				Resources:       proxyResources,
			}

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
