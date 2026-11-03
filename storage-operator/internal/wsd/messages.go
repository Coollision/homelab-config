// Package wsd is a minimal WS-Discovery (WSD) responder that makes Windows Explorer list a host
// under Network. Like the Bonjour announcer it publishes a static list and is a *proxy*: each
// device is announced under its own name and UUID while the SMB share itself lives elsewhere
// (Explorer opens \\<name>, which the network's DNS must resolve). It never relays anything.
//
// Protocol shape follows what Samba's wsdd2 / christgau's wsdd send for a "Computer" device:
// Hello/Bye and ProbeMatches/ResolveMatches over UDP multicast 239.255.255.250:3702, and a
// WS-Transfer Get metadata endpoint over HTTP (port 5357) that carries the computer name.
package wsd

import (
	"bytes"
	"crypto/sha1"
	"encoding/xml"
	"fmt"
	"html"
	"strings"
)

// Device is one announced computer.
type Device struct {
	Name      string // bare name Explorer shows and opens (\\Name); must resolve via DNS
	Workgroup string // shown as Workgroup:<Workgroup>; defaults to WORKGROUP
	UUID      string // stable per Name, derived by NewUUID
}

// NewUUID derives a stable RFC 4122 version-5-style UUID from a name, so the device keeps its
// identity across restarts (Windows caches discovered devices by endpoint address).
func NewUUID(name string) string {
	h := sha1.Sum([]byte("wsd-proxy:" + strings.ToLower(name)))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

const (
	nsSoap = "http://www.w3.org/2003/05/soap-envelope"
	nsWsa  = "http://schemas.xmlsoap.org/ws/2004/08/addressing"
	nsWsd  = "http://schemas.xmlsoap.org/ws/2005/04/discovery"
	nsWsdp = "http://schemas.xmlsoap.org/ws/2006/02/devprof"
	nsPub  = "http://schemas.microsoft.com/windows/pub/2005/07"
	nsPnpx = "http://schemas.microsoft.com/windows/pnpx/2005/10"
	nsWsx  = "http://schemas.xmlsoap.org/ws/2004/09/mex"

	actHello        = nsWsd + "/Hello"
	actBye          = nsWsd + "/Bye"
	actProbe        = nsWsd + "/Probe"
	actProbeMatches = nsWsd + "/ProbeMatches"
	actResolve      = nsWsd + "/Resolve"
	actResolveMatch = nsWsd + "/ResolveMatches"
	actGet          = "http://schemas.xmlsoap.org/ws/2004/09/transfer/Get"
	actGetResponse  = "http://schemas.xmlsoap.org/ws/2004/09/transfer/GetResponse"

	toDiscovery = "urn:schemas-xmlsoap-org:ws:2005:04:discovery"
	toAnon      = nsWsa + "/role/anonymous"
)

// types every device advertises.
const deviceTypes = "wsdp:Device pub:Computer"

const envelopeOpen = `<?xml version="1.0" encoding="utf-8"?>` +
	`<soap:Envelope xmlns:soap="` + nsSoap + `" xmlns:wsa="` + nsWsa + `" xmlns:wsd="` + nsWsd +
	`" xmlns:wsdp="` + nsWsdp + `" xmlns:pub="` + nsPub + `" xmlns:pnpx="` + nsPnpx + `" xmlns:wsx="` + nsWsx + `">`

func header(to, action, msgID, relatesTo string, seq *appSequence) string {
	var b strings.Builder
	b.WriteString("<soap:Header>")
	fmt.Fprintf(&b, "<wsa:To>%s</wsa:To><wsa:Action>%s</wsa:Action><wsa:MessageID>urn:uuid:%s</wsa:MessageID>", to, action, msgID)
	if relatesTo != "" {
		fmt.Fprintf(&b, "<wsa:RelatesTo>%s</wsa:RelatesTo>", html.EscapeString(relatesTo))
	}
	if seq != nil {
		fmt.Fprintf(&b, `<wsd:AppSequence InstanceId="%d" SequenceId="urn:uuid:%s" MessageNumber="%d"/>`, seq.instance, seq.sequenceID, seq.number)
	}
	b.WriteString("</soap:Header>")
	return b.String()
}

// appSequence is the WS-Discovery AppSequence header; InstanceId must grow across restarts.
type appSequence struct {
	instance   int64
	sequenceID string
	number     int
}

func (d Device) epr() string {
	return `<wsa:EndpointReference><wsa:Address>urn:uuid:` + d.UUID + `</wsa:Address></wsa:EndpointReference>`
}

func (d Device) matchBody(xaddr string) string {
	return d.epr() + `<wsd:Types>` + deviceTypes + `</wsd:Types><wsd:XAddrs>` + xaddr + `</wsd:XAddrs><wsd:MetadataVersion>1</wsd:MetadataVersion>`
}

// Hello announces d (multicast, on start).
func Hello(d Device, xaddr, msgID string, seq *appSequence) []byte {
	return []byte(envelopeOpen + header(toDiscovery, actHello, msgID, "", seq) +
		`<soap:Body><wsd:Hello>` + d.matchBody(xaddr) + `</wsd:Hello></soap:Body></soap:Envelope>`)
}

// Bye withdraws d (multicast, on shutdown).
func Bye(d Device, msgID string, seq *appSequence) []byte {
	return []byte(envelopeOpen + header(toDiscovery, actBye, msgID, "", seq) +
		`<soap:Body><wsd:Bye>` + d.epr() + `</wsd:Bye></soap:Body></soap:Envelope>`)
}

// ProbeMatches answers a Probe (unicast to the prober).
func ProbeMatches(d Device, xaddr, msgID, relatesTo string, seq *appSequence) []byte {
	return []byte(envelopeOpen + header(toAnon, actProbeMatches, msgID, relatesTo, seq) +
		`<soap:Body><wsd:ProbeMatches><wsd:ProbeMatch>` + d.matchBody(xaddr) + `</wsd:ProbeMatch></wsd:ProbeMatches></soap:Body></soap:Envelope>`)
}

// ResolveMatches answers a Resolve (unicast to the resolver).
func ResolveMatches(d Device, xaddr, msgID, relatesTo string, seq *appSequence) []byte {
	return []byte(envelopeOpen + header(toAnon, actResolveMatch, msgID, relatesTo, seq) +
		`<soap:Body><wsd:ResolveMatches><wsd:ResolveMatch>` + d.matchBody(xaddr) + `</wsd:ResolveMatch></wsd:ResolveMatches></soap:Body></soap:Envelope>`)
}

// Metadata is the WS-Transfer GetResponse body Explorer reads to learn the computer's name.
func Metadata(d Device, msgID, relatesTo string) []byte {
	wg := d.Workgroup
	if wg == "" {
		wg = "WORKGROUP"
	}
	name := html.EscapeString(d.Name)
	return []byte(envelopeOpen + header(toAnon, actGetResponse, msgID, relatesTo, nil) +
		`<soap:Body><wsx:Metadata>` +
		`<wsx:MetadataSection Dialect="` + nsWsdp + `/ThisDevice"><wsdp:ThisDevice>` +
		`<wsdp:FriendlyName>WSD Device ` + name + `</wsdp:FriendlyName><wsdp:FirmwareVersion>1.0</wsdp:FirmwareVersion><wsdp:SerialNumber>1</wsdp:SerialNumber>` +
		`</wsdp:ThisDevice></wsx:MetadataSection>` +
		`<wsx:MetadataSection Dialect="` + nsWsdp + `/ThisModel"><wsdp:ThisModel>` +
		`<wsdp:Manufacturer>storage-operator</wsdp:Manufacturer><wsdp:ModelName>smb proxy</wsdp:ModelName><pnpx:DeviceCategory>Computers</pnpx:DeviceCategory>` +
		`</wsdp:ThisModel></wsx:MetadataSection>` +
		`<wsx:MetadataSection Dialect="` + nsWsdp + `/Relationship" Identifier="` + nsWsdp + `/host"><wsdp:Relationship Type="` + nsWsdp + `/host"><wsdp:Host>` +
		d.epr() + `<wsdp:Types>pub:Computer</wsdp:Types><wsdp:ServiceId>urn:uuid:` + d.UUID + `</wsdp:ServiceId>` +
		`<pub:Computer>` + name + `/Workgroup:` + html.EscapeString(wg) + `</pub:Computer>` +
		`</wsdp:Host></wsdp:Relationship></wsx:MetadataSection>` +
		`</wsx:Metadata></soap:Body></soap:Envelope>`)
}

// request is the part of an incoming SOAP message the responder needs. encoding/xml matches on
// local names here, so namespace prefixes (which differ between Windows versions) do not matter.
type request struct {
	Action    string `xml:"Header>Action"`
	MessageID string `xml:"Header>MessageID"`
	Probe     *struct {
		Types string `xml:"Types"`
	} `xml:"Body>Probe"`
	Resolve *struct {
		Address string `xml:"EndpointReference>Address"`
	} `xml:"Body>Resolve"`
}

// ParseRequest decodes a Probe, Resolve or WS-Transfer Get.
func parseRequest(data []byte) (*request, error) {
	var r request
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	r.Action = strings.TrimSpace(r.Action)
	r.MessageID = strings.TrimSpace(r.MessageID)
	return &r, nil
}

// probeWants reports whether a Probe's Types filter matches our devices: empty means everything;
// otherwise every requested type must be one we advertise (compared by local name).
func probeWants(types string) bool {
	have := map[string]bool{"device": true, "computer": true}
	for _, t := range strings.Fields(types) {
		if i := strings.LastIndex(t, ":"); i >= 0 {
			t = t[i+1:]
		}
		if !have[strings.ToLower(t)] {
			return false
		}
	}
	return true
}
