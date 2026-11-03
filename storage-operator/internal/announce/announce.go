// Package announce publishes static Bonjour records for hosts that do not (or cannot) announce
// themselves, e.g. the smb-server LoadBalancer IP, so macOS Finder lists them under Network. It is
// a *publisher* of a configured list, not a reflector: it never re-publishes anything it hears,
// which is what broke the Matter mesh (see the wiki's mDNS reflector lesson). Each record is a
// proxy announcement: the A record points at the record's IP, not at the pod running this.
package announce

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/grandcat/zeroconf"
	"sigs.k8s.io/yaml"

	"homelab/storage-operator/internal/wsd"
)

// Record is one announced host/service. Instance, Host, IP and Port are required.
type Record struct {
	Instance string   `json:"instance"`       // Finder display name, e.g. "Homelab Storage"
	Host     string   `json:"host"`           // mDNS host label without ".local"
	IP       string   `json:"ip"`             // IPv4 clients connect to; "${VAR}" is expanded from the env
	Port     int      `json:"port"`           // service port, e.g. 445
	Type     string   `json:"type,omitempty"` // service type, default "_smb._tcp"
	Text     []string `json:"txt,omitempty"`  // TXT key=value pairs
	// Model adds a _device-info record (model=<Model>) for the same instance, which picks the
	// Finder icon (e.g. "Xserve"). Empty skips it.
	Model string `json:"model,omitempty"`
	// WSDName, when set, also announces the record over WS-Discovery so Windows Explorer lists it
	// under Network as this computer name. Explorer opens \\<WSDName>, so the network's DNS must
	// resolve that bare name (DHCP search domain + an A/CNAME record). Needs exactly one interface.
	WSDName   string `json:"wsdName,omitempty"`
	Workgroup string `json:"workgroup,omitempty"` // shown with the WSD name; default WORKGROUP
}

// File is the on-disk list.
type File struct {
	Records []Record `json:"records"`
}

const defaultType = "_smb._tcp"

// Parse reads a records file. ${VAR} references are expanded first so private addresses can come
// from a Secret-backed environment variable instead of the (public) values file.
func Parse(data []byte, getenv func(string) string) ([]Record, error) {
	expanded := os.Expand(string(data), getenv)
	var f File
	if err := yaml.UnmarshalStrict([]byte(expanded), &f); err != nil {
		return nil, fmt.Errorf("parsing records: %w", err)
	}
	if len(f.Records) == 0 {
		return nil, errors.New("no records configured")
	}
	var errs []error
	for i := range f.Records {
		r := &f.Records[i]
		if r.Type == "" {
			r.Type = defaultType
		}
		if err := r.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("records[%d] (%s): %w", i, r.Instance, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return f.Records, nil
}

// Validate rejects a record that would announce garbage.
func (r Record) Validate() error {
	var errs []error
	if strings.TrimSpace(r.Instance) == "" {
		errs = append(errs, errors.New("instance is empty"))
	}
	if h := r.Host; h == "" || strings.ContainsAny(h, ". _") {
		errs = append(errs, fmt.Errorf("host %q must be a single DNS label without dots, spaces or underscores", h))
	}
	if ip := net.ParseIP(r.IP); ip == nil || ip.To4() == nil {
		errs = append(errs, fmt.Errorf("ip %q is not an IPv4 address (unset env var?)", r.IP))
	}
	if r.Port < 1 || r.Port > 65535 {
		errs = append(errs, fmt.Errorf("port %d out of range", r.Port))
	}
	if n := r.WSDName; n != "" && (strings.ContainsAny(n, ". _/\\") || len(n) > 15) {
		errs = append(errs, fmt.Errorf("wsdName %q must be a single label of at most 15 characters without dots, spaces, underscores or slashes", n))
	}
	if !strings.HasPrefix(r.Type, "_") || !strings.Contains(r.Type, "._") {
		errs = append(errs, fmt.Errorf("type %q must look like _smb._tcp", r.Type))
	}
	return errors.Join(errs...)
}

type service struct {
	typ  string
	port int
	txt  []string
}

// services returns the (type, port, TXT) sets to publish for this record.
func (r Record) services() []service {
	out := []service{{typ: r.Type, port: r.Port, txt: r.Text}}
	if r.Model != "" {
		// Port 9 is what Apple's own _device-info records use (discard); it is never connected to.
		out = append(out, service{typ: "_device-info._tcp", port: 9, txt: []string{"model=" + r.Model}})
	}
	return out
}

func resolveIfaces(names []string) ([]net.Interface, error) {
	var out []net.Interface
	for _, n := range names {
		i, err := net.InterfaceByName(n)
		if err != nil {
			return nil, fmt.Errorf("interface %q: %w", n, err)
		}
		out = append(out, *i)
	}
	return out, nil
}

// Run announces every record until ctx is done, then withdraws them (mDNS goodbye packets) so
// Finder drops the entries immediately instead of waiting out the TTL. ifaceNames empty means
// every multicast-capable interface; set it so nothing leaks onto the pod network.
func Run(ctx context.Context, records []Record, ifaceNames []string, logf func(format string, args ...any)) error {
	ifaces, err := resolveIfaces(ifaceNames)
	if err != nil {
		return err
	}
	var servers []*zeroconf.Server
	defer func() {
		for _, s := range servers {
			s.Shutdown()
		}
	}()
	for _, r := range records {
		for _, s := range r.services() {
			srv, err := zeroconf.RegisterProxy(r.Instance, s.typ, "local.", s.port, r.Host, []string{r.IP}, s.txt, ifaces)
			if err != nil {
				return fmt.Errorf("registering %q %s: %w", r.Instance, s.typ, err)
			}
			servers = append(servers, srv)
			logf("announcing %q %s on %s.local -> %s:%d (ifaces=%v)", r.Instance, s.typ, r.Host, r.IP, s.port, ifaceNames)
		}
	}
	devices := wsdDevices(records)
	if len(devices) == 0 {
		<-ctx.Done()
		return nil
	}
	if len(ifaces) != 1 {
		return errors.New("wsdName needs exactly one --interfaces entry")
	}
	ip, err := ifaceIPv4(&ifaces[0])
	if err != nil {
		return err
	}
	return wsd.Run(ctx, wsd.Options{Devices: devices, Iface: &ifaces[0], IP: ip, Logf: logf})
}

// wsdDevices returns one WS-Discovery device per record that asks for it.
func wsdDevices(records []Record) []wsd.Device {
	var out []wsd.Device
	for _, r := range records {
		if r.WSDName != "" {
			out = append(out, wsd.Device{Name: r.WSDName, Workgroup: r.Workgroup, UUID: wsd.NewUUID(r.WSDName)})
		}
	}
	return out
}

func ifaceIPv4(i *net.Interface) (net.IP, error) {
	addrs, err := i.Addrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return ipn.IP.To4(), nil
		}
	}
	return nil, fmt.Errorf("interface %s has no IPv4 address", i.Name)
}
