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
	trace, counters *os.File
	captures        map[string]*os.File
	Directory       string
}

func OpenObserver(c Config) (_ *Observer, err error) {
	if err := os.MkdirAll(c.Observability.Directory, 0700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(c.Observability.Directory, c.RouterID+"-")
	if err != nil {
		return nil, err
	}
	o := &Observer{Directory: dir, captures: map[string]*os.File{}}
	defer func() {
		if err != nil {
			_ = o.Close()
		}
	}()
	if o.trace, err = os.OpenFile(filepath.Join(dir, "trace.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err != nil {
		return nil, err
	}
	if o.counters, err = os.OpenFile(filepath.Join(dir, "counters.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err != nil {
		return nil, err
	}
	for _, iface := range c.Interfaces {
		f, openErr := os.OpenFile(filepath.Join(dir, iface.Name+".pcap"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if openErr != nil {
			return nil, openErr
		}
		o.captures[iface.Name] = f
		if err := WritePCAPHeader(f); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func WritePCAPHeader(w io.Writer) error {
	header := make([]byte, 24)
	binary.LittleEndian.PutUint32(header[0:4], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(header[4:6], 2)
	binary.LittleEndian.PutUint16(header[6:8], 4)
	binary.LittleEndian.PutUint32(header[16:20], 65535)
	binary.LittleEndian.PutUint32(header[20:24], 101) // LINKTYPE_RAW, not the platform-specific DLT value.
	return writeAll(w, header)
}

func WritePCAPPacket(w io.Writer, at time.Time, packet []byte) error {
	if len(packet) > 65535 {
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
func (o *Observer) Close() error {
	var errs []error
	if o.trace != nil {
		errs = append(errs, o.trace.Close())
	}
	if o.counters != nil {
		errs = append(errs, o.counters.Close())
	}
	for _, f := range o.captures {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}
