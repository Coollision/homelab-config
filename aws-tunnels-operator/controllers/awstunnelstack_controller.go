package controllers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type SingleStackRunner struct {
	Client        client.Client
	Namespace     string
	ConfigMapName string
	Interval      time.Duration
	ArgoAppName   string

	// Per-profile creds-refresh backoff. reconcileOnce runs on a single goroutine (ticker), so these
	// need no locking. When an auto-refresh fails — typically because the SSO session has expired and
	// the refresh token is rejected — the profile is parked in an exponentially growing cooldown so
	// the operator stops re-hitting AWS every tick. The cooldown is cleared on success, when creds no
	// longer need refreshing, or when a fresh login changes the token cache (lastTokenVersion).
	refreshNextAttempt map[string]time.Time
	refreshFailures    map[string]int
	lastTokenVersion   string
}

const (
	awsConfigVolumeName   = "aws-config"
	awsConfigMountPath    = "/aws-config"
	awsConfigFilePath     = awsConfigMountPath + "/config"
	tunnelStateVolumeName = "tunnel-state"
	tunnelStateMountPath  = "/tmp/tunnel-state"

	// credsVolumeName/credsMountPath are used in refresh mode (stack.aws.useRefresh): the profile's
	// creds Secret is mounted read-only so the tunnel-runner reads rotated STS creds from disk on
	// demand (AWS_CREDS_DIR), instead of receiving them as immutable env vars that force a pod roll
	// on every ~hourly rotation.
	credsVolumeName = "aws-creds"
	credsMountPath  = "/var/run/aws-creds"

	// Creds-refresh failure backoff bounds: a failing profile is retried after refreshBackoffBase,
	// doubling each consecutive failure up to refreshBackoffMax, instead of every reconcile tick.
	refreshBackoffBase = 1 * time.Minute
	refreshBackoffMax  = 15 * time.Minute

	labelStack    = "proxies.homelab.io/stack"
	labelInstance = "app.kubernetes.io/instance"
	annotTracking = "argocd.argoproj.io/tracking-id"
	traefikAPI    = "traefik.io/v1alpha1"

	// annotManuallyStopped, when "true" on a tunnel Deployment, forces it to replicas=0 even when
	// credentials are valid. Set/cleared by the auth-server stop/start toggle and respected by the
	// reconcile loop so a manual stop survives the next reconcile tick.
	annotManuallyStopped = "proxies.homelab.io/manuallyStopped"

	// annotManualReplicas pins a tunnel to an operator-chosen replica count, overriding the
	// configured value. Set by the auth-server scale control; cleared by scaling back to "auto".
	// Like annotManuallyStopped it is read by the reconcile loop, so a manual scale survives the
	// next tick instead of being reverted ~30s later.
	//
	// Replicas matter because AWS rate-limits each SSM session to ~0.7 MB/s and all connections
	// through one tunnel are smux-multiplexed onto a single datachannel. Extra replicas therefore
	// add throughput only for workloads that open several connections (the Service spreads them
	// across pods, each with its own session) — a single stream gains nothing.
	annotManualReplicas = "proxies.homelab.io/manualReplicas"

	// maxTunnelReplicas bounds the scale control. Each replica holds an independent SSM session,
	// so this is a guard against a typo opening dozens of sessions against one bastion.
	maxTunnelReplicas = 10

	// Defaults for the compressed SSH transport (see SSHSpec). 2222 is a loopback-only port
	// inside the pod, used to reach the bastion's :22 through the SSM forward.
	defaultSSHUser      = "ec2-user"
	defaultLocalSSHPort = int32(2222)
)

// argoTrackingID builds a NON-SELF-REFERENCING argocd.argoproj.io/tracking-id annotation.
// By pointing every operator-managed resource at the stack ConfigMap (which ArgoCD already
// owns), the resources appear in the ArgoCD UI but are never diffed or pruned by ArgoCD.
// See: https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/#non-self-referencing-annotations
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

// desiredReplicas returns the replica count for a tunnel Deployment. Without valid credentials
// the tunnel is scaled to zero regardless of what was requested — the runner cannot start an SSM
// session anyway, and the operator re-scales it once creds arrive.
func desiredReplicas(validCreds bool, want int32) *int32 {
	v := int32(0)
	if validCreds {
		v = clampReplicas(want)
	}
	return &v
}

