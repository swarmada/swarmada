/*
Copyright 2026 The Swarmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// FileSink is a durable append-only [Sink] that persists each sealed [Entry] as
// one JSON object per line (NDJSON) and fsyncs after every append, so a sealed
// entry survives a crash (§9.6.5.4 durability). It is the production backing for
// the tamper-evident chain; only the elected leader writes, and the mutex
// serialises concurrent producers within that process.
//
// On any write/fsync failure Append returns the error, so [Log.Record] does NOT
// advance the chain (fail-closed: an entry that could not be durably persisted is
// not counted, leaving no sequence gap and letting a safety-relevant producer
// escalate).
type FileSink struct {
	mu sync.Mutex
	// path is retained so the chain tail can be read back at startup (Tail). The write
	// handle is append-only and write-only by design, so it cannot serve that read.
	path string
	f    *os.File
	enc  *json.Encoder
}

// NewFileSink opens (creating if absent) the audit log at path for append. The
// file is 0600 — the audit log is sensitive.
func NewFileSink(path string) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening audit log %q: %w", path, err)
	}
	return &FileSink{path: path, f: f, enc: json.NewEncoder(f)}, nil
}

// Tail reports the highest-sequence entry per namespace, satisfying [ResumableSink].
//
// It re-reads the file rather than tracking the tail in memory, because the case it
// exists for is exactly the one where this process has no memory: a restart. A line
// that does not parse is skipped rather than fatal — a torn final line is the expected
// shape of a crash during append, and refusing to start over it would turn a recoverable
// crash into an outage. Every intact line still chains, so Verify remains the authority
// on whether the log is sound.
func (s *FileSink) Tail() (map[string]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Entry{}, nil // nothing sealed yet; genesis is correct
		}
		return nil, fmt.Errorf("reading audit log %q: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()

	out := map[string]Entry{}
	sc := bufio.NewScanner(f)
	// An entry carries a free-form detail map, so a line can exceed the 64 KiB default.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // torn or foreign line; Verify reports it, Tail does not block on it
		}
		if cur, seen := out[e.Namespace]; !seen || e.SequenceNumber > cur.SequenceNumber {
			out[e.Namespace] = e
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scanning audit log %q: %w", s.path, err)
	}
	return out, nil
}

// Append writes e as one NDJSON line and fsyncs it. Safe for concurrent use.
func (s *FileSink) Append(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return fmt.Errorf("audit FileSink is closed")
	}
	if err := s.enc.Encode(e); err != nil { // Encode appends a trailing newline.
		return fmt.Errorf("writing audit entry: %w", err)
	}
	// Durability: a crash after Record returns MUST NOT lose the sealed entry.
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("fsync audit log: %w", err)
	}
	return nil
}

// Close flushes and closes the underlying file. Further Appends fail closed.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	f := s.f
	s.f, s.enc = nil, nil
	return f.Close()
}

// ReadEntries loads every entry from an NDJSON audit log at path, in write order
// (which is per-namespace sequence order). Callers filter by namespace and pass
// the slice to [Verify] to check chain integrity, or to the audit CLI to display.
func ReadEntries(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening audit log %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var out []Entry
	dec := json.NewDecoder(f)
	for {
		var e Entry
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decoding audit log %q: %w", path, err)
		}
		out = append(out, e)
	}
	return out, nil
}
