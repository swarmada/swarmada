// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sclient

import (
	"context"
	"sync"
)

// StaticStore is a Store backed by an in-memory Fleet rather than a cluster. It
// exists so the UI can be exercised (in tests, and later in an offline demo
// mode) without kubeconfig, informers, or envtest. Set the fleet with Set,
// which also fires a Changed() nudge, exactly as the live store would.
type StaticStore struct {
	mu      sync.RWMutex
	fleet   Fleet
	changed chan struct{}
}

// NewStaticStore returns a StaticStore seeded with the given fleet.
func NewStaticStore(f Fleet) *StaticStore {
	return &StaticStore{fleet: f, changed: make(chan struct{}, 1)}
}

// Start is a no-op: a static store is always "synced".
func (s *StaticStore) Start(context.Context) error { return nil }

// Snapshot returns the current fleet. The stored Fleet is treated as immutable
// between Set calls, so returning it directly is safe for the UI's read-only use.
func (s *StaticStore) Snapshot() Fleet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fleet
}

// Changed returns the coalesced change-nudge channel.
func (s *StaticStore) Changed() <-chan struct{} { return s.changed }

// Set replaces the fleet and fires a coalesced Changed() nudge.
func (s *StaticStore) Set(f Fleet) {
	s.mu.Lock()
	s.fleet = f
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