// clampReplicas keeps a requested count within [1, maxTunnelReplicas]. Zero is not a valid
// *configured* value — stopping a tunnel is expressed with annotManuallyStopped, so that "off"
// stays a distinct, explicit state rather than an easily-mistyped replica count.
func clampReplicas(want int32) int32 {
	switch {
	case want < 1:
		return 1
	case want > maxTunnelReplicas:
		return maxTunnelReplicas
	default:
		return want
	}
}

// resolveReplicas picks the configured replica count for a tunnel: per-tunnel value, else the
// stack default, else 1.
func resolveReplicas(tunnel TunnelSpec, defaults TunnelDefaultSpec) int32 {
	if tunnel.Replicas != nil {
		return clampReplicas(*tunnel.Replicas)
	}
	if defaults.Replicas != nil {
		return clampReplicas(*defaults.Replicas)
	}
	return 1
}

// tcpTLSSpec renders the `tls` block of an IngressRouteTCP.
//
// Passthrough forwards the client's TLS session untouched (payload stays ciphertext, so it cannot
// be compressed). Otherwise TLS terminates at the ingress using the supplied secret or ACME
// resolver, which keeps SNI routing and client-side `sslmode=require` working while making the
// payload compressible from the ingress inwards.
func tcpTLSSpec(spec TLSSpec) map[string]any {
	if spec.Passthrough {
		// A passthrough route must not also carry a certificate — Traefik cannot do both.
		return map[string]any{"passthrough": true}
	}
	out := map[string]any{"passthrough": false}
	if spec.SecretName != "" {
		out["secretName"] = spec.SecretName
	}
	if spec.CertResolver != "" {
		out["certResolver"] = spec.CertResolver
	}
	return out
}

// resolveSSH merges the per-tunnel SSH settings over the stack defaults and fills in the blanks.
// A tunnel opts in by setting ssh.enabled itself, or inherits the stack default.
func resolveSSH(tunnel TunnelSpec, defaults TunnelDefaultSpec) SSHSpec {
	out := defaults.SSH
	if tunnel.SSH.Enabled {
		out.Enabled = true
	}
	if tunnel.SSH.Compression != nil {
		out.Compression = tunnel.SSH.Compression
	}
	if tunnel.SSH.User != "" {
		out.User = tunnel.SSH.User
	}
	if tunnel.SSH.LocalSSHPort != 0 {
		out.LocalSSHPort = tunnel.SSH.LocalSSHPort
	}

	if out.User == "" {
		out.User = defaultSSHUser
	}
	if out.LocalSSHPort == 0 {
		out.LocalSSHPort = defaultLocalSSHPort
	}
	// Compression is the entire point of this mode, so it is on unless explicitly disabled.
	if out.Compression == nil {
		on := true
		out.Compression = &on
	}
	return out
}

// manualReplicaOverride reads annotManualReplicas off a Deployment. A missing, empty or
// unparseable annotation means "no override" so a hand-edited bad value falls back to config
// rather than wedging the tunnel.
func manualReplicaOverride(dep *appsv1.Deployment) (int32, bool) {
	raw, ok := dep.Annotations[annotManualReplicas]
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return clampReplicas(int32(n)), true
}

func (r *SingleStackRunner) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	log := ctrl.LoggerFrom(ctx)
	if err := r.reconcileOnce(ctx); err != nil {
		log.Error(err, "reconcile failed")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.reconcileOnce(ctx); err != nil {
				log.Error(err, "reconcile failed")
			}
		}
	}
}

// refreshInCooldown reports whether a profile's creds refresh is still parked after a recent failure.
func (r *SingleStackRunner) refreshInCooldown(profile string, now time.Time) bool {
	t, ok := r.refreshNextAttempt[profile]
	return ok && now.Before(t)
}

// recordRefreshFailure parks a profile in an exponentially growing cooldown (capped) and returns it.
func (r *SingleStackRunner) recordRefreshFailure(profile string, now time.Time) time.Duration {
	if r.refreshNextAttempt == nil {
		r.refreshNextAttempt = map[string]time.Time{}
		r.refreshFailures = map[string]int{}
	}
	r.refreshFailures[profile]++
	shift := r.refreshFailures[profile] - 1
	if shift > 8 { // clamp so the shift can't overflow; 1m<<8 already far exceeds the cap
		shift = 8
	}
	backoff := refreshBackoffBase << shift
	if backoff > refreshBackoffMax || backoff <= 0 {
		backoff = refreshBackoffMax
	}
	r.refreshNextAttempt[profile] = now.Add(backoff)
	return backoff
}

