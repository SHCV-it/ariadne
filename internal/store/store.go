// Package store is durability only. It is never on the query path.
//
// Deliberate choice: an append-only JSONL log plus a periodic gob snapshot,
// rather than SQLite. Rationale:
//   - The query engine reads exclusively from the in-memory index, so a SQL
//     engine buys nothing at query time.
//   - SQLite means either cgo (breaks the static-binary/cross-compile story
//     for ARM64) or a multi-megabyte pure-Go dependency.
//   - Everything below sits behind the Store interface. Swapping in SQLite for
//     phase 1 (multi-host sync, ad-hoc analytics) touches this file only.
package store

import (
	"bufio"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"ariadne/internal/core"
)

// Snapshot is the full persisted state, minus the raw event log.
type Snapshot struct {
	Version     int
	Entries     []*core.Entry
	Bigrams     map[core.Hash]map[core.Hash]int32
	Tools       map[string]*core.ToolSpec
	Weights     []float64
	WeightsVer  int
	Impressions []Impression
	LastEventTS int64
}

// Impression is one panel render: the features shown and what the user chose.
// This is the ranker's training set.
type Impression struct {
	TS       int64
	Prefix   string
	Features [][]float64
	Hashes   []core.Hash
	Accepted int // -1 = rejected all
}

type Store interface {
	AppendEvent(e *core.Event) error
	LoadEvents(fn func(*core.Event)) error
	SaveSnapshot(s *Snapshot) error
	LoadSnapshot() (*Snapshot, error)
	AppendImpression(im Impression) error
	Compact(keepRawAfter int64) error
	Dir() string
	Close() error
}

type fileStore struct {
	dir string

	mu   sync.Mutex
	log  *os.File
	logW *bufio.Writer
	imp  *os.File
	impW *bufio.Writer
}

func Open(dir string) (Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fs := &fileStore{dir: dir}
	var err error
	fs.log, err = os.OpenFile(filepath.Join(dir, "events.jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	fs.logW = bufio.NewWriterSize(fs.log, 32<<10)

	fs.imp, err = os.OpenFile(filepath.Join(dir, "impressions.jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	fs.impW = bufio.NewWriterSize(fs.imp, 8<<10)
	return fs, nil
}

func (f *fileStore) Dir() string { return f.dir }

func (f *fileStore) AppendEvent(e *core.Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logW.Write(b)
	return f.logW.WriteByte('\n')
}

func (f *fileStore) AppendImpression(im Impression) error {
	b, err := json.Marshal(im)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.impW.Write(b)
	return f.impW.WriteByte('\n')
}

func (f *fileStore) LoadEvents(fn func(*core.Event)) error {
	fh, err := os.Open(filepath.Join(f.dir, "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e core.Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue // tolerate a torn tail line
		}
		fn(&e)
	}
	return sc.Err()
}

func (f *fileStore) SaveSnapshot(s *Snapshot) error {
	f.mu.Lock()
	f.logW.Flush()
	f.impW.Flush()
	f.mu.Unlock()

	tmp := filepath.Join(f.dir, "snapshot.gob.tmp")
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(fh, 64<<10)
	if err := gob.NewEncoder(w).Encode(s); err != nil {
		fh.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return err
	}
	fh.Close()
	return os.Rename(tmp, filepath.Join(f.dir, "snapshot.gob"))
}

func (f *fileStore) LoadSnapshot() (*Snapshot, error) {
	fh, err := os.Open(filepath.Join(f.dir, "snapshot.gob"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()
	var s Snapshot
	if err := gob.NewDecoder(bufio.NewReader(fh)).Decode(&s); err != nil {
		return nil, fmt.Errorf("snapshot corrupt: %w", err)
	}
	return &s, nil
}

// Compact rewrites the event log, dropping raw events older than keepRawAfter.
// Statistics survive in the snapshot; only replay fidelity is lost.
func (f *fileStore) Compact(keepRawAfter int64) error {
	src := filepath.Join(f.dir, "events.jsonl")
	tmp := src + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(out, 64<<10)
	kept := 0
	err = f.LoadEvents(func(e *core.Event) {
		if e.TS < keepRawAfter {
			return
		}
		b, _ := json.Marshal(e)
		w.Write(b)
		w.WriteByte('\n')
		kept++
	})
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	w.Flush()
	out.Sync()
	out.Close()

	f.mu.Lock()
	defer f.mu.Unlock()
	f.logW.Flush()
	f.log.Close()
	if err := os.Rename(tmp, src); err != nil {
		return err
	}
	f.log, err = os.OpenFile(src, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.logW = bufio.NewWriterSize(f.log, 32<<10)
	return nil
}

func (f *fileStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logW.Flush()
	f.impW.Flush()
	f.log.Close()
	return f.imp.Close()
}
