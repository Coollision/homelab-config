package wsd

import (
	"encoding/xml"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
)

func testResponder() (*responder, Device) {
	d := Device{Name: "storage", Workgroup: "HOME", UUID: NewUUID("storage")}
	return newResponder([]Device{d}, net.ParseIP("192.0.2.5"), func(string, ...any) {}), d
}

func probe(types string) []byte {
	return []byte(`<?xml version="1.0"?><soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope" xmlns:w="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">` +
		`<soap:Header><w:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</w:To><w:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</w:Action><w:MessageID>urn:uuid:11111111-2222-3333-4444-555555555555</w:MessageID></soap:Header>` +
		`<soap:Body><d:Probe><d:Types>` + types + `</d:Types></d:Probe></soap:Body></soap:Envelope>`)
}

func TestUUIDStable(t *testing.T) {
	if NewUUID("Storage") != NewUUID("storage") || NewUUID("a") == NewUUID("b") {
		t.Fatal("uuid must be stable per (case-insensitive) name and differ between names")
	}
	if u := NewUUID("x"); len(u) != 36 || u[14] != '5' {
		t.Fatalf("not a v5-style uuid: %s", u)
	}
}

func TestMessagesAreWellFormedXML(t *testing.T) {
	r, d := testResponder()
	for name, msg := range map[string][]byte{
		"hello":    Hello(d, r.xaddrs[d.UUID], "m1", r.seq()),
		"bye":      Bye(d, "m2", r.seq()),
		"probem":   ProbeMatches(d, r.xaddrs[d.UUID], "m3", "urn:uuid:x", r.seq()),
		"resolvem": ResolveMatches(d, r.xaddrs[d.UUID], "m4", "urn:uuid:x", r.seq()),
		"metadata": Metadata(d, "m5", "urn:uuid:x"),
	} {
		if err := xml.Unmarshal(msg, new(struct{ XMLName xml.Name })); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	h := string(Hello(d, r.xaddrs[d.UUID], "m1", r.seq()))
	for _, want := range []string{"wsdp:Device pub:Computer", "http://192.0.2.5:5357/" + d.UUID, "urn:uuid:" + d.UUID} {
		if !strings.Contains(h, want) {
			t.Errorf("hello missing %q", want)
		}
	}
}

func TestProbeAnswered(t *testing.T) {
	r, d := testResponder()
	for _, types := range []string{"", "wsdp:Device", "wsdp:Device pub:Computer", "ns:Computer"} {
		out := r.handleUDP(probe(types))
		if len(out) != 1 {
			t.Fatalf("types %q: %d replies", types, len(out))
		}
		s := string(out[0])
		if !strings.Contains(s, "RelatesTo>urn:uuid:11111111-2222-3333-4444-555555555555<") || !strings.Contains(s, d.UUID) {
			t.Errorf("types %q: reply does not relate to the probe or lacks the device: %s", types, s)
		}
	}
	if out := r.handleUDP(probe("wsdp:Printer")); len(out) != 0 {
		t.Errorf("printer probe must not match a computer")
	}
	if out := r.handleUDP([]byte("not xml")); len(out) != 0 {
		t.Errorf("garbage must be ignored")
	}
}

func TestResolve(t *testing.T) {
	r, d := testResponder()
	msg := func(addr string) []byte {
		return []byte(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"><s:Header><a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Resolve</a:Action><a:MessageID>urn:uuid:abc</a:MessageID></s:Header><s:Body><d:Resolve><a:EndpointReference><a:Address>` + addr + `</a:Address></a:EndpointReference></d:Resolve></s:Body></s:Envelope>`)
	}
	if out := r.handleUDP(msg("urn:uuid:" + d.UUID)); len(out) != 1 || !strings.Contains(string(out[0]), "ResolveMatch") {
		t.Fatalf("resolve of a known device: %v", out)
	}
	if out := r.handleUDP(msg("urn:uuid:00000000-0000-0000-0000-000000000000")); len(out) != 0 {
		t.Fatalf("resolve of an unknown device must be ignored")
	}
}

func TestMetadataEndpoint(t *testing.T) {
	r, d := testResponder()
	get := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing"><s:Header><a:Action>http://schemas.xmlsoap.org/ws/2004/09/transfer/Get</a:Action><a:MessageID>urn:uuid:req1</a:MessageID></s:Header><s:Body/></s:Envelope>`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/"+d.UUID, strings.NewReader(get)))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "<pub:Computer>storage/Workgroup:HOME</pub:Computer>") || !strings.Contains(body, "RelatesTo>urn:uuid:req1<") {
		t.Fatalf("metadata: %d %s", rec.Code, body)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/00000000-0000-0000-0000-000000000000", strings.NewReader(get)))
	if rec.Code != 404 {
		t.Errorf("unknown device: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/"+d.UUID, nil))
	if rec.Code != 404 {
		t.Errorf("GET must 404, got %d", rec.Code)
	}
}
