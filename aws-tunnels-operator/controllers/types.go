package controllers

import corev1 "k8s.io/api/core/v1"

type AWSProfileSpec struct {
	Name        string `json:"name"`
	Region      string `json:"region"`
	SSOStartURL string `json:"ssoStartUrl"`
	AccountID   string `json:"accountId"`
	RoleName    string `json:"roleName"`
}

type AWSSpec struct {
	Profile      string           `json:"profile"`
	Region       string           `json:"region"`
	SSOStartURL  string           `json:"ssoStartUrl"`
	AccountID    string           `json:"accountId"`
	RoleName     string           `json:"roleName"`
	ExtraProfile []AWSProfileSpec `json:"extraProfiles,omitempty"`

	// UseRefresh switches the rendered AWS config to the modern sso-session format with registration
	// scopes (sso:account:access), so `aws sso login` mints a refresh token and the operator can silently auto-refresh
	// STS creds for the whole SSO session. When false (default), the legacy inline format is used
	// and there is no auto-refresh (a manual login is needed whenever creds expire). The login flow
	// also adds --use-device-code when this is true, so the device-code (remote-clickable) flow is
	// used instead of the sso-session default localhost-redirect flow, which can't work in-cluster.
	UseRefresh bool `json:"useRefresh,omitempty"`
}

type AuthSpec struct {
	Enabled       bool                        `json:"enabled,omitempty"`
	Name          string                      `json:"name,omitempty"`
	Host          string                      `json:"host,omitempty"`
	Image         string                      `json:"image,omitempty"`
	Port          int32                       `json:"port,omitempty"`
	Resources     corev1.ResourceRequirements `json:"resources,omitempty"`
	InitResources corev1.ResourceRequirements `json:"initResources,omitempty"`
}

type NodeAffinitySpec struct {
	ExcludedType string `json:"excludedType,omitempty"`
}

type SharedNamesSpec struct {
	AWSConfigMapName    string `json:"awsConfigMapName,omitempty"`
	ScriptConfigMapName string `json:"scriptConfigMapName,omitempty"`
	AuthConfigMapName   string `json:"authServerConfigMapName,omitempty"`
}

type LivenessProbeSpec struct {
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int32 `json:"periodSeconds,omitempty"`
	FailureThreshold    int32 `json:"failureThreshold,omitempty"`
}

type TunnelDefaultSpec struct {
	Image          string                      `json:"image,omitempty"`
	ProxyImage     string                      `json:"proxyImage,omitempty"`
	ServicePort    int32                       `json:"servicePort,omitempty"`
	Resources      corev1.ResourceRequirements `json:"resources,omitempty"`
	ProxyResources corev1.ResourceRequirements `json:"proxyResources,omitempty"`
	LivenessProbe  LivenessProbeSpec           `json:"livenessProbe,omitempty"`

	// Replicas is the default number of pods (and therefore independent SSM sessions) per tunnel.
	// Defaults to 1. See TunnelSpec.Replicas for when raising it actually helps.
	Replicas *int32 `json:"replicas,omitempty"`

	// SSH enables the compressed SSH transport by default for all tunnels. See SSHSpec.
	SSH SSHSpec `json:"ssh,omitempty"`
}

// SSHSpec configures the compressed SSH transport.
//
// A single SSM session is rate-limited by AWS to ~0.7 MB/s, and that cap applies to the bytes on
// the wire. Rather than forwarding the target port directly over SSM, this mode forwards the
// bastion's SSH port and runs `ssh -C` through it, so payloads are gzipped ON THE BASTION before
// they reach the bottleneck. Measured 3.8x on a Postgres bulk read.
//
// Only worthwhile for compressible traffic. Already-compressed or encrypted payloads get slightly
// SLOWER (measured 0.61 -> 0.56 MB/s), so leave this off for TLS-passthrough tunnels: their bytes
// are ciphertext and cannot compress. A DB tunnel benefits only when TLS terminates at the ingress
// rather than passing through to the database.
type SSHSpec struct {
	// Enabled turns on the SSH transport for this tunnel.
	Enabled bool `json:"enabled,omitempty"`

	// Compression sets `ssh -C`. Defaults to true when Enabled; set false to measure the SSH
	// layer on its own (it costs nothing — the entire gain is compression).
	Compression *bool `json:"compression,omitempty"`

	// User is the bastion OS user for the SSH login. Defaults to ec2-user.
	User string `json:"user,omitempty"`

	// LocalSSHPort is the loopback port the bastion's :22 is forwarded to inside the pod. It must
	// not collide with the tunnel's own localPort. Defaults to 2222.
	LocalSSHPort int32 `json:"localSshPort,omitempty"`
}