// clearRefreshBackoff resets a profile's failure state (on success or when it no longer needs one).
func (r *SingleStackRunner) clearRefreshBackoff(profile string) {
	delete(r.refreshNextAttempt, profile)
	delete(r.refreshFailures, profile)
}

func (r *SingleStackRunner) reconcileOnce(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)
	cfg, err := LoadStackConfig(ctx, r.Client, r.Namespace, r.ConfigMapName)
	if err != nil {
		return err
	}
	stackName := cfg.Name
	stack := cfg.Spec
	awsConfigMapName, awsConfigText, awsProfiles, err := SyncAWSConfigMap(ctx, r.Client, cfg)
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
			secret.Labels[labelStack] = stackName
			if r.ArgoAppName != "" {
				secret.Labels[labelInstance] = r.ArgoAppName
				if secret.Annotations == nil {
					secret.Annotations = map[string]string{}
				}
				secret.Annotations[annotTracking] = argoTrackingID(r.ArgoAppName, "", "ConfigMap", r.Namespace, r.ConfigMapName)
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

	// --- Silent auto-refresh: mint fresh STS creds from the captured SSO token, no login ---
	// Seeds the cached token onto disk, refreshes each profile whose creds are expired or near
	// expiry, then persists any rotated refresh token back to the Secret. Profiles whose creds
	// actually changed are rolled below so running tunnels pick up the new creds.
	profilesToRoll := map[string]bool{}
	if stack.AWS.UseRefresh {
		// Skip this tick's refresh if an interactive login is writing the cache (it holds the lock
		// for its whole duration). TryLock avoids stalling reconcile behind a multi-minute device-
		// code login; the next tick retries.
		if ssoCacheMu.TryLock() {
			if seeded, tokenVer, serr := seedTokenCache(ctx, r.Client, cfg.Namespace, stackName); serr != nil {
				log.Error(serr, "failed to seed sso token cache")
			} else if seeded {
				now := time.Now().UTC()
				// A fresh login (or the first seed) changes the token cache — clear all cooldowns so a
				// re-login recovers immediately instead of waiting out a backoff from the dead session.
				if tokenVer != r.lastTokenVersion {
					r.refreshNextAttempt = map[string]time.Time{}
					r.refreshFailures = map[string]int{}
					r.lastTokenVersion = tokenVer
				}
				for _, profile := range profiles {
					credSecret := &corev1.Secret{}
					_ = r.Client.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: credsSecretName(stackName, profile)}, credSecret)
					if !credsNeedRefresh(credSecret, credsRefreshAhead) {
						r.clearRefreshBackoff(profile)
						continue
					}
					// Don't re-hit AWS every tick for a profile whose refresh is failing (e.g. the SSO
					// session has expired); wait out its cooldown.
					if r.refreshInCooldown(profile, now) {
						continue
					}
					changed, rerr := r.refreshProfileCredentials(ctx, cfg, profile, awsConfigText, awsProfiles)
					if rerr != nil {
						backoff := r.recordRefreshFailure(profile, now)
						log.Error(rerr, "sso token refresh failed; backing off (a fresh login may be required)", "profile", profile, "retryAfter", backoff)
						continue
					}
					r.clearRefreshBackoff(profile)
					if changed {
						profilesToRoll[profile] = true
						log.Info("refreshed STS creds from cached SSO token", "profile", profile)
					}
				}
				if perr := persistTokenCache(ctx, r.Client, cfg.Namespace, stackName); perr != nil {
					log.Error(perr, "failed to persist rotated sso token cache")
				}
			}
			ssoCacheMu.Unlock()
		} else {
			log.Info("skipping sso token refresh this tick; an interactive login is in progress")
		}
	}

	rollAt := time.Now().UTC().Format(time.RFC3339)

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
				labelStack:                  stackName,
			}
			if r.ArgoAppName != "" {
				labels[labelInstance] = r.ArgoAppName
			}
			dep.Labels = labels
			if r.ArgoAppName != "" {
				if dep.Annotations == nil {
					dep.Annotations = map[string]string{}
				}
				dep.Annotations[annotTracking] = argoTrackingID(r.ArgoAppName, "", "ConfigMap", r.Namespace, r.ConfigMapName)
			}
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": tunnelName}}
			// In refresh mode the pod reads rotated STS creds from a mounted Secret on demand, so it
			// stays up across rotations; in legacy mode creds are immutable env vars and the pod is
			// rolled when they change. This flag drives both the container wiring and the roll below.
			useRefresh := stack.AWS.UseRefresh
			// A manual stop (set via the auth-server toggle) pins the tunnel down even when
			// creds are valid — e.g. keeping a prod tunnel closed until explicitly started.
			manuallyStopped := dep.Annotations[annotManuallyStopped] == "true"
			// A manual scale set via the auth-server UI wins over the configured count, so an
			// operator bumping replicas to push more throughput isn't reverted on the next tick.
			wantReplicas := resolveReplicas(tunnel, stack.TunnelDefaults)
			if manual, ok := manualReplicaOverride(dep); ok {
				wantReplicas = manual
			}
			dep.Spec.Replicas = desiredReplicas(validCreds && !manuallyStopped, wantReplicas)
			// Surge the replacement pod up before retiring the old one so the Service never has
			// zero endpoints across a roll (config change, or a creds roll in legacy mode). Existing
			// TCP streams through the old pod still reset once at the swap.
			dep.Spec.Strategy = appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
					MaxSurge:       &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
				},
			}
			zero := int32(0)
			dep.Spec.RevisionHistoryLimit = &zero
			dep.Spec.Template.ObjectMeta.Labels = map[string]string{"app": tunnelName, labelStack: stackName}
			if dep.Spec.Template.ObjectMeta.Annotations == nil {
				dep.Spec.Template.ObjectMeta.Annotations = map[string]string{}
			}
			dep.Spec.Template.ObjectMeta.Annotations["proxies.homelab.io/awsConfigHash"] = fmt.Sprintf("%x", sha256.Sum256([]byte(awsConfigText)))[:12]
			// Legacy mode only: roll this profile's tunnels onto creds the loop just refreshed,
			// because immutable env-var creds don't hot-reload. In refresh mode the pod re-reads the
			// mounted creds itself, so no roll is needed (that's the whole point — no hourly drop).
			if profilesToRoll[profile] && !useRefresh {
				dep.Spec.Template.ObjectMeta.Annotations["proxies.homelab.io/restartedAt"] = rollAt
			}

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

			if len(dep.Spec.Template.Spec.Containers) < 2 {
				dep.Spec.Template.Spec.Containers = []corev1.Container{{}, {}}
			}

			// Credentials reach the tunnel differently per mode. Refresh mode: the profile's creds
			// Secret is mounted read-only and the runner reads rotated creds on demand (AWS_CREDS_DIR)
			// — no static AWS_* env (which would shadow them) and no SSO/aws-config in the pod, since
			// creds resolve from files. Legacy mode: creds come via EnvFrom and the SSO aws-config is
			// mounted (prior behavior).
			tunnelEnv := []corev1.EnvVar{
				{Name: "AWS_REGION", Value: awsRegion},
				{Name: "BASTION_NAME", Value: tunnel.BastionName},
				{Name: "REMOTE_HOST", Value: tunnel.RemoteHost},
				{Name: "REMOTE_PORT", Value: tunnel.RemotePort},
				{Name: "LOCAL_PORT", Value: fmt.Sprintf("%d", tunnel.LocalPort)},
				{Name: "RDS_INSTANCE_PREFIX", Value: tunnel.RDS.InstancePrefix},
				{Name: "RDS_CLUSTER_PREFIX", Value: tunnel.RDS.ClusterPrefix},
				{Name: "TUNNEL_NAME", Value: tunnelName},
				{Name: "HOME", Value: "/root"},
			}
			// Compressed SSH transport: the runner forwards the bastion's :22 over SSM and runs
			// `ssh -C -L` through it, so payloads are gzipped before crossing AWS's per-session
			// rate limit. Off unless configured — it is a loss on incompressible traffic.
			if sshCfg := resolveSSH(tunnel, stack.TunnelDefaults); sshCfg.Enabled {
				tunnelEnv = append(tunnelEnv,
					corev1.EnvVar{Name: "SSH_ENABLED", Value: "true"},
					corev1.EnvVar{Name: "SSH_COMPRESSION", Value: strconv.FormatBool(*sshCfg.Compression)},
					corev1.EnvVar{Name: "SSH_USER", Value: sshCfg.User},
					corev1.EnvVar{Name: "SSH_LOCAL_PORT", Value: fmt.Sprintf("%d", sshCfg.LocalSSHPort)},
				)
			}

			tunnelMounts := []corev1.VolumeMount{
				{Name: tunnelStateVolumeName, MountPath: tunnelStateMountPath},
			}
			var tunnelEnvFrom []corev1.EnvFromSource
			if useRefresh {
				tunnelEnv = append(tunnelEnv, corev1.EnvVar{Name: "AWS_CREDS_DIR", Value: credsMountPath})
				tunnelMounts = append(tunnelMounts, corev1.VolumeMount{Name: credsVolumeName, MountPath: credsMountPath, ReadOnly: true})
			} else {
				tunnelEnv = append(tunnelEnv,
					corev1.EnvVar{Name: "AWS_PROFILE", Value: profile},
					corev1.EnvVar{Name: "AWS_SDK_LOAD_CONFIG", Value: "1"},
					corev1.EnvVar{Name: "AWS_CONFIG_FILE", Value: awsConfigFilePath},
				)
				tunnelMounts = append(tunnelMounts, corev1.VolumeMount{Name: awsConfigVolumeName, MountPath: awsConfigMountPath, ReadOnly: true})
				tunnelEnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: profileSecretName}}}}
			}

			dep.Spec.Template.Spec.Containers[0] = corev1.Container{
				Name:            "tunnel",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env:             tunnelEnv,
				EnvFrom:         tunnelEnvFrom,
				VolumeMounts:    tunnelMounts,
				Command:         []string{"/usr/local/bin/tunnel-runner"},
				Resources:       resources,
				// Gate Service endpoint membership on the SSM tunnel actually being up (state file
				// written), so a surged replacement pod only takes traffic once it serves and the
				// old pod isn't retired until then — new connections stay gap-free across a roll.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "[ -s /tmp/tunnel-state/state ]"}},
					},
					InitialDelaySeconds: 5,
					PeriodSeconds:       10,
					FailureThreshold:    3,
				},
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
				Name:         tunnelStateVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
			if useRefresh {
				dep.Spec.Template.Spec.Volumes = upsertVolume(dep.Spec.Template.Spec.Volumes, corev1.Volume{
					Name:         credsVolumeName,
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: profileSecretName}},
				})
			} else {
				dep.Spec.Template.Spec.Volumes = upsertVolume(dep.Spec.Template.Spec.Volumes, corev1.Volume{
					Name: awsConfigVolumeName,
					VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: awsConfigMapName},
						Items:                []corev1.KeyToPath{{Key: awsConfigDataKey, Path: "config"}},
					}},
				})
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
			svc.Labels = map[string]string{labelStack: stackName}
			if r.ArgoAppName != "" {
				svc.Labels[labelInstance] = r.ArgoAppName
				if svc.Annotations == nil {
					svc.Annotations = map[string]string{}
				}
				svc.Annotations[annotTracking] = argoTrackingID(r.ArgoAppName, "", "ConfigMap", r.Namespace, r.ConfigMapName)
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
			ing.SetAPIVersion(traefikAPI)
			ing.SetKind("IngressRouteTCP")
			ing.SetNamespace(cfg.Namespace)
			ing.SetName(tunnelName)
			_, _ = controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
				ingLabels := map[string]string{labelStack: stackName}
				if r.ArgoAppName != "" {
					ingLabels[labelInstance] = r.ArgoAppName
					ing.SetAnnotations(map[string]string{annotTracking: argoTrackingID(r.ArgoAppName, "", "ConfigMap", r.Namespace, r.ConfigMapName)})
				}
				ing.SetLabels(ingLabels)
				ing.Object["spec"] = map[string]any{
					"entryPoints": []any{"websecure"},
					"routes":      []any{map[string]any{"match": fmt.Sprintf("HostSNI(`%s`)", tunnel.Host), "services": []any{map[string]any{"name": tunnelName, "namespace": cfg.Namespace, "port": svcPort}}}},
					"tls":         tcpTLSSpec(tunnel.TLS),
				}
				return nil
			})
			stale := &unstructured.Unstructured{}
			stale.SetAPIVersion(traefikAPI)
			stale.SetKind("IngressRoute")
			stale.SetNamespace(cfg.Namespace)
			stale.SetName(tunnelName)
			_ = client.IgnoreNotFound(r.Client.Delete(ctx, stale))
		} else {
			ing := &unstructured.Unstructured{}
			ing.SetAPIVersion(traefikAPI)
			ing.SetKind("IngressRoute")
			ing.SetNamespace(cfg.Namespace)
			ing.SetName(tunnelName)
			_, _ = controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
				ingLabels := map[string]string{labelStack: stackName}
				if r.ArgoAppName != "" {
					ingLabels[labelInstance] = r.ArgoAppName
					ing.SetAnnotations(map[string]string{annotTracking: argoTrackingID(r.ArgoAppName, "", "ConfigMap", r.Namespace, r.ConfigMapName)})
				}
				ing.SetLabels(ingLabels)
				ing.Object["spec"] = map[string]any{
					"entryPoints": []any{"websecure"},
					"routes":      []any{map[string]any{"kind": "Rule", "match": fmt.Sprintf("Host(`%s`)", tunnel.Host), "services": []any{map[string]any{"kind": "Service", "name": tunnelName, "namespace": cfg.Namespace, "port": svcPort, "scheme": "http"}}}},
					"tls":         map[string]any{},
				}
				return nil
			})
			stale := &unstructured.Unstructured{}
			stale.SetAPIVersion(traefikAPI)
			stale.SetKind("IngressRouteTCP")
			stale.SetNamespace(cfg.Namespace)
			stale.SetName(tunnelName)
			_ = client.IgnoreNotFound(r.Client.Delete(ctx, stale))
		}
	}

	return r.pruneStaleResources(ctx, cfg)
}

