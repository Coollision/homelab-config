package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	proxyv1alpha1 "homelab/aws-tunnels-operator/api/v1alpha1"
)

const (
	defaultStackName      = "aws-tunnels"
	defaultStackConfigMap = "aws-tunnels-operator-stack"
	stackSpecJSONKey      = "spec.json"
	stackNameKey          = "stackName"
)

type StackConfig struct {
	Namespace string
	Name      string
	Spec      proxyv1alpha1.AWSTunnelStackSpec
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
		stackName = defaultStackName
	}

	raw := strings.TrimSpace(cm.Data[stackSpecJSONKey])
	if raw == "" {
		return StackConfig{}, fmt.Errorf("configmap %s/%s missing %q", namespace, configMapName, stackSpecJSONKey)
	}

	var spec proxyv1alpha1.AWSTunnelStackSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return StackConfig{}, fmt.Errorf("parse %q in %s/%s: %w", stackSpecJSONKey, namespace, configMapName, err)
	}

	if len(spec.Tunnels) == 0 {
		return StackConfig{}, fmt.Errorf("stack spec has no tunnels")
	}

	return StackConfig{Namespace: namespace, Name: stackName, Spec: spec}, nil
}
