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

// Package leadership is this control-plane process's hold on leadership and the leader term
// that goes with it (RFC-0001 #fleet-adapter-protocol-the-stop-latch, The leader term).
//
// A control-plane process sends on an adapter's streams only while it holds leadership, and
// closes them when it loses it (RFC-0001 #safety-robot-stop-table). State is what the stream
// server and the command dispatcher ask: whether this process leads, under which term, and a
// channel that closes when it stops leading. Runnable is the one writer of State. It runs
// only on the elected leader, takes a new term each time this process acquires leadership,
// and gives leadership up when the manager cancels it, which controller-runtime does when the
// election Lease is lost.
//
// The term never decreases, even if the election Lease is deleted: Floor persists the highest
// term issued in its own Lease and takes max(floor, the election Lease's transition count)+1
// on each acquisition.
package leadership

import (
	"sync"
)

// State is this process's hold on leadership. The zero value does not lead. It is safe for
// concurrent use.
type State struct {
	mu      sync.Mutex
	leading bool
	term    uint64
	lost    chan struct{}
}

// Leading reports whether this process holds leadership and, when it does, a channel that is
// closed when it loses it. A stream opened while leading is closed when that channel closes.
func (s *State) Leading() (lost <-chan struct{}, leading bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.leading {
		return nil, false
	}
	return s.lost, true
}

// Term is the term of the leadership this process holds, and whether it holds it. It is never
// 0 while leading: 0 on the wire means no term was sent.
func (s *State) Term() (term uint64, leading bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term, s.leading
}

// Acquire records that this process leads under term. A term that is not higher than the one
// held before is a fault of the caller, since a term is never reused; Acquire refuses it and
// reports false, and the process does not lead.
func (s *State) Acquire(term uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leading || term == 0 || term <= s.term {
		return false
	}
	s.leading = true
	s.term = term
	s.lost = make(chan struct{})
	return true
}

// Release records that this process no longer leads, and closes the channel Leading handed
// out, so every stream opened under that leadership closes. The term is kept, so a later
// Acquire must exceed it. Release when not leading does nothing.
func (s *State) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.leading {
		return
	}
	s.leading = false
	close(s.lost)
	s.lost = nil
}
