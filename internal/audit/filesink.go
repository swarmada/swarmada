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
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// NewFileSink opens (creating if absent) the audit log at path for append. The
// file is 0600 — the audit log is sensitive.
func NewFileSink(path string) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening audit log %q: %w", path, err)
	}
	return &FileSink{f: f, enc: json.NewEncoder(f)}, nil
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
