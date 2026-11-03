package wsd

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	multicastAddr = "239.255.255.250:3702"
	httpPort      = 5357
	maxDatagram   = 65535
	maxDelay      = 500 * time.Millisecond // AppMaxDelay from the WS-Discovery spec
)

// Options configures Run.
type Options struct {
	Devices []Device
	Iface   *net.Interface
	IP      net.IP // the interface's IPv4: where the metadata HTTP endpoint listens and what XAddrs carry
	Logf    func(format string, args ...any)
}

func newMsgID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// responder holds the state shared by the UDP and HTTP sides.
type responder struct {
	devices  map[string]Device // by UUID
	xaddrs   map[string]string // by UUID
	instance int64
	seqID    string
	mu       sync.Mutex
	number   int
	logf     func(string, ...any)
}

func newResponder(devs []Device, ip net.IP, logf func(string, ...any)) *responder {
	r := &responder{devices: map[string]Device{}, xaddrs: map[string]string{}, instance: time.Now().Unix(), seqID: newMsgID(), logf: logf}
	for _, d := range devs {
		if d.UUID == "" {
			d.UUID = NewUUID(d.Name)
		}
		r.devices[d.UUID] = d
		r.xaddrs[d.UUID] = fmt.Sprintf("http://%s:%d/%s", ip, httpPort, d.UUID)
	}
	return r
}

func (r *responder) seq() *appSequence {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.number++
	return &appSequence{instance: r.instance, sequenceID: r.seqID, number: r.number}
}

// handleUDP returns the reply datagrams for one received multicast/unicast message.
func (r *responder) handleUDP(data []byte) [][]byte {
	req, err := parseRequest(data)
	if err != nil {
		return nil
	}
	var out [][]byte
	switch {
	case req.Action == actProbe && req.Probe != nil && probeWants(req.Probe.Types):
		for _, d := range r.devices {
			out = append(out, ProbeMatches(d, r.xaddrs[d.UUID], newMsgID(), req.MessageID, r.seq()))
		}
	case req.Action == actResolve && req.Resolve != nil:
		id := strings.TrimPrefix(strings.TrimSpace(req.Resolve.Address), "urn:uuid:")
		if d, ok := r.devices[id]; ok {
			out = append(out, ResolveMatches(d, r.xaddrs[id], newMsgID(), req.MessageID, r.seq()))
		}
	}
	return out
}

// ServeHTTP answers the WS-Transfer Get for /<uuid> with the device metadata.
func (r *responder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	d, ok := r.devices[strings.Trim(req.URL.Path, "/")]
	if !ok || req.Method != http.MethodPost {
		http.NotFound(w, req)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pr, err := parseRequest(body)
	if err != nil || pr.Action != actGet {
		http.Error(w, "unsupported action", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	_, _ = w.Write(Metadata(d, newMsgID(), pr.MessageID))
}

// Run announces the devices until ctx is done, then sends Bye for each.
func Run(ctx context.Context, o Options) error {
	if len(o.Devices) == 0 {
		return errors.New("wsd: no devices")
	}
	if o.Iface == nil || o.IP == nil {
		return errors.New("wsd: interface and IPv4 address are required")
	}
	r := newResponder(o.Devices, o.IP, o.Logf)

	// One wildcard socket: it receives the multicast probes and unicast ones alike. (Binding the
	// group address and the interface address separately works on macOS but fails with
	// "address already in use" in a Linux pod, so do not split it.) The pod has a second, routed
	// interface, so a plain WriteTo would take its source address from the default route (the pod
	// network) and Windows would never see the reply. Every send therefore carries an explicit
	// source address and interface through an IP_PKTINFO control message.
	raw, err := net.ListenPacket("udp4", ":3702")
	if err != nil {
		return fmt.Errorf("wsd: listen udp 3702: %w", err)
	}
	defer raw.Close()
	pc := ipv4.NewPacketConn(raw)
	if err := pc.JoinGroup(o.Iface, &net.UDPAddr{IP: net.ParseIP("239.255.255.250")}); err != nil {
		return fmt.Errorf("wsd: join multicast on %s: %w", o.Iface.Name, err)
	}
	_ = pc.SetMulticastLoopback(false)
	dst, _ := net.ResolveUDPAddr("udp4", multicastAddr)
	cm := &ipv4.ControlMessage{Src: o.IP, IfIndex: o.Iface.Index}

	srv := &http.Server{Addr: fmt.Sprintf("%s:%d", o.IP, httpPort), Handler: r, ReadHeaderTimeout: 10 * time.Second}
	httpErr := make(chan error, 1)
	go func() { httpErr <- srv.ListenAndServe() }()

	send := func(b []byte, to net.Addr) {
		if _, err := pc.WriteTo(b, cm, to); err != nil {
			o.Logf("wsd: send to %s: %v", to, err)
		}
	}
	// Hello twice a little apart: UDP is lossy and Explorer only needs to hear one.
	hello := func() {
		for _, d := range r.devices {
			send(Hello(d, r.xaddrs[d.UUID], newMsgID(), r.seq()), dst)
		}
	}
	hello()
	for _, d := range r.devices {
		o.Logf("wsd: announcing computer %q (urn:uuid:%s) at %s", d.Name, d.UUID, r.xaddrs[d.UUID])
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			hello()
		}
	}()

	go func() {
		buf := make([]byte, maxDatagram)
		for {
			n, _, from, err := pc.ReadFrom(buf)
			if err != nil {
				return // socket closed on shutdown
			}
			for _, rep := range r.handleUDP(append([]byte(nil), buf[:n]...)) {
				rep, from := rep, from
				go func() {
					j, _ := rand.Int(rand.Reader, big.NewInt(int64(maxDelay/time.Millisecond)))
					time.Sleep(time.Duration(j.Int64()) * time.Millisecond)
					send(rep, from)
				}()
			}
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-httpErr:
		return fmt.Errorf("wsd: http: %w", err)
	}
	for _, d := range r.devices {
		send(Bye(d, newMsgID(), r.seq()), dst)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return nil
}
