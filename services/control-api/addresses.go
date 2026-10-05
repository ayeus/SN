package main

import (
	"net"
	"net/http"
	"strings"
)

// The console hands out one address: the gateway's. Install commands, the
// endpoint a customer calls and the address an agent dials are all the same
// URL, because the gateway carries agent sessions on its own port. In
// production the address is configured. In local development it follows the
// request instead, so the same stack works on one machine, across a home
// network, or over a VPN without any setup: whichever address the browser used
// to reach the console is one the caller's network can route, so it is the one
// handed out.
//
// A loopback address is the exception. These addresses are meant to be pasted
// on other machines, where "localhost" would point at themselves, so loopback
// is replaced by this machine's address on the local network.

// publicBase is the gateway base URL to show the caller, without a trailing slash.
func (a *API) publicBase(r *http.Request) string {
	if a.publicURL != "" {
		return a.publicURL
	}
	host, port := splitHostPort(r.Host)
	if port == "" {
		return "http://" + a.reachableHost(host)
	}
	return "http://" + net.JoinHostPort(a.reachableHost(host), port)
}

// coordinatorBase is the address agents dial: the gateway's, unless the
// installation gives the coordinator a name of its own.
func (a *API) coordinatorBase(r *http.Request) string {
	if a.coordinatorPublicURL != "" {
		return a.coordinatorPublicURL
	}
	return a.publicBase(r)
}

// reachableHost swaps a loopback host for the machine's network address when
// it has one.
func (a *API) reachableHost(host string) string {
	if !isLoopbackHost(host) {
		return host
	}
	if ip := a.lanIP(); ip != "" {
		return ip
	}
	return host
}

func splitHostPort(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return strings.Trim(hostport, "[]"), ""
}

func isLoopbackHost(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localNetworkIP returns the address other machines on the local network use
// to reach this one, or "" when it is not on a network.
func localNetworkIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var list []netInterface
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		ni := netInterface{name: ifc.Name, flags: ifc.Flags}
		for _, addr := range addrs {
			if n, ok := addr.(*net.IPNet); ok {
				ni.ips = append(ni.ips, n.IP)
			}
		}
		list = append(list, ni)
	}

	// The source address of the default route, as a tie-breaker. Dialling UDP
	// sends nothing; it only asks the kernel which address it would use.
	var preferred net.IP
	if conn, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		preferred = conn.LocalAddr().(*net.UDPAddr).IP
		_ = conn.Close()
	}
	return pickLANAddress(list, preferred)
}

type netInterface struct {
	name  string
	flags net.Flags
	ips   []net.IP
}

// virtualInterfacePrefixes name interfaces that carry private addresses no
// other machine can reach: container and VM bridges.
var virtualInterfacePrefixes = []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "bridge", "cni", "flannel"}

// pickLANAddress chooses a private IPv4 address on a real network interface.
//
// The default route alone is the wrong answer on a machine running a VPN: the
// VPN owns the default route, and its tunnel address is one nobody on the
// local network can reach. Tunnels are point-to-point interfaces, so those
// are skipped, as are loopback and virtual bridges. Among what is left, the
// default route's own address wins when it qualifies.
func pickLANAddress(ifaces []netInterface, preferred net.IP) string {
	var candidates []net.IP
	for _, ifc := range ifaces {
		if ifc.flags&net.FlagUp == 0 || ifc.flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		virtual := false
		for _, p := range virtualInterfacePrefixes {
			if strings.HasPrefix(ifc.name, p) {
				virtual = true
				break
			}
		}
		if virtual {
			continue
		}
		for _, ip := range ifc.ips {
			if ip4 := ip.To4(); ip4 != nil && ip4.IsPrivate() {
				candidates = append(candidates, ip4)
			}
		}
	}
	for _, ip := range candidates {
		if preferred != nil && ip.Equal(preferred) {
			return ip.String()
		}
	}
	if len(candidates) > 0 {
		return candidates[0].String()
	}
	return ""
}
