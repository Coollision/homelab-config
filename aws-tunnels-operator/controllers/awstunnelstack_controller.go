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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	proxyv1alpha1 "homelab/aws-tunnels-operator/api/v1alpha1"
)

type AWSTunnelStackReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

func (r *AWSTunnelStackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var stack proxyv1alpha1.AWSTunnelStack
	if err := r.Get(ctx, req.NamespacedName, &stack); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	profiles := []string{stack.Spec.AWS.Profile}
	for _, p := range stack.Spec.AWS.ExtraProfile {
		profiles = append(profiles, p.Name)
	}

	for _, profile := range profiles {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credsSecretName(stack.Name, profile), Namespace: stack.Namespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.Labels == nil {
				secret.Labels = map[string]string{}
			}
			secret.Labels["app.kubernetes.io/managed-by"] = "aws-tunnels-operator"
			secret.Labels["proxies.homelab.io/stack"] = stack.Name
			secret.Type = corev1.SecretTypeOpaque
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			if _, ok := secret.Data["expiration"]; !ok {
				secret.Data["expiration"] = []byte("1970-01-01T00:00:00Z")
			}
			return controllerutil.SetControllerReference(&stack, secret, r.Scheme)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	for _, tunnel := range stack.Spec.Tunnels {
		profile := tunnel.AWSProfile
		if profile == "" {
			profile = stack.Spec.AWS.Profile
		}
		profileSecretName := credsSecretName(stack.Name, profile)

		credSecret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Namespace: stack.Namespace, Name: profileSecretName}, credSecret)
		validCreds := err == nil && IsCredentialValid(credSecret)

		tunnelName := fmt.Sprintf("%s-%s", stack.Name, tunnel.Name)
		svcPort := tunnel.ServicePort
		if svcPort == 0 {
			svcPort = stack.Spec.TunnelDefaults.ServicePort
		}
		if svcPort == 0 {
			svcPort = 8080
		}
		image := tunnel.Image
		if image == "" {
			image = stack.Spec.TunnelDefaults.Image
		}
		if image == "" {
			image = "amazon/aws-cli:2.27.9"
		}
		proxyImage := tunnel.ProxyImage
		if proxyImage == "" {
			proxyImage = stack.Spec.TunnelDefaults.ProxyImage
		}
		if proxyImage == "" {
			proxyImage = "alpine/socat:1.8.0.3"
		}
		awsRegion := tunnel.AWSRegion
		if awsRegion == "" {
			awsRegion = stack.Spec.AWS.Region
		}

		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: stack.Namespace}}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
			labels := map[string]string{
				"app.kubernetes.io/name": tunnelName,
				"app.kubernetes.io/part-of": stack.Name,
				"proxies.homelab.io/stack": stack.Name,
			}
			dep.Labels = labels
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": tunnelName}}
			dep.Spec.Replicas = desiredReplicas(validCreds)
			dep.Spec.Template.ObjectMeta.Labels = map[string]string{"app": tunnelName, "proxies.homelab.io/stack": stack.Name}

			resources := tunnel.Resources
			if resources.Requests == nil && resources.Limits == nil {
				resources = stack.Spec.TunnelDefaults.Resources
			}

			proxyResources := tunnel.ProxyResources
			if proxyResources.Requests == nil && proxyResources.Limits == nil {
				proxyResources = stack.Spec.TunnelDefaults.ProxyResources
			}

			if dep.Spec.Template.Spec.Containers == nil || len(dep.Spec.Template.Spec.Containers) == 0 {
				dep.Spec.Template.Spec.Containers = []corev1.Container{{}, {}}
			}

			dep.Spec.Template.Spec.Containers[0] = corev1.Container{
				Name:            "tunnel",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env: []corev1.EnvVar{
					{Name: "AWS_REGION", Value: awsRegion},
					{Name: "BASTION_NAME", Value: tunnel.BastionName},
					{Name: "REMOTE_HOST", Value: tunnel.RemoteHost},
					{Name: "REMOTE_PORT", Value: tunnel.RemotePort},
					{Name: "LOCAL_PORT", Value: fmt.Sprintf("%d", tunnel.LocalPort)},
				},
				EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: profileSecretName}}}},
				Command: []string{"/bin/sh", "-ec"},
				Args: []string{`INSTANCE_ID=$(aws ec2 describe-instances --region "$AWS_REGION" --filters "Name=tag:Name,Values=${BASTION_NAME}" "Name=instance-state-name,Values=running" --query "Reservations[0].Instances[0].InstanceId" --output text); if [ -z "$INSTANCE_ID" ] || [ "$INSTANCE_ID" = "None" ]; then echo "no running bastion"; exit 1; fi; if [ -z "${REMOTE_HOST}" ]; then echo "REMOTE_HOST is required"; exit 1; fi; exec aws ssm start-session --region "$AWS_REGION" --target "$INSTANCE_ID" --document-name AWS-StartPortForwardingSessionToRemoteHost --parameters "{\"host\":[\"${REMOTE_HOST}\"],\"portNumber\":[\"${REMOTE_PORT}\"],\"localPortNumber\":[\"${LOCAL_PORT}\"]}"`},
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

			if stack.Spec.NodeAffinity.ExcludedType != "" {
				dep.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "type", Operator: corev1.NodeSelectorOpNotIn, Values: []string{stack.Spec.NodeAffinity.ExcludedType}}}}}}}}
			}

			return controllerutil.SetControllerReference(&stack, dep, r.Scheme)
		})
		if err != nil {
			return ctrl.Result{}, err
		}

		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: stack.Namespace}}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			svc.Labels = map[string]string{"proxies.homelab.io/stack": stack.Name}
			svc.Spec.Selector = map[string]string{"app": tunnelName}
			svc.Spec.Ports = []corev1.ServicePort{{Name: "tunnel", Port: svcPort, TargetPort: intstr.FromInt32(svcPort)}}
			return controllerutil.SetControllerReference(&stack, svc, r.Scheme)
		})
		if err != nil {
			return ctrl.Result{}, err
		}

		ingressMode := tunnel.IngressMode
		if ingressMode == "" {
			ingressMode = "http"
		}
		if ingressMode == "tcp" {
			ing := &unstructured.Unstructured{}
			ing.SetAPIVersion("traefik.io/v1alpha1")
			ing.SetKind("IngressRouteTCP")
			ing.SetNamespace(stack.Namespace)
			ing.SetName(tunnelName)
			_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
				ing.Object["spec"] = map[string]any{
					"entryPoints": []any{"websecure"},
					"routes": []any{map[string]any{"match": fmt.Sprintf("HostSNI(`%s`)", tunnel.Host), "services": []any{map[string]any{"name": tunnelName, "namespace": stack.Namespace, "port": svcPort}}}},
					"tls": map[string]any{"passthrough": tunnel.TLS.Passthrough},
				}
				return controllerutil.SetControllerReference(&stack, ing, r.Scheme)
			})
			if err != nil {
				logger.Error(err, "failed to reconcile IngressRouteTCP", "tunnel", tunnelName)
			}
		} else {
			ing := &unstructured.Unstructured{}
			ing.SetAPIVersion("traefik.io/v1alpha1")
			ing.SetKind("IngressRoute")
			ing.SetNamespace(stack.Namespace)
			ing.SetName(tunnelName)
			_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
				ing.Object["spec"] = map[string]any{
					"entryPoints": []any{"websecure"},
					"routes": []any{map[string]any{"kind": "Rule", "match": fmt.Sprintf("Host(`%s`)", tunnel.Host), "services": []any{map[string]any{"kind": "Service", "name": tunnelName, "namespace": stack.Namespace, "port": svcPort, "scheme": "http"}}}},
					"tls": map[string]any{},
				}
				return controllerutil.SetControllerReference(&stack, ing, r.Scheme)
			})
			if err != nil {
				logger.Error(err, "failed to reconcile IngressRoute", "tunnel", tunnelName)
			}
		}
	}

	stack.Status.ObservedGeneration = stack.Generation
	stack.Status.ManagedTunnels = int32(len(stack.Spec.Tunnels))
	if err := r.Status().Update(ctx, &stack); err != nil {
		logger.Error(err, "failed to update status")
	}

	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

func (r *AWSTunnelStackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1alpha1.AWSTunnelStack{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
