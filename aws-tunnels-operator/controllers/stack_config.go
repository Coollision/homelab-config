package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	defaultStackConfigMap = "aws-tunnels-operator-stack"
	stackSpecJSONKey      = "spec.json"
	stackNameKey          = "stackName"
	awsConfigDataKey      = "config"
)

type StackConfig struct {
	Namespace string
	Name      string
	Spec      AWSTunnelStackSpec
}

func LoadStackConfig(ctx context.Context, c client.Client, namespace, configMapName string) (StackConfig, error) {
	if strings.TrimSpace(namespace) == "" {
		return StackConfig{}, fmt.Errorf("stack namespace is required")
	}
	if strings.TrimSpace(configMapName) == "" {
		configMapName = defaultStackConfigMap
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: configMapName}, cm); err != nil {
		return StackConfig{}, fmt.Errorf("get stack configmap %s/%s: %w", namespace, configMapName, err)
	}

	stackName := strings.TrimSpace(cm.Data[stackNameKey])
	if stackName == "" {
		return StackConfig{}, fmt.Errorf("configmap %s/%s missing %q", namespace, configMapName, stackNameKey)
	}

	raw := strings.TrimSpace(cm.Data[stackSpecJSONKey])
	if raw == "" {
		return StackConfig{}, fmt.Errorf("configmap %s/%s missing %q", namespace, configMapName, stackSpecJSONKey)
	}

	var spec AWSTunnelStackSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return StackConfig{}, fmt.Errorf("parse %q in %s/%s: %w", stackSpecJSONKey, namespace, configMapName, err)
	}

	if len(spec.Tunnels) == 0 {
		return StackConfig{}, fmt.Errorf("stack spec has no tunnels")
	}

	return StackConfig{Namespace: namespace, Name: stackName, Spec: spec}, nil
}

func (c StackConfig) DefinedAWSProfiles() []string {
	profiles := map[string]struct{}{}
	if p := strings.TrimSpace(c.Spec.AWS.Profile); p != "" {
		profiles[p] = struct{}{}
	}
	for _, p := range c.Spec.AWS.ExtraProfile {
		if name := strings.TrimSpace(p.Name); name != "" {
			profiles[name] = struct{}{}
		}
	}
	return sortedKeys(profiles)
}

func (c StackConfig) ReferencedAWSProfiles() []string {
	profiles := map[string]struct{}{}
	for _, p := range c.DefinedAWSProfiles() {
		profiles[p] = struct{}{}
	}
	for _, tunnel := range c.Spec.Tunnels {
		if p := strings.TrimSpace(tunnel.AWSProfile); p != "" {
			profiles[p] = struct{}{}
		}
	}
	return sortedKeys(profiles)
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (c StackConfig) AWSConfigMapName() string {
	if name := strings.TrimSpace(c.Spec.Shared.AWSConfigMapName); name != "" {
		return name
	}
	return fmt.Sprintf("%s-aws-config", c.Name)
}

func (c StackConfig) RenderAWSConfig() (string, []string, error) {
	type profileDef struct {
		Name      string
		Region    string
		StartURL  string
		AccountID string
		RoleName  string
	}

	profiles := make([]profileDef, 0, 1+len(c.Spec.AWS.ExtraProfile))
	if name := strings.TrimSpace(c.Spec.AWS.Profile); name != "" {
		profiles = append(profiles, profileDef{
			Name:      name,
			Region:    strings.TrimSpace(c.Spec.AWS.Region),
			StartURL:  strings.TrimSpace(c.Spec.AWS.SSOStartURL),
			AccountID: strings.TrimSpace(c.Spec.AWS.AccountID),
			RoleName:  strings.TrimSpace(c.Spec.AWS.RoleName),
		})
	}
	for _, p := range c.Spec.AWS.ExtraProfile {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		profiles = append(profiles, profileDef{
			Name:      name,
			Region:    strings.TrimSpace(p.Region),
			StartURL:  strings.TrimSpace(p.SSOStartURL),
			AccountID: strings.TrimSpace(p.AccountID),
			RoleName:  strings.TrimSpace(p.RoleName),
		})
	}

	if len(profiles) == 0 {
		return "", nil, fmt.Errorf("stack config has no aws profiles. Set stack.aws.profile and optional stack.aws.extraProfiles")
	}

	var b strings.Builder
	available := make([]string, 0, len(profiles))
	for _, p := range profiles {
		if p.StartURL == "" || p.AccountID == "" || p.RoleName == "" {
			return "", nil, fmt.Errorf("aws profile %q is missing required sso fields (ssoStartUrl, accountId, roleName)", p.Name)
		}
		region := p.Region
		if region == "" {
			region = "eu-west-1"
		}
		available = append(available, p.Name)
		b.WriteString("[profile ")
		b.WriteString(p.Name)
		b.WriteString("]\n")
		b.WriteString("region = ")
		b.WriteString(region)
		b.WriteString("\n")
		b.WriteString("sso_start_url = ")
		b.WriteString(p.StartURL)
		b.WriteString("\n")
		b.WriteString("sso_region = ")
		b.WriteString(region)
		b.WriteString("\n")
		b.WriteString("sso_account_id = ")
		b.WriteString(p.AccountID)
		b.WriteString("\n")
		b.WriteString("sso_role_name = ")
		b.WriteString(p.RoleName)
		b.WriteString("\n")
		b.WriteString("output = json\n\n")
	}

	return b.String(), available, nil
}

func SyncAWSConfigMap(ctx context.Context, c client.Client, cfg StackConfig) (string, string, []string, error) {
	configMapName := cfg.AWSConfigMapName()
	configText, profiles, err := cfg.RenderAWSConfig()
	if err != nil {
		return "", "", nil, err
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: cfg.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels["app.kubernetes.io/managed-by"] = "aws-tunnels-operator"
		cm.Labels["proxies.homelab.io/stack"] = cfg.Name
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[awsConfigDataKey] = configText
		return nil
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("sync aws config configmap %s/%s: %w", cfg.Namespace, configMapName, err)
	}

	return configMapName, configText, profiles, nil
}