func (r *SingleStackRunner) pruneStaleResources(ctx context.Context, cfg StackConfig) error {
	stackName := cfg.Name
	stackLabel := client.MatchingLabels{labelStack: stackName}
	ns := client.InNamespace(cfg.Namespace)

	desiredTunnels := make(map[string]struct{}, len(cfg.Spec.Tunnels))
	for _, t := range cfg.Spec.Tunnels {
		desiredTunnels[fmt.Sprintf("%s-%s", stackName, t.Name)] = struct{}{}
	}

	desiredSecrets := make(map[string]struct{})
	for _, p := range cfg.DefinedAWSProfiles() {
		desiredSecrets[credsSecretName(stackName, p)] = struct{}{}
	}
	// The captured SSO token Secret is operator-managed runtime state, not a creds Secret —
	// keep it out of the pruner's sights even if it ever gets the stack label.
	desiredSecrets[tokenSecretName(stackName)] = struct{}{}

	depList := &appsv1.DeploymentList{}
	if err := r.Client.List(ctx, depList, ns, stackLabel); err != nil {
		return fmt.Errorf("list deployments for pruning: %w", err)
	}
	for i := range depList.Items {
		if _, ok := desiredTunnels[depList.Items[i].Name]; !ok {
			if err := client.IgnoreNotFound(r.Client.Delete(ctx, &depList.Items[i])); err != nil {
				return fmt.Errorf("delete stale deployment %s: %w", depList.Items[i].Name, err)
			}
		}
	}

	svcList := &corev1.ServiceList{}
	if err := r.Client.List(ctx, svcList, ns, stackLabel); err != nil {
		return fmt.Errorf("list services for pruning: %w", err)
	}
	for i := range svcList.Items {
		if _, ok := desiredTunnels[svcList.Items[i].Name]; !ok {
			if err := client.IgnoreNotFound(r.Client.Delete(ctx, &svcList.Items[i])); err != nil {
				return fmt.Errorf("delete stale service %s: %w", svcList.Items[i].Name, err)
			}
		}
	}

	secretList := &corev1.SecretList{}
	if err := r.Client.List(ctx, secretList, ns, stackLabel); err != nil {
		return fmt.Errorf("list secrets for pruning: %w", err)
	}
	for i := range secretList.Items {
		if _, ok := desiredSecrets[secretList.Items[i].Name]; !ok {
			if err := client.IgnoreNotFound(r.Client.Delete(ctx, &secretList.Items[i])); err != nil {
				return fmt.Errorf("delete stale secret %s: %w", secretList.Items[i].Name, err)
			}
		}
	}

	for _, kind := range []string{"IngressRoute", "IngressRouteTCP"} {
		ingList := &unstructured.UnstructuredList{}
		ingList.SetAPIVersion(traefikAPI)
		ingList.SetKind(kind + "List")
		if err := r.Client.List(ctx, ingList, ns, stackLabel); err != nil {
			continue // CRD may not be installed; skip gracefully
		}
		for i := range ingList.Items {
			if _, ok := desiredTunnels[ingList.Items[i].GetName()]; !ok {
				if err := client.IgnoreNotFound(r.Client.Delete(ctx, &ingList.Items[i])); err != nil {
					return fmt.Errorf("delete stale %s %s: %w", kind, ingList.Items[i].GetName(), err)
				}
			}
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
