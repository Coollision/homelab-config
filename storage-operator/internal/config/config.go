// Package config parses the smb-operator config.yaml (ConfigMap smb-operator-config).
//
// Keys and defaults mirror the Python operator (smb_operator_v2.py, Config.load_from_configmap)
// exactly, so the existing ConfigMap works unchanged. Keys that the Python code read but never
// used (operator.logLevel, operator.healthPort, resources.operator, smb.security) are accepted and
// ignored.
package config

import (
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// Resources is the requests/limits block for the smb-server container. All four values are
// required at render time (the Python code indexed them directly).
type Resources map[string]map[string]string

// Config is the parsed operator configuration plus the secret-sourced values from the env.
type Config struct {
	Namespace         string
	SMBLoadBalancerIP string
	SMBUsername       string
	SMBPassword       string

	ReconcileInterval int
	UseWatchAPI       bool
	LogLevel          string
	HealthPort        int

	SMBDeploymentName      string
	SMBServiceName         string
	SMBImage               string
	SMBWorkgroup           string
	SMBServerString        string
	NFSMountOptions        string
	MountReconcileInterval int
	InitialMountWait       int

	LonghornNamespace string
	LonghornGroup     string
	LonghornVersion   string
	LonghornPlural    string

	MaxRetries           int
	InitialDelay         int
	BackoffMultiplier    int
	RetryableStatusCodes []int

	SMBResources      Resources
	OperatorResources Resources

	// Deprecated holds the dotted names of accepted-but-ignored keys that were present,
	// so the caller can log a deprecation line.
	Deprecated []string
}

// Defaults (identical to the Python dataclass / .get() fallbacks).
const (
	DefaultNFSMountOptions = "vers=4.2,noresvport,soft,timeo=50,retrans=2"
)

// DefaultSMBResources mirrors Config.__post_init__.
func DefaultSMBResources() Resources {
	return Resources{
		"requests": {"cpu": "100m", "memory": "128Mi"},
		"limits":   {"cpu": "500m", "memory": "512Mi"},
	}
}

// Env is the subset of environment variables the config needs.
type Env struct {
	LoadBalancerIP string // SMB_LOADBALANCER_IP
	Username       string // SMB_USERNAME
	Password       string // SMB_PASSWORD
}

// ErrEmpty is returned for a missing or empty config.yaml.
var ErrEmpty = errors.New("config.yaml is empty")

// Parse parses config.yaml text and merges the secret env values. Validation matches Python:
// operator.namespace must be truthy, and all three env values must be non-empty.
func Parse(text string, env Env) (*Config, error) {
	if text == "" {
		return nil, fmt.Errorf("%w; required keys: operator.namespace", ErrEmpty)
	}
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("failed to parse config.yaml: %w", err)
	}
	op := section(raw, "operator")
	smb := section(raw, "smb")
	lh := section(raw, "longhorn")
	retry := section(raw, "retry")
	res := section(raw, "resources")

	ns, _ := op["namespace"].(string)
	if !truthy(op["namespace"]) {
		return nil, errors.New("missing required configuration keys: operator.namespace")
	}
	if env.LoadBalancerIP == "" {
		return nil, errors.New("SMB_LOADBALANCER_IP env var is required (from smb-credentials Secret)")
	}
	if env.Username == "" || env.Password == "" {
		return nil, errors.New("SMB_USERNAME and SMB_PASSWORD env vars are required (from smb-credentials Secret)")
	}

	c := &Config{
		Namespace:         ns,
		SMBLoadBalancerIP: env.LoadBalancerIP,
		SMBUsername:       env.Username,
		SMBPassword:       env.Password,

		ReconcileInterval: getInt(op, "reconcileInterval", 30),
		UseWatchAPI:       getBool(op, "useWatchAPI", true),
		LogLevel:          getStr(op, "logLevel", "INFO"),
		HealthPort:        getInt(op, "healthPort", 8080),

		SMBDeploymentName:      getStr(smb, "deploymentName", "smb-server"),
		SMBServiceName:         getStr(smb, "serviceName", "smb-server"),
		SMBImage:               getStr(smb, "image", "dperson/samba:latest"),
		SMBWorkgroup:           getStr(smb, "workgroup", "WORKGROUP"),
		SMBServerString:        getStr(smb, "serverString", "Homelab Storage"),
		NFSMountOptions:        getStr(smb, "nfsMountOptions", DefaultNFSMountOptions),
		MountReconcileInterval: getInt(smb, "mountReconcileInterval", 15),
		InitialMountWait:       getInt(smb, "initialMountWait", 30),

		LonghornNamespace: getStr(lh, "namespace", "storage"),
		LonghornGroup:     getStr(lh, "group", "longhorn.io"),
		LonghornVersion:   getStr(lh, "version", "v1beta2"),
		LonghornPlural:    getStr(lh, "plural", "volumes"),

		MaxRetries:           getInt(retry, "maxRetries", 3),
		InitialDelay:         getInt(retry, "initialDelay", 1),
		BackoffMultiplier:    getInt(retry, "backoffMultiplier", 2),
		RetryableStatusCodes: getIntList(retry, "retryableStatusCodes", []int{409, 429, 500, 503, 504}),

		SMBResources:      getResources(res, "smbServer", DefaultSMBResources()),
		OperatorResources: getResources(res, "operator", nil),
	}
	if _, ok := smb["security"]; ok {
		c.Deprecated = append(c.Deprecated, "smb.security")
	}
	if _, ok := res["operator"]; ok {
		c.Deprecated = append(c.Deprecated, "resources.operator")
	}
	for _, k := range []string{"logLevel", "healthPort"} {
		if _, ok := op[k]; ok {
			c.Deprecated = append(c.Deprecated, "operator."+k)
		}
	}
	return c, nil
}

func section(raw map[string]any, key string) map[string]any {
	if m, ok := raw[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case float64:
		return t != 0
	}
	return true
}

func getStr(m map[string]any, k, def string) string {
	v, ok := m[k]
	if !ok {
		return def
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprint(t)
	case bool:
		if t {
			return "True"
		}
		return "False"
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func getInt(m map[string]any, k string, def int) int {
	switch t := m[k].(type) {
	case float64:
		return int(t)
	case string:
		var n int
		if _, err := fmt.Sscan(t, &n); err == nil {
			return n
		}
	}
	return def
}

func getBool(m map[string]any, k string, def bool) bool {
	switch t := m[k].(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true") || strings.EqualFold(t, "yes")
	}
	return def
}

func getIntList(m map[string]any, k string, def []int) []int {
	l, ok := m[k].([]any)
	if !ok {
		return def
	}
	out := make([]int, 0, len(l))
	for _, v := range l {
		if f, ok := v.(float64); ok {
			out = append(out, int(f))
		}
	}
	return out
}

func getResources(m map[string]any, k string, def Resources) Resources {
	sec, ok := m[k].(map[string]any)
	if !ok {
		return def
	}
	out := Resources{}
	for kind, v := range sec {
		vals, ok := v.(map[string]any)
		if !ok {
			continue
		}
		out[kind] = map[string]string{}
		for rk, rv := range vals {
			out[kind][rk] = getStr(vals, rk, fmt.Sprint(rv))
		}
	}
	return out
}

// Validate is called by the renderer: the Python code indexed requests/limits cpu/memory directly.
func (r Resources) Validate() error {
	for _, kind := range []string{"requests", "limits"} {
		for _, k := range []string{"cpu", "memory"} {
			if r[kind][k] == "" {
				return fmt.Errorf("resources.smbServer.%s.%s is required", kind, k)
			}
		}
	}
	return nil
}
