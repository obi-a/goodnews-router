package goodnews

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func testPacket(t testing.TB, src, dst string, proto, ttl uint8, payload []byte) []byte {
	t.Helper()
	p, err := BuildIPv4(netip.MustParseAddr(src), netip.MustParseAddr(dst), proto, ttl, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testEcho() []byte {
	p := []byte{8, 0, 0, 0, 0, 42, 0, 1, 'h', 'e', 'l', 'l', 'o'}
	binary.BigEndian.PutUint16(p[2:4], Checksum(p))
	return p
}

func TestChecksum(t *testing.T) {
	// RFC 1071's example sum is 0xddf2, before taking the complement.
	if got := Checksum([]byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7}); got != 0x220d {
		t.Fatalf("checksum=%04x", got)
	}
	if got := Checksum([]byte{1, 2, 3}); got != 0xfbfd {
		t.Fatalf("odd checksum=%04x", got)
	}
}

func TestParseIPv4Bounds(t *testing.T) {
	valid := testPacket(t, "10.0.0.2", "10.0.1.2", 17, 64, []byte("payload"))
	cases := []struct {
		name   string
		mutate func([]byte) []byte
		reason string
	}{
		{"short", func(p []byte) []byte { return p[:19] }, "truncated_header"},
		{"version", func(p []byte) []byte { p[0] = 0x65; return p }, "invalid_version"},
		{"small ihl", func(p []byte) []byte { p[0] = 0x44; return p }, "invalid_header_length"},
		{"large ihl", func(p []byte) []byte { p[0] = 0x4f; return p }, "invalid_header_length"},
		{"short total", func(p []byte) []byte { binary.BigEndian.PutUint16(p[2:4], 19); return p }, "invalid_total_length"},
		{"long total", func(p []byte) []byte { binary.BigEndian.PutUint16(p[2:4], 65000); return p }, "invalid_total_length"},
		{"checksum", func(p []byte) []byte { p[10] ^= 1; return p }, "invalid_checksum"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.mutate(append([]byte(nil), valid...))
			_, err := ParseIPv4(p, true)
			if err == nil || err.Error() != tc.reason {
				t.Fatalf("got %v, want %s", err, tc.reason)
			}
		})
	}
	h, err := ParseIPv4(append(valid, 0, 0, 0), true)
	if err != nil || len(h.Bytes) != len(valid) {
		t.Fatalf("trailing bytes: %v, %d", err, len(h.Bytes))
	}
	bad := append([]byte(nil), valid...)
	bad[10] ^= 1
	if _, err := ParseIPv4(bad, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseIPv4([]byte{0x45}, false); err == nil {
		t.Fatal("protection disabled bounds check")
	}
}

func FuzzParseIPv4(f *testing.F) {
	f.Add(testPacket(f, "10.0.0.2", "10.0.1.2", 1, 64, testEcho()))
	f.Add([]byte{})
	f.Add([]byte{0x4f, 0, 0, 0})
	f.Fuzz(func(t *testing.T, p []byte) {
		for _, verify := range []bool{true, false} {
			h, err := ParseIPv4(p, verify)
			if err == nil {
				if h.HeaderLength < 20 || h.HeaderLength > len(h.Bytes) || len(h.Bytes) > len(p) {
					t.Fatal("unsafe parsed bounds")
				}
				if verify && Checksum(h.Bytes[:h.HeaderLength]) != 0 {
					t.Fatal("accepted bad checksum")
				}
			}
		}
	})
}
