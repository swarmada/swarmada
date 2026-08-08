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

import "context"

// Store is the seam the UI reads through: a pull-based, whole-fleet snapshot
// view (option C). The UI asks for a consistent Fleet whenever it is ready to
// render; Changed() is a coalesced "something moved" nudge. Nothing about the
// CRDs or client-go leaks past this interface.
//
// The production Store is a reducer over a FleetWatcher (see reducer_store.go):
// it consumes the watcher's add/update/delete stream into name-keyed maps and
// materializes a Fleet on demand. That keeps the informer machinery behind
// FleetWatcher and makes the whole read path testable with a FakeWatcher.
type Store interface {
	// Start sets up the underlying watch and blocks until it has synced (or ctx
	// is cancelled). After it returns nil, Snapshot() reflects cluster state and
	// Changed() delivers nudges.
	Start(ctx context.Context) error

	// Snapshot returns an independent, consistent copy of everything watched.
	// Safe to call concurrently and to retain.
	Snapshot() Fleet

	// Changed delivers a coalesced nudge after any watched object changes. It
	// never blocks the producer: additional changes collapse into the one
	// already buffered.
	Changed() <-chan struct{}
}
