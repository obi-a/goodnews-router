// Package dashboard is a read-only terminal view of router observation files.
package dashboard

import (
	"bufio"
	"container/list"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"

	goodnews "github.com/obi-a/goodnews-router"
)

const maxRecord = 1 << 20
const cacheSize = 256

type reference struct{ Key, Offset, Length uint64 }

const refSize = 24

type diskIndex struct {
	file  *os.File
	count int64
}

func (d *diskIndex) append(r reference) error {
	if err := d.writeAt(d.count, r); err != nil {
		return err
	}
	d.count++
	return nil
}

func (d *diskIndex) writeAt(row int64, r reference) error {
	var b [refSize]byte
	binary.LittleEndian.PutUint64(b[0:8], r.Key)
	binary.LittleEndian.PutUint64(b[8:16], r.Offset)
	binary.LittleEndian.PutUint64(b[16:24], r.Length)
	if _, err := d.file.WriteAt(b[:], row*refSize); err != nil {
		return err
	}
	return nil
}

func (d *diskIndex) at(n int64) (reference, error) {
	var r reference
	if n < 0 || n >= d.count {
		return r, fmt.Errorf("history position out of range")
	}
	var b [refSize]byte
	if _, err := d.file.ReadAt(b[:], n*refSize); err != nil {
		return r, err
	}
	r.Key, r.Offset, r.Length = binary.LittleEndian.Uint64(b[0:8]), binary.LittleEndian.Uint64(b[8:16]), binary.LittleEndian.Uint64(b[16:24])
	return r, nil
}

func (d *diskIndex) find(key uint64) (reference, int64, error) {
	lo, hi := int64(0), d.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		r, err := d.at(mid)
		if err != nil {
			return reference{}, 0, err
		}
		if r.Key < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == d.count {
		return reference{}, 0, os.ErrNotExist
	}
	r, err := d.at(lo)
	if err == nil && r.Key != key {
		err = os.ErrNotExist
	}
	return r, lo, err
}

type logReader struct {
	file             *os.File
	reader           *bufio.Reader
	position, start  int64
	line             int64
	pending          []byte
	oversize, failed bool
}

func openLog(path string, optional bool) (*logReader, error) {
	f, err := os.Open(path)
	if optional && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &logReader{file: f, reader: bufio.NewReaderSize(f, 64*1024)}, nil
}

// next retains incomplete writes and caps even malformed, unterminated lines.
func (r *logReader) next() ([]byte, reference, bool, error) {
	if r == nil || r.failed {
		return nil, reference{}, false, nil
	}
	for range 32 {
		part, err := r.reader.ReadSlice('\n')
		r.position += int64(len(part))
		if !r.oversize {
			if len(r.pending)+len(part) > maxRecord {
				r.pending = nil
				r.oversize = true
			} else {
				r.pending = append(r.pending, part...)
			}
		}
		if len(part) != 0 && part[len(part)-1] == '\n' {
			r.line++
			ref := reference{Offset: uint64(r.start), Length: uint64(r.position - r.start)}
			r.start = r.position
			line := r.pending
			r.pending = nil
			if r.oversize {
				r.oversize = false
				return nil, ref, true, fmt.Errorf("line %d exceeds 1 MiB", r.line)
			}
			return line, ref, true, nil
		}
		if errors.Is(err, io.EOF) {
			return nil, reference{}, false, nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			r.failed = true
			return nil, reference{}, false, err
		}
	}
	return nil, reference{}, false, nil
}

