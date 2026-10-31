package shares

import "testing"

func TestParseNFSEndpoint(t *testing.T) {
	cases := []struct {
		in, server, path string
		ok               bool
	}{
		{"nfs://10.0.0.1/pvc-1", "10.0.0.1", "/pvc-1", true},
		{"nfs://h/a/b", "h", "/a/b", true},
		{"nfs://h/", "h", "/", true},
		{"nfs://h", "", "", false},
		{"http://h/x", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		s, p, ok := ParseNFSEndpoint(c.in)
		if s != c.server || p != c.path || ok != c.ok {
			t.Errorf("%q -> %q %q %v", c.in, s, p, ok)
		}
	}
}

func TestShareDerived(t *testing.T) {
	s := New("media", "apps", "media-data", AccessShared)
	if s.Endpoint() != "" {
		t.Error("no server -> empty endpoint")
	}
	s.NFSServer, s.NFSPath = "h", "/p"
	if s.Endpoint() != "h:/p" || s.StructuralKey() != "apps/media-data|media|/shares/media|nfs|shared" {
		t.Errorf("%q %q", s.Endpoint(), s.StructuralKey())
	}
	s.Attached = false
	if s.Endpoint() != "" || s.StructuralKey() != "apps/media-data|media|/shares/media|nfs|shared" {
		t.Error("detached must keep structural key and drop endpoint")
	}
	long := Share{LonghornVolumeName: "a-very-long-longhorn-volume-name-that-exceeds-sixty-three-chars"}
	if n := long.PodVolumeName(); len(n) != 63 || n != long.MigrationPVCName()[:63] {
		t.Errorf("pod volume name %q", n)
	}
}

func TestNormalizeAccess(t *testing.T) {
	for in, want := range map[string]string{"shared": "shared", "read-only": "read-only", "rw": "shared", "": "shared"} {
		if got, _ := NormalizeAccess(map[string]string{"smb-access": in}); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	if got, ok := NormalizeAccess(nil); got != "shared" || !ok {
		t.Error("absent label defaults to shared without warning")
	}
}

func TestSizeMi(t *testing.T) {
	for in, want := range map[any]string{"5368709120": "5120Mi", "abc": "1Gi", int64(2097152): "2Mi", float64(1048576): "1Mi", "1": "0Mi"} {
		if got := SizeMi(map[string]any{"spec": map[string]any{"size": in}}); got != want {
			t.Errorf("%v -> %s want %s", in, got, want)
		}
	}
	if got := SizeMi(map[string]any{}); got != "1024Mi" {
		t.Errorf("missing size -> %s", got)
	}
}

func TestParseMigrationErrors(t *testing.T) {
	if e, _, err := ParseMigration("  \n"); e != nil || err != nil {
		t.Error("blank -> nothing")
	}
	if _, _, err := ParseMigration("volumes: [\n"); err == nil {
		t.Error("bad yaml must error (caller logs and uses no shares)")
	}
}
