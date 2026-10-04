package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPickLANAddress(t *testing.T) {
	const up = net.FlagUp | net.FlagBroadcast
	lo := netInterface{"lo0", net.FlagUp | net.FlagLoopback, []net.IP{net.ParseIP("127.0.0.1")}}
	wifi := netInterface{"en0", up, []net.IP{net.ParseIP("fe80::1"), net.ParseIP("192.168.1.4")}}
	vpn := netInterface{"utun4", net.FlagUp | net.FlagPointToPoint, []net.IP{net.ParseIP("172.16.0.2")}}
	docker := netInterface{"docker0", up, []net.IP{net.ParseIP("172.17.0.1")}}
	wired := netInterface{"eth0", up, []net.IP{net.ParseIP("10.0.0.12")}}
	unplugged := netInterface{"en5", net.FlagBroadcast, []net.IP{net.ParseIP("10.9.9.9")}}
	public := netInterface{"eth1", up, []net.IP{net.ParseIP("203.0.113.9")}}

	cases := []struct {
		name      string
		ifaces    []netInterface
		preferred string
		want      string
	}{
		{"a VPN owns the default route: use the Wi-Fi address", []netInterface{lo, wifi, vpn}, "172.16.0.2", "192.168.1.4"},
		{"container bridges are not the local network", []netInterface{lo, docker, wifi}, "192.168.1.4", "192.168.1.4"},
		{"two real networks: the default route decides", []netInterface{lo, wifi, wired}, "10.0.0.12", "10.0.0.12"},
		{"two real networks, no default route: the first", []netInterface{lo, wifi, wired}, "", "192.168.1.4"},
		{"interfaces that are down are ignored", []netInterface{lo, unplugged, wired}, "", "10.0.0.12"},
		{"only a VPN: nothing on the local network", []netInterface{lo, vpn}, "172.16.0.2", ""},
		{"a public address is not handed out", []netInterface{lo, public}, "203.0.113.9", ""},
		{"offline", []netInterface{lo}, "", ""},
	}
	for _, c := range cases {
		if got := pickLANAddress(c.ifaces, net.ParseIP(c.preferred)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// issueToken asks for an install command as a browser at the given address would.
func issueToken(t *testing.T, h *harness, token, host string) (server, coordinator string, commands map[string]string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/hosts/register-token", bytes.NewReader([]byte(`{"tier":"t3","region":"IN-SOUTH"}`)))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register-token via %s: %d %s", host, rec.Code, rec.Body.String())
	}
	var out struct {
		Server      string            `json:"server_url"`
		Coordinator string            `json:"coordinator_url"`
		Commands    map[string]string `json:"commands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Server, out.Coordinator, out.Commands
}

// Without configured addresses (local development) the install command must
// be usable on the machine it is pasted into, which is usually not this one.
func TestInstallCommandFollowsTheAddressTheConsoleWasOpenedOn(t *testing.T) {
	h := newHarness(t)
	h.api.publicURL, h.api.coordinatorPublicURL = "", ""
	h.api.lanIP = func() string { return "192.168.50.7" }
	s := h.signup("addr")

	cases := []struct {
		name, host, server, coordinator string
	}{
		{"opened by network address", "192.168.1.20:8080", "http://192.168.1.20:8080", "http://192.168.1.20:50051"},
		{"opened by VPN name", "gpu-box.tailnet.ts.net:8080", "http://gpu-box.tailnet.ts.net:8080", "http://gpu-box.tailnet.ts.net:50051"},
		{"localhost becomes the network address", "localhost:8080", "http://192.168.50.7:8080", "http://192.168.50.7:50051"},
		{"127.0.0.1 becomes the network address", "127.0.0.1:8080", "http://192.168.50.7:8080", "http://192.168.50.7:50051"},
		{"IPv6 loopback becomes the network address", "[::1]:8080", "http://192.168.50.7:8080", "http://192.168.50.7:50051"},
	}
	for _, c := range cases {
		server, coordinator, cmds := issueToken(t, h, s.Token, c.host)
		if server != c.server || coordinator != c.coordinator {
			t.Errorf("%s: server %q coordinator %q, want %q and %q", c.name, server, coordinator, c.server, c.coordinator)
		}
		for name, cmd := range cmds {
			if !strings.Contains(cmd, c.coordinator) {
				t.Errorf("%s: %s command does not dial %s: %s", c.name, name, c.coordinator, cmd)
			}
			if strings.Contains(cmd, "localhost") || strings.Contains(cmd, "127.0.0.1") {
				t.Errorf("%s: %s command points at the loopback address: %s", c.name, name, cmd)
			}
		}
		if !strings.Contains(cmds["linux_macos"], "curl -fsSL "+c.server+"/install.sh") {
			t.Errorf("%s: installer is not fetched from %s: %s", c.name, c.server, cmds["linux_macos"])
		}
	}

	// A machine with no network keeps working on its own.
	h.api.lanIP = func() string { return "" }
	if server, coordinator, _ := issueToken(t, h, s.Token, "localhost:8080"); server != "http://localhost:8080" || coordinator != "http://localhost:50051" {
		t.Errorf("offline: got %q and %q, want the loopback addresses", server, coordinator)
	}

	// Configured addresses always win over the request.
	h.api.publicURL, h.api.coordinatorPublicURL = "https://app.example.com", "https://agents.example.com"
	if server, coordinator, _ := issueToken(t, h, s.Token, "evil.example.net"); server != "https://app.example.com" || coordinator != "https://agents.example.com" {
		t.Errorf("configured: got %q and %q, want the configured addresses", server, coordinator)
	}
}
