package goodnews

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

// Config describes logical interfaces, never addresses to install in the kernel.
type Config struct {
	Version       int               `json:"version"`
	RouterID      string            `json:"router_id"`
	Interfaces    []Interface       `json:"interfaces"`
	Routes        []StaticRoute     `json:"routes"`
	Observability ObservationConfig `json:"observability"`
	Protections   Protections       `json:"protections"`
}

type Interface struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	MTU     int    `json:"mtu"`
	Driver  string `json:"driver"`
	Device  string `json:"device"`
	NetNS   string `json:"netns,omitempty"`
}

type StaticRoute struct {
	Prefix    string `json:"prefix"`
	Interface string `json:"interface"`
	NextHop   string `json:"next_hop,omitempty"`
}

type ObservationConfig struct {
	Directory string `json:"directory"`
	Socket    string `json:"socket"`
}

// Pointers distinguish an omitted protection (enabled) from an explicit false.
type Protections struct {
	VerifyIPv4Checksum *bool `json:"verify_ipv4_checksum,omitempty"`
	DecrementTTL       *bool `json:"decrement_ttl,omitempty"`
}

func enabled(value *bool) bool { return value == nil || *value }

func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	return DecodeConfig(f)
}

func DecodeConfig(r io.Reader) (Config, error) {
	var c Config
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("config must contain exactly one JSON object")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("config version must be 1")
	}
	if !safeName(c.RouterID) {
		return fmt.Errorf("router_id must contain only letters, digits, hyphens or underscores")
	}
	if len(c.Interfaces) == 0 {
		return fmt.Errorf("at least one interface is required")
	}
	names, addresses, devices := map[string]bool{}, map[netip.Addr]bool{}, map[string]bool{}
	for _, iface := range c.Interfaces {
		if !safeName(iface.Name) || names[iface.Name] {
			return fmt.Errorf("invalid or duplicate interface name %q", iface.Name)
		}
		names[iface.Name] = true
		p, err := netip.ParsePrefix(iface.Address)
		if err != nil || !p.Addr().Is4() || !usableUnicast(p.Addr()) || isSubnetEndpoint(p.Addr(), p.Masked()) {
			return fmt.Errorf("interface %s requires a usable IPv4 address/prefix", iface.Name)
		}
		if addresses[p.Addr()] {
			return fmt.Errorf("duplicate interface address %s", p.Addr())
		}
		addresses[p.Addr()] = true
		if iface.MTU < 68 || iface.MTU > 65535 {
			return fmt.Errorf("interface %s MTU must be 68..65535", iface.Name)
		}
		if iface.Driver != "tun" {
			return fmt.Errorf("interface %s: Stage 1 supports driver tun only", iface.Name)
		}
		if !safeName(iface.Device) || len(iface.Device) > 15 {
			return fmt.Errorf("invalid device name %q", iface.Device)
		}
		if iface.NetNS != "" && !filepath.IsAbs(iface.NetNS) {
			return fmt.Errorf("netns must be an absolute path")
		}
		key := iface.NetNS + "/" + iface.Device
		if devices[key] {
			return fmt.Errorf("duplicate device attachment %s", key)
		}
		devices[key] = true
	}
	if !filepath.IsAbs(c.Observability.Socket) || len(c.Observability.Socket) > 100 {
		return fmt.Errorf("observability.socket must be an absolute Unix socket path of at most 100 bytes")
	}
	if strings.TrimSpace(c.Observability.Directory) == "" {
		return fmt.Errorf("observability.directory is required")
	}
	_, err := BuildTable(c, 1)
	return err
}

func safeName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func usableUnicast(a netip.Addr) bool {
	if !a.Is4() {
		return false
	}
	b := a.As4()
	return b[0] != 0 && b[0] != 127 && b[0] < 224
}

func isSubnetEndpoint(a netip.Addr, prefix netip.Prefix) bool {
	if prefix.Bits() >= 31 || !prefix.Contains(a) {
		return false
	}
	if a == prefix.Masked().Addr() {
		return true
	}
	b, base := a.As4(), prefix.Masked().Addr().As4()
	for bit := prefix.Bits(); bit < 32; bit++ {
		base[bit/8] |= 1 << (7 - bit%8)
	}
	return b == base
}
