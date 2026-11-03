package announce

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseExpandsEnvAndDefaults(t *testing.T) {
	recs, err := Parse([]byte(`
records:
  - instance: Homelab Storage
    host: smb-server
    ip: ${SMB_IP}
    port: 445
    model: Xserve
`), env(map[string]string{"SMB_IP": "192.0.2.10"}))
	if err != nil {
		t.Fatal(err)
	}
	r := recs[0]
	if r.IP != "192.0.2.10" || r.Type != "_smb._tcp" {
		t.Fatalf("got %+v", r)
	}
	svcs := r.services()
	if len(svcs) != 2 || svcs[1].typ != "_device-info._tcp" || svcs[1].txt[0] != "model=Xserve" {
		t.Fatalf("services: %+v", svcs)
	}
}

func TestParseRejects(t *testing.T) {
	base := "records:\n  - instance: X\n    host: h\n    ip: 192.0.2.1\n    port: 445\n"
	for name, tc := range map[string]struct{ doc, want string }{
		"unset env":     {strings.Replace(base, "192.0.2.1", "${MISSING}", 1), "unset env var"},
		"dotted host":   {strings.Replace(base, "host: h", "host: a.local", 1), "single DNS label"},
		"ipv6":          {strings.Replace(base, "192.0.2.1", "\"2001:db8::1\"", 1), "not an IPv4"},
		"port zero":     {strings.Replace(base, "445", "0", 1), "out of range"},
		"bad type":      {base + "    type: smb\n", "must look like"},
		"unknown field": {base + "    colour: red\n", "colour"},
		"empty":         {"records: []\n", "no records"},
	} {
		if _, err := Parse([]byte(tc.doc), env(nil)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, tc.want)
		}
	}
}