type RDSSpec struct {
	InstancePrefix string `json:"instancePrefix,omitempty"`
	ClusterPrefix  string `json:"clusterPrefix,omitempty"`
}

type TLSSpec struct {
	// Passthrough forwards the client's TLS session untouched to the far end.
	//
	// Routing is by SNI, so the client MUST speak TLS for the route to match at all. That also
	// means every byte crossing the tunnel is ciphertext, which cannot be compressed — so
	// passthrough and the compressed SSH transport are mutually exclusive. Terminate TLS here
	// (see SecretName) if you want compression on a database tunnel.
	Passthrough bool `json:"passthrough,omitempty"`

	// SecretName terminates TLS at the ingress using this Kubernetes TLS secret instead of
	// passing it through. Clients still connect with TLS (so SNI routing and `sslmode=require`
	// keep working unchanged), but the payload becomes plaintext from the ingress inwards and can
	// therefore be compressed.
	//
	// This changes the security posture: TLS is no longer end-to-end from client to target. The
	// path becomes client->ingress (TLS), ingress->tunnel pod (in-cluster), pod->bastion (SSH),
	// bastion->target. Every hop is protected, but not by one continuous TLS session. Use it
	// deliberately.
	SecretName string `json:"secretName,omitempty"`

	// CertResolver is an alternative to SecretName: let Traefik obtain the certificate via the
	// named ACME resolver.
	CertResolver string `json:"certResolver,omitempty"`
}

type TunnelSpec struct {
	Name           string                      `json:"name"`
	Host           string                      `json:"host"`
	BastionName    string                      `json:"bastionName"`
	RemoteHost     string                      `json:"remoteHost,omitempty"`
	RemotePort     string                      `json:"remotePort"`
	LocalPort      int32                       `json:"localPort"`
	AWSProfile     string                      `json:"awsProfile,omitempty"`
	AWSRegion      string                      `json:"awsRegion,omitempty"`
	IngressMode    string                      `json:"ingressMode,omitempty"`
	Image          string                      `json:"image,omitempty"`
	ProxyImage     string                      `json:"proxyImage,omitempty"`
	ServicePort    int32                       `json:"servicePort,omitempty"`
	RDS            RDSSpec                     `json:"rds,omitempty"`
	TLS            TLSSpec                     `json:"tls,omitempty"`
	Resources      corev1.ResourceRequirements `json:"resources,omitempty"`
	ProxyResources corev1.ResourceRequirements `json:"proxyResources,omitempty"`

	// Replicas overrides TunnelDefaults.Replicas for this tunnel.
	//
	// Each replica holds its own SSM session. Because all connections through a single tunnel are
	// smux-multiplexed onto one datachannel, and AWS rate-limits per session, extra replicas raise
	// aggregate throughput ONLY for workloads that open several connections — the Service spreads
	// those across pods. A single stream is unaffected. Capped at maxTunnelReplicas.
	Replicas *int32 `json:"replicas,omitempty"`

	// SSH overrides TunnelDefaults.SSH for this tunnel.
	SSH SSHSpec `json:"ssh,omitempty"`
}

type AWSTunnelStackSpec struct {
	AWS            AWSSpec           `json:"aws"`
	Auth           AuthSpec          `json:"auth,omitempty"`
	NodeAffinity   NodeAffinitySpec  `json:"nodeAffinity,omitempty"`
	Shared         SharedNamesSpec   `json:"shared,omitempty"`
	TunnelDefaults TunnelDefaultSpec `json:"tunnelDefaults,omitempty"`
	Tunnels        []TunnelSpec      `json:"tunnels"`
}
