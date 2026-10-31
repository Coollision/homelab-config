package config

import (
	"reflect"
	"strings"
	"testing"
)

var env = Env{LoadBalancerIP: "192.0.2.10", Username: "u", Password: "p"}

func TestDefaults(t *testing.T) {
	c, err := Parse("operator:\n  namespace: storage\n", env)
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Namespace: "storage", SMBLoadBalancerIP: "192.0.2.10", SMBUsername: "u", SMBPassword: "p",
		ReconcileInterval: 30, UseWatchAPI: true, LogLevel: "INFO", HealthPort: 8080,
		SMBDeploymentName: "smb-server", SMBServiceName: "smb-server", SMBImage: "dperson/samba:latest",
		SMBWorkgroup: "WORKGROUP", SMBServerString: "Homelab Storage", NFSMountOptions: DefaultNFSMountOptions,
		MountReconcileInterval: 15, InitialMountWait: 30,
		LonghornNamespace: "storage", LonghornGroup: "longhorn.io", LonghornVersion: "v1beta2", LonghornPlural: "volumes",
		MaxRetries: 3, InitialDelay: 1, BackoffMultiplier: 2, RetryableStatusCodes: []int{409, 429, 500, 503, 504},
		SMBResources: DefaultSMBResources(),
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v\nwant %+v", c, want)
	}
}

func TestOverridesAndDeprecated(t *testing.T) {
	c, err := Parse(`operator:
  namespace: files
  reconcileInterval: 10
  useWatchAPI: false
  logLevel: DEBUG
smb:
  image: x/y:1
  security: {ntlmAuth: yes}
longhorn: {namespace: longhorn-system}
retry: {maxRetries: 5, retryableStatusCodes: [500]}
resources:
  smbServer: {requests: {cpu: 1, memory: 1Gi}, limits: {cpu: "2", memory: 2Gi}}
  operator: {}
`, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReconcileInterval != 10 || c.UseWatchAPI || c.SMBImage != "x/y:1" || c.LonghornNamespace != "longhorn-system" ||
		c.MaxRetries != 5 || !reflect.DeepEqual(c.RetryableStatusCodes, []int{500}) {
		t.Fatalf("overrides not applied: %+v", c)
	}
	if c.SMBResources["requests"]["cpu"] != "1" || c.SMBResources["limits"]["memory"] != "2Gi" {
		t.Fatalf("resources: %+v", c.SMBResources)
	}
	got := strings.Join(c.Deprecated, ",")
	for _, k := range []string{"smb.security", "resources.operator", "operator.logLevel"} {
		if !strings.Contains(got, k) {
			t.Errorf("deprecated %q missing from %q", k, got)
		}
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]struct {
		text string
		env  Env
		want string
	}{
		"empty":       {"", env, "empty"},
		"no ns":       {"operator: {}\n", env, "operator.namespace"},
		"empty ns":    {"operator: {namespace: \"\"}\n", env, "operator.namespace"},
		"no ip":       {"operator: {namespace: s}\n", Env{Username: "u", Password: "p"}, "SMB_LOADBALANCER_IP"},
		"no password": {"operator: {namespace: s}\n", Env{LoadBalancerIP: "x", Username: "u"}, "SMB_PASSWORD"},
		"bad yaml":    {"operator: [\n", env, "parse"},
	}
	for name, c := range cases {
		_, err := Parse(c.text, c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
	}
}

func TestResourcesValidate(t *testing.T) {
	if err := DefaultSMBResources().Validate(); err != nil {
		t.Fatal(err)
	}
	r := Resources{"requests": {"cpu": "1", "memory": "1Gi"}, "limits": {"memory": "1Gi"}}
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "limits.cpu") {
		t.Fatalf("err = %v", err)
	}
}
