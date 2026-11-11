//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	goodnews "github.com/obi-a/goodnews-router"
	"github.com/obi-a/goodnews-router/drivers"
	"github.com/obi-a/goodnews-router/internal/dashboard"
)

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "goodnews:", err)
		os.Exit(1)
	}
}

func command(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: goodnews check|run|show|craft|dashboard (use COMMAND -h for options)")
	}
	switch args[0] {
	case "check", "run":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		path := fs.String("config", "lab/stage1.json", "router JSON configuration")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments: %v", fs.Args())
		}
		c, err := goodnews.LoadConfig(*path)
		if err != nil {
			return err
		}
		if args[0] == "check" {
			fmt.Fprintln(os.Stdout, "configuration valid")
			return nil
		}
		return run(c)
	case "show":
		return show(args[1:])
	case "craft":
		return craft(args[1:])
	case "dashboard":
		return dashboardCommand(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, "usage: goodnews check|run|show|craft|dashboard (use COMMAND -h for options)")
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func dashboardCommand(args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	config := fs.String("config", "lab/stage1.json", "live router configuration (default when --run is absent)")
	directory := fs.String("run", "", "saved run directory; does not contact a router")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	configSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configSet = true
		}
	})
	if configSet && *directory != "" {
		return fmt.Errorf("dashboard: choose either --config or --run")
	}
	options := dashboard.Options{Directory: *directory}
	if *directory == "" {
		c, err := goodnews.LoadConfig(*config)
		if err != nil {
			return err
		}
		options.Socket = c.Observability.Socket
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return dashboard.Run(ctx, options)
}

func run(c goodnews.Config) (err error) {
	engine, err := goodnews.NewEngine(c)
	if err != nil {
		return err
	}
	links := map[string]goodnews.Link{}
	defer func() { _ = goodnews.CloseLinks(links) }()
	for _, iface := range c.Interfaces {
		var link goodnews.Link
		var err error
		if iface.Driver == "ethernet" {
			link, err = drivers.OpenEthernet(iface.Device, iface.MTU)
		} else {
			link, err = drivers.OpenTUN(iface.Device, iface.NetNS)
		}
		if err != nil {
			return fmt.Errorf("interface %s: %w", iface.Name, err)
		}
		links[iface.Name] = link
	}
	observer, err := goodnews.OpenObserver(c)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, observer.Close()) }()
	router, err := goodnews.NewRuntime(engine, links, observer)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "artifacts:", observer.Directory)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	management := make(chan error, 1)
	go func() { management <- goodnews.ServeManagement(ctx, router, c.Observability.Socket); cancel() }()
	runErr := router.Run(ctx)
	cancel()
	return errors.Join(runErr, <-management)
}

func show(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: goodnews show interfaces|routes|counters|arp|impairments|neighbors|lsdb [--socket PATH] [--json]")
	}
	subsystem := args[0]
	if !strings.Contains("|interfaces|routes|counters|arp|impairments|neighbors|lsdb|", "|"+subsystem+"|") {
		return fmt.Errorf("unknown subsystem %q", subsystem)
	}
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	socket := fs.String("socket", "/run/goodnews/r1.sock", "management Unix socket")
	raw := fs.Bool("json", false, "emit compact JSON")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", *socket)
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("http://goodnews/v1/" + subsystem)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNotImplemented {
		return fmt.Errorf("management: %s", response.Status)
	}
	var value any
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&value); err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	if !*raw {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(value)
}

