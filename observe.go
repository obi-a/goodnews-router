package goodnews

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Observer is synchronous on purpose: educational traces must not silently lose
// records to an asynchronous logging queue.
type Observer struct {
	trace, counters, fib *os.File
	captures             map[string]*os.File
	Directory            string
	info                 RunInfo
	versions             map[uint64]bool
}

func OpenObserver(c Config) (_ *Observer, err error) {
	owner, err := resolveArtifactOwner(os.Geteuid(), os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID"))
	if err != nil {
		return nil, err
	}
	if err := owner.mkdirAll(c.Observability.Directory); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(c.Observability.Directory, c.RouterID+"-")
	if err != nil {
		return nil, err
	}
	runDir, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer runDir.Close()
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	o := &Observer{Directory: dir, captures: map[string]*os.File{}, versions: map[uint64]bool{}}
	o.info = RunInfo{Version: 1, RunID: filepath.Base(dir), RouterID: c.RouterID,
		StartedAt: time.Now().UTC(), ArtifactDirectory: absolute, Interfaces: append([]Interface(nil), c.Interfaces...),
		Protections: map[string]bool{"verify_ipv4_checksum": enabled(c.Protections.VerifyIPv4Checksum), "decrement_ttl": enabled(c.Protections.DecrementTTL)}}
	defer func() {
		if err != nil {
			_ = o.Close()
		}
	}()
	if o.trace, err = owner.create(filepath.Join(dir, "trace.jsonl")); err != nil {
		return nil, err
	}
	if o.counters, err = owner.create(filepath.Join(dir, "counters.jsonl")); err != nil {
		return nil, err
	}
	if o.fib, err = owner.create(filepath.Join(dir, "fib.jsonl")); err != nil {
		return nil, err
	}
	manifest, err := json.MarshalIndent(o.info, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestFile, err := owner.create(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, err
	}
	if err = errors.Join(writeAll(manifestFile, append(manifest, '\n')), manifestFile.Close()); err != nil {
		return nil, err
	}
	table, err := BuildTable(c, 1)
	if err != nil {
		return nil, err
	}
	if err = o.RecordFIB(table); err != nil {
		return nil, err
	}
	if err = o.Counters(initialCounters(c)); err != nil {
		return nil, err
	}
	for _, iface := range c.Interfaces {
		f, openErr := owner.create(filepath.Join(dir, iface.Name+".pcap"))
		if openErr != nil {
			return nil, openErr
		}
		o.captures[iface.Name] = f
		linkType := uint32(101)
		if iface.Driver == "ethernet" {
			linkType = 1
		}
		if err := writePCAPHeader(f, linkType); err != nil {
			return nil, err
		}
	}
	// Publish the run to its owner only after all initial artifacts are ready.
	if err := owner.apply(runDir); err != nil {
		return nil, err
	}
	return o, nil
}

func WritePCAPHeader(w io.Writer) error {
	return writePCAPHeader(w, 101)
}

func writePCAPHeader(w io.Writer, linkType uint32) error {
	header := make([]byte, 24)
	binary.LittleEndian.PutUint32(header[0:4], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(header[4:6], 2)
	binary.LittleEndian.PutUint16(header[6:8], 4)
	binary.LittleEndian.PutUint32(header[16:20], 65535)
	if linkType == 1 {
		binary.LittleEndian.PutUint32(header[16:20], 65549)
	}
	binary.LittleEndian.PutUint32(header[20:24], linkType) // LINKTYPE_RAW=101, Ethernet=1.
	return writeAll(w, header)
}

func WritePCAPPacket(w io.Writer, at time.Time, packet []byte) error {
	if len(packet) > 65549 {
		return fmt.Errorf("pcap packet exceeds snap length")
	}
	header := make([]byte, 16)
	binary.LittleEndian.PutUint32(header[0:4], uint32(at.Unix()))
	binary.LittleEndian.PutUint32(header[4:8], uint32(at.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(packet)))
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(packet)))
	if err := writeAll(w, header); err != nil {
		return err
	}
	return writeAll(w, packet)
}

func writeAll(w io.Writer, bytes []byte) error {
	n, err := w.Write(bytes)
	if err == nil && n != len(bytes) {
		return io.ErrShortWrite
	}
	return err
}

func (o *Observer) Capture(iface string, at time.Time, packet []byte) error {
	f, ok := o.captures[iface]
	if !ok {
		return fmt.Errorf("no capture for interface %s", iface)
	}
	return WritePCAPPacket(f, at, packet)
}
func (o *Observer) Trace(record TraceRecord) error { return json.NewEncoder(o.trace).Encode(record) }
func (o *Observer) Counters(record CounterSnapshot) error {
	return json.NewEncoder(o.counters).Encode(record)
}

func (o *Observer) RunInfo() RunInfo { return o.info }

// RecordFIB runs on the observation worker, before any trace using this version.
func (o *Observer) RecordFIB(table *Table) error {
	if o.versions[table.Version()] {
		return nil
	}
	if err := json.NewEncoder(o.fib).Encode(FIBSnapshot{Version: table.Version(), Routes: table.Routes()}); err != nil {
		return err
	}
	o.versions[table.Version()] = true
	return nil
}
func (o *Observer) Close() error {
	var errs []error
	if o.trace != nil {
		errs = append(errs, o.trace.Close())
	}
	if o.counters != nil {
		errs = append(errs, o.counters.Close())
	}
	if o.fib != nil {
		errs = append(errs, o.fib.Close())
	}
	for _, f := range o.captures {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}