func decodeAt(reader *logReader, ref reference, value any) error {
	if reader == nil || ref.Length == 0 || ref.Length > maxRecord {
		return os.ErrNotExist
	}
	b := make([]byte, int(ref.Length))
	if _, err := reader.file.ReadAt(b, int64(ref.Offset)); err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

type cached struct {
	row   int64
	event goodnews.TraceRecord
}
type peer struct {
	address string
	row     int64
}

// Store indexes byte offsets on disk; only a small decoded LRU lives in memory.
// All methods are owned by the dashboard event loop.
type Store struct {
	Directory              string
	Info                   goodnews.RunInfo
	Legacy                 bool
	Warnings               []string
	trace, counters, fib   *logReader
	events, counts, tables diskIndex
	relations              diskIndex // Per event row: first child, last child, next sibling (row+1).
	temporary              string
	cache                  map[int64]*list.Element
	lru                    *list.List
	peers                  map[string][]peer
	lastID                 uint64
	lastCounterID          uint64
	hasCounter             bool
}

func OpenStore(directory string) (_ *Store, err error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	s := &Store{Directory: abs, Legacy: true, cache: map[int64]*list.Element{}, lru: list.New(), peers: map[string][]peer{}}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	manifest, readErr := os.ReadFile(filepath.Join(abs, "run.json"))
	if readErr == nil {
		if decodeErr := json.Unmarshal(manifest, &s.Info); decodeErr != nil || s.Info.Version != 1 {
			s.warn("run.json is invalid or unsupported; historical configuration unavailable")
			s.Info = goodnews.RunInfo{}
		} else {
			s.Legacy = false
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read run metadata: %w", readErr)
	}
	if s.Info.RunID == "" {
		s.Info.RunID = filepath.Base(abs)
	}
	if s.trace, err = openLog(filepath.Join(abs, "trace.jsonl"), false); err != nil {
		return nil, fmt.Errorf("open trace (check run directory and ownership): %w", err)
	}
	if s.counters, err = openLog(filepath.Join(abs, "counters.jsonl"), true); err != nil {
		return nil, err
	}
	if s.fib, err = openLog(filepath.Join(abs, "fib.jsonl"), true); err != nil {
		return nil, err
	}
	if s.temporary, err = os.MkdirTemp("", "goodnews-history-"); err != nil {
		return nil, err
	}
	for i, index := range []*diskIndex{&s.events, &s.counts, &s.tables, &s.relations} {
		index.file, err = os.OpenFile(filepath.Join(s.temporary, fmt.Sprintf("%d.index", i)), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) warn(message string) {
	for _, old := range s.Warnings {
		if old == message {
			return
		}
	}
	s.Warnings = append(s.Warnings, message)
	if len(s.Warnings) > 8 {
		s.Warnings = s.Warnings[len(s.Warnings)-8:]
	}
}

// Poll does a bounded amount of work, allowing input and rendering during indexing.
func (s *Store) Poll(limit int) error {
	for _, item := range []struct {
		reader *logReader
		name   string
		index  *diskIndex
	}{
		{s.fib, "fib", &s.tables}, {s.counters, "counters", &s.counts}, {s.trace, "trace", &s.events},
	} {
		if item.reader == nil || item.reader.failed {
			continue
		}
		stat, err := item.reader.file.Stat()
		if err != nil {
			return err
		}
		if stat.Size() < item.reader.position {
			item.reader.failed = true
			s.warn(item.name + " file was truncated; reopen this run")
			continue
		}
		for range limit {
			line, ref, ready, err := item.reader.next()
			if err != nil {
				s.warn(item.name + ": " + err.Error())
			}
			if !ready {
				break
			}
			if err != nil {
				continue
			}
			switch item.name {
			case "trace":
				var event goodnews.TraceRecord
				if err := json.Unmarshal(line, &event); err != nil || (event.Kind != "packet" && event.Kind != "ethernet") || event.PacketID <= s.lastID {
					s.warn(fmt.Sprintf("trace line %d is invalid or has unordered packet IDs", item.reader.line))
					continue
				}
				ref.Key = event.PacketID
				s.lastID = event.PacketID
				if s.Info.RouterID == "" {
					s.Info.RouterID = event.RouterID
				}
				s.observePeers(event, s.events.count)
				if err := s.indexRelation(event, s.events.count); err != nil {
					return err
				}
			case "counters":
				var value goodnews.CounterSnapshot
				if err := json.Unmarshal(line, &value); err != nil || value.Kind != "counters" {
					s.warn(fmt.Sprintf("invalid counters line %d", item.reader.line))
					continue
				}
				// Old logs lack an event correlation; never guess a past snapshot.
				if value.AfterPacketID == nil {
					continue
				}
				ref.Key = *value.AfterPacketID
				if s.hasCounter && ref.Key <= s.lastCounterID {
					s.warn("counter snapshots are not ordered")
					continue
				}
				s.hasCounter, s.lastCounterID = true, ref.Key
			case "fib":
				var value goodnews.FIBSnapshot
				if err := json.Unmarshal(line, &value); err != nil || value.Routes == nil {
					s.warn(fmt.Sprintf("invalid FIB line %d", item.reader.line))
					continue
				}
				ref.Key = value.Version
			}
			if err := item.index.append(ref); err != nil {
				return fmt.Errorf("history index: %w", err)
			}
		}
	}
	return nil
}

func (s *Store) Count() int64 { return s.events.count }

func (s *Store) Event(row int64) (goodnews.TraceRecord, error) {
	if el := s.cache[row]; el != nil {
		s.lru.MoveToFront(el)
		return el.Value.(cached).event, nil
	}
	ref, err := s.events.at(row)
	if err != nil {
		return goodnews.TraceRecord{}, err
	}
	var event goodnews.TraceRecord
	if err := decodeAt(s.trace, ref, &event); err != nil {
		return event, err
	}
	s.cache[row] = s.lru.PushFront(cached{row, event})
	if s.lru.Len() > cacheSize {
		old := s.lru.Back()
		delete(s.cache, old.Value.(cached).row)
		s.lru.Remove(old)
	}
	return event, nil
}

func (s *Store) Counters(event *goodnews.TraceRecord) *goodnews.CounterSnapshot {
	id := uint64(0)
	if event != nil {
		id = event.PacketID
		if event.CounterPacketID != nil {
			id = *event.CounterPacketID
		} else if event.ParentID != nil {
			id = *event.ParentID
		}
	}
	ref, _, err := s.counts.find(id)
	if err != nil {
		return nil
	}
	var counters goodnews.CounterSnapshot
	if decodeAt(s.counters, ref, &counters) != nil {
		return nil
	}
	return &counters
}

func (s *Store) Table(version uint64) *goodnews.FIBSnapshot {
	// Snapshots are few and immutable. The disk index also supports nonconsecutive versions.
	for row := s.tables.count - 1; row >= 0; row-- {
		ref, err := s.tables.at(row)
		if err != nil {
			return nil
		}
		if ref.Key == version {
			var table goodnews.FIBSnapshot
			if decodeAt(s.fib, ref, &table) == nil {
				return &table
			}
			return nil
		}
	}
	return nil
}

func (s *Store) RowForID(id uint64) (int64, bool) {
	_, row, err := s.events.find(id)
	return row, err == nil
}

func (s *Store) Related(event goodnews.TraceRecord) []goodnews.TraceRecord {
	id := event.PacketID
	if event.ParentID != nil {
		id = *event.ParentID
	}
	row, ok := s.RowForID(id)
	if !ok {
		return nil
	}
	var related []goodnews.TraceRecord
	if id != event.PacketID {
		if parent, err := s.Event(row); err == nil {
			related = append(related, parent)
		}
	}
	links, err := s.relations.at(row)
	if err != nil {
		return related
	}
	for next := links.Key; next != 0 && len(related) < 16; {
		childRow := int64(next - 1)
		candidate, err := s.Event(childRow)
		if err != nil {
			break
		}
		if candidate.PacketID != event.PacketID {
			related = append(related, candidate)
		}
		childLinks, err := s.relations.at(childRow)
		if err != nil {
			break
		}
		next = childLinks.Length
	}
	return related
}

// A disk-backed sibling list finds deferred completions even when many other
// packets occur between parent and child, without scanning the entire history.
func (s *Store) indexRelation(event goodnews.TraceRecord, row int64) error {
	if err := s.relations.append(reference{}); err != nil {
		return err
	}
	if event.ParentID == nil || *event.ParentID >= event.PacketID {
		return nil
	}
	parentRow, ok := s.RowForID(*event.ParentID)
	if !ok {
		return nil
	}
	parent, err := s.relations.at(parentRow)
	if err != nil {
		return err
	}
	next := uint64(row + 1)
	if parent.Key == 0 {
		parent.Key = next
	} else {
		previous, err := s.relations.at(int64(parent.Offset - 1))
		if err != nil {
			return err
		}
		previous.Length = next
		if err := s.relations.writeAt(int64(parent.Offset-1), previous); err != nil {
			return err
		}
	}
	parent.Offset = next
	return s.relations.writeAt(parentRow, parent)
}

func (s *Store) observePeers(event goodnews.TraceRecord, row int64) {
	for _, iface := range s.Info.Interfaces {
		prefix, err := netip.ParsePrefix(iface.Address)
		if err != nil {
			continue
		}
		for _, address := range []*netip.Addr{event.Source, event.Destination} {
			if address == nil || !prefix.Contains(*address) || *address == prefix.Addr() {
				continue
			}
			known := s.peers[iface.Name]
			found := false
			for _, p := range known {
				if p.address == address.String() {
					found = true
				}
			}
			// The diagram shows up to three first-observed endpoints per interface.
			if !found && len(known) < 3 {
				s.peers[iface.Name] = append(known, peer{address.String(), row})
			}
		}
	}
}

func (s *Store) Peers(iface string, row int64) []string {
	var peers []string
	for _, p := range s.peers[iface] {
		if p.row <= row {
			peers = append(peers, p.address)
		}
	}
	sort.Strings(peers)
	return peers
}

func (s *Store) Incomplete() bool {
	for _, r := range []*logReader{s.trace, s.counters, s.fib} {
		if r != nil && (len(r.pending) > 0 || r.oversize) {
			return true
		}
	}
	return false
}

func (s *Store) CatchingUp() bool {
	for _, r := range []*logReader{s.trace, s.counters, s.fib} {
		if r == nil || r.failed {
			continue
		}
		if stat, err := r.file.Stat(); err == nil && stat.Size() > r.position {
			return true
		}
	}
	return false
}

func (s *Store) Close() error {
	var errs []error
	for _, r := range []*logReader{s.trace, s.counters, s.fib} {
		if r != nil {
			errs = append(errs, r.file.Close())
		}
	}
	s.trace, s.counters, s.fib = nil, nil, nil
	for _, d := range []*diskIndex{&s.events, &s.counts, &s.tables, &s.relations} {
		if d.file != nil {
			errs = append(errs, d.file.Close())
			d.file = nil
		}
	}
	if s.temporary != "" {
		errs = append(errs, os.RemoveAll(s.temporary))
		s.temporary = ""
	}
	return errors.Join(errs...)
}
