package goodnews

import (
	"encoding/json"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{Version: 1, RouterID: "r1", Interfaces: []Interface{
		{Name: "left", Address: "10.0.0.1/24", MTU: 1500, Driver: "tun", Device: "left0"},
		{Name: "right", Address: "10.0.1.1/24", MTU: 1500, Driver: "tun", Device: "right0"},
	}, Observability: ObservationConfig{Directory: "artifacts", Socket: "/run/goodnews/r1.sock"}}
}

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"version", func(c *Config) { c.Version = 2 }},
		{"router traversal", func(c *Config) { c.RouterID = "../r1" }},
		{"duplicate name", func(c *Config) { c.Interfaces[1].Name = "left" }},
		{"duplicate IP", func(c *Config) { c.Interfaces[1].Address = "10.0.0.1/25" }},
		{"duplicate connected prefix", func(c *Config) { c.Interfaces[1].Address = "10.0.0.3/24" }},
		{"IPv6", func(c *Config) { c.Interfaces[0].Address = "::1/128" }},
		{"subnet address", func(c *Config) { c.Interfaces[0].Address = "10.0.0.0/24" }},
		{"broadcast", func(c *Config) { c.Interfaces[0].Address = "10.0.0.255/24" }},
		{"MTU", func(c *Config) { c.Interfaces[0].MTU = 67 }},
		{"driver", func(c *Config) { c.Interfaces[0].Driver = "unknown" }},
		{"Ethernet namespace", func(c *Config) { c.Interfaces[0].Driver = "ethernet"; c.Interfaces[0].NetNS = "/run/netns/h1" }},
		{"relative namespace", func(c *Config) { c.Interfaces[0].NetNS = "h1" }},
		{"device traversal", func(c *Config) { c.Interfaces[0].Device = "../tun0" }},
		{"same attachment", func(c *Config) { c.Interfaces[1].Device = "left0" }},
		{"socket", func(c *Config) { c.Observability.Socket = "relative.sock" }},
		{"missing interface", func(c *Config) { c.Routes = []StaticRoute{{Prefix: "192.0.2.0/24", Interface: "missing"}} }},
		{"duplicate prefix", func(c *Config) { c.Routes = []StaticRoute{{Prefix: "10.0.0.0/24", Interface: "left"}} }},
		{"offlink gateway", func(c *Config) {
			c.Routes = []StaticRoute{{Prefix: "0.0.0.0/0", Interface: "left", NextHop: "10.1.0.2"}}
		}},
		{"self gateway", func(c *Config) {
			c.Routes = []StaticRoute{{Prefix: "0.0.0.0/0", Interface: "left", NextHop: "10.0.0.1"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			tc.change(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	c := testConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !enabled(c.Protections.VerifyIPv4Checksum) || !enabled(c.Protections.DecrementTTL) {
		t.Fatal("protections must default on")
	}
	data, _ := json.Marshal(c)
	for _, input := range []string{string(data) + " {}", strings.Replace(string(data), `"version":1`, `"version":1,"unknown":true`, 1)} {
		if _, err := DecodeConfig(strings.NewReader(input)); err == nil {
			t.Fatal("accepted unknown field or trailing object")
		}
	}
}
