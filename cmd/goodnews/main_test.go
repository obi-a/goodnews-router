//go:build linux

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	goodnews "github.com/obi-a/goodnews-router"
)

func TestDashboardArguments(t *testing.T) {
	if err := command([]string{"dashboard", "--help"}); err != nil {
		t.Fatal(err)
	}
	if err := dashboardCommand([]string{"--run", "some-run", "--config", "some-config"}); err == nil {
		t.Fatal("ambiguous source accepted")
	}
	if err := dashboardCommand([]string{"unexpected"}); err == nil {
		t.Fatal("unexpected positional argument accepted")
	}
	// Replay must not try to load the default config from this working directory.
	if err := dashboardCommand([]string{"--run", filepath.Join(t.TempDir(), "missing")}); err == nil || !strings.Contains(err.Error(), "open trace") {
		t.Fatalf("%v", err)
	}
}

func TestCraftFiles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flags  []string
		reason string
	}{
		{"valid", nil, ""}, {"checksum", []string{"--bad-checksum"}, "invalid_checksum"},
		{"version", []string{"--version", "6"}, "invalid_version"}, {"ihl", []string{"--ihl", "4"}, "invalid_header_length"},
		{"length", []string{"--total-length", "65000"}, "invalid_total_length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "packet.ip")
			args := append([]string{"--output", file}, tc.flags...)
			if err := craft(args); err != nil {
				t.Fatal(err)
			}
			packet, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			h, err := goodnews.ParseIPv4(packet, true)
			if tc.reason != "" {
				if err == nil || err.Error() != tc.reason {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil || h.TTL != 64 || goodnews.Checksum(h.Bytes[20:]) != 0 || h.Bytes[20] != 8 {
				t.Fatalf("invalid crafted echo: %v", err)
			}
		})
	}
	file := filepath.Join(t.TempDir(), "udp.ip")
	if err := craft([]string{"--output", file, "--protocol", "udp", "--df", "--payload", "100"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if binary.BigEndian.Uint16(data[6:8]) != 0x4000 || data[9] != 17 || binary.BigEndian.Uint16(data[24:26]) != 108 {
		t.Fatal("invalid UDP packet")
	}
	if err := craft([]string{"--ttl", "256"}); err == nil {
		t.Fatal("accepted invalid fields")
	}
}
