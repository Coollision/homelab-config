package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	AWSConfigMapName   string `json:"awsConfigMapName,omitempty"`
	ScriptConfigMapName string `json:"scriptConfigMapName,omitempty"`
	AuthConfigMapName  string `json:"authServerConfigMapName,omitempty"`
}

type LivenessProbeSpec struct {
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int32 `json:"periodSeconds,omitempty"`
	FailureThreshold    int32 `json:"failureThreshold,omitempty"`
}

type TunnelDefaultSpec struct {
	Image         string                      `json:"image,omitempty"`
	ProxyImage    string                      `json:"proxyImage,omitempty"`
	ServicePort   int32                       `json:"servicePort,omitempty"`
	Resources     corev1.ResourceRequirements `json:"resources,omitempty"`
	ProxyResources corev1.ResourceRequirements `json:"proxyResources,omitempty"`
	LivenessProbe LivenessProbeSpec           `json:"livenessProbe,omitempty"`
}

type RDSSpec struct {
	InstancePrefix string `json:"instancePrefix,omitempty"`
	ClusterPrefix  string `json:"clusterPrefix,omitempty"`
}

type TLSSpec struct {
	Passthrough bool `json:"passthrough,omitempty"`
}

type TunnelSpec struct {
	Name       string `json:"name"`
	Host       string `json:"host"`
	BastionName string `json:"bastionName"`
	RemoteHost string `json:"remoteHost,omitempty"`
	RemotePort string `json:"remotePort"`
	LocalPort  int32  `json:"localPort"`
	AWSProfile string `json:"awsProfile,omitempty"`
	AWSRegion  string `json:"awsRegion,omitempty"`
	IngressMode string `json:"ingressMode,omitempty"`
	Image      string `json:"image,omitempty"`
	ProxyImage string `json:"proxyImage,omitempty"`
	ServicePort int32 `json:"servicePort,omitempty"`
	RDS        RDSSpec `json:"rds,omitempty"`
	TLS        TLSSpec `json:"tls,omitempty"`
	Resources      corev1.ResourceRequirements `json:"resources,omitempty"`
	ProxyResources corev1.ResourceRequirements `json:"proxyResources,omitempty"`
}

type AWSTunnelStackSpec struct {
	AWS            AWSSpec           `json:"aws"`
	Auth           AuthSpec          `json:"auth,omitempty"`
	NodeAffinity   NodeAffinitySpec  `json:"nodeAffinity,omitempty"`
	Shared         SharedNamesSpec   `json:"shared,omitempty"`
	TunnelDefaults TunnelDefaultSpec `json:"tunnelDefaults,omitempty"`
	Tunnels        []TunnelSpec      `json:"tunnels"`
}

type AWSTunnelStackStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	ManagedTunnels     int32 `json:"managedTunnels,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type AWSTunnelStack struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AWSTunnelStackSpec   `json:"spec,omitempty"`
	Status AWSTunnelStackStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type AWSTunnelStackList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AWSTunnelStack `json:"items"`
}