func craft(args []string) error {
	fs := flag.NewFlagSet("craft", flag.ContinueOnError)
	device := fs.String("interface", "", "host-side TUN or Ethernet device (run in its namespace)")
	destinationMAC := fs.String("dst-mac", "", "next-hop MAC required for Ethernet crafting")
	output := fs.String("output", "", "write raw IPv4 bytes to a file instead of sending")
	source := fs.String("src", "10.0.0.2", "source IPv4 address")
	destination := fs.String("dst", "10.0.1.2", "destination IPv4 address")
	protocol := fs.String("protocol", "icmp", "icmp or udp")
	ttl := fs.Int("ttl", 64, "IPv4 TTL (0..255)")
	payloadSize := fs.Int("payload", 32, "payload bytes after the ICMP/UDP header")
	badChecksum := fs.Bool("bad-checksum", false, "corrupt the IPv4 checksum")
	badICMP := fs.Bool("bad-icmp-checksum", false, "corrupt the ICMP checksum")
	version := fs.Int("version", 4, "IPv4 version nibble (0..15)")
	ihl := fs.Int("ihl", 5, "header-length nibble (0..15; values other than 5 deliberately malformed)")
	length := fs.Int("total-length", -1, "override total length (-1 keeps the computed length)")
	offset := fs.Int("fragment-offset", 0, "fragment offset in eight-byte units")
	mf := fs.Bool("more-fragments", false, "set MF")
	df := fs.Bool("df", false, "set Don't Fragment")
	icmpType := fs.Int("icmp-type", 8, "ICMP type")
	icmpCode := fs.Int("icmp-code", 0, "ICMP code")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *device == "" && *output == "" {
		return fmt.Errorf("craft needs --interface or --output")
	}
	if *device != "" && *output != "" {
		return fmt.Errorf("choose either --interface or --output")
	}
	if *destinationMAC != "" && *device == "" {
		return fmt.Errorf("--dst-mac requires --interface")
	}
	if *ttl < 0 || *ttl > 255 || *payloadSize < 0 || *payloadSize > 65507 || *version < 0 || *version > 15 || *ihl < 0 || *ihl > 15 || *length < -1 || *length > 65535 || *offset < 0 || *offset > 8191 || *icmpType < 0 || *icmpType > 255 || *icmpCode < 0 || *icmpCode > 255 {
		return fmt.Errorf("packet field out of range")
	}
	src, err := netip.ParseAddr(*source)
	if err != nil || !src.Is4() {
		return fmt.Errorf("invalid source IPv4 address")
	}
	dst, err := netip.ParseAddr(*destination)
	if err != nil || !dst.Is4() {
		return fmt.Errorf("invalid destination IPv4 address")
	}
	payload := make([]byte, 8+*payloadSize)
	for i := 8; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	var proto uint8
	switch *protocol {
	case "icmp":
		proto = 1
		payload[0], payload[1] = uint8(*icmpType), uint8(*icmpCode)
		binary.BigEndian.PutUint16(payload[4:6], 1)
		binary.BigEndian.PutUint16(payload[6:8], 1)
		binary.BigEndian.PutUint16(payload[2:4], goodnews.Checksum(payload))
		if *badICMP {
			payload[2] ^= 0xff
		}
	case "udp":
		proto = 17
		binary.BigEndian.PutUint16(payload[0:2], 40000)
		binary.BigEndian.PutUint16(payload[2:4], 33434)
		binary.BigEndian.PutUint16(payload[4:6], uint16(len(payload))) // Zero UDP checksum is legal in IPv4.
	default:
		return fmt.Errorf("protocol must be icmp or udp")
	}
	packet, err := goodnews.BuildIPv4(src, dst, proto, uint8(*ttl), payload)
	if err != nil {
		return err
	}
	flags := uint16(*offset)
	if *mf {
		flags |= 0x2000
	}
	if *df {
		flags |= 0x4000
	}
	binary.BigEndian.PutUint16(packet[6:8], flags)
	goodnews.SetIPv4Checksum(packet)
	if *length >= 0 {
		binary.BigEndian.PutUint16(packet[2:4], uint16(*length))
	}
	packet[0] = byte(*version<<4 | *ihl)
	// For a normal header, keep length overrides independently checksum-valid.
	if *ihl == 5 {
		goodnews.SetIPv4Checksum(packet)
	}
	if *badChecksum {
		packet[10] ^= 0xff
	}
	if *output != "" {
		return os.WriteFile(*output, packet, 0600)
	}
	if *destinationMAC != "" {
		mac, err := net.ParseMAC(*destinationMAC)
		if err != nil || len(mac) != 6 || !goodnews.MAC(mac).Unicast() {
			return fmt.Errorf("dst-mac must be a six-byte unicast MAC")
		}
		return drivers.SendRawIP(*device, packet, goodnews.MAC(mac))
	}
	return drivers.SendRawIP(*device, packet)
}
