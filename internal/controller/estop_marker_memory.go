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

package controller

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// writtenMarkers is the trigger whose processed marker this process last wrote, per carrier.
// The manager's cache can read a carrier one write behind its own marker, and a pass that reads
// that copy would issue the stop again (row S1 fires once for a trigger). A hit never decides
// anything: it only sends the pass to the API server, which holds the answer, so what is
// remembered cannot withhold a stop. A miss adds no read, so a first stop waits on nothing new.
type writtenMarkers struct {
	mu   sync.Mutex
	last map[types.NamespacedName]string
}

func (w *writtenMarkers) wrote(key types.NamespacedName, trigger string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[types.NamespacedName]string{}
	}
	w.last[key] = trigger
}

func (w *writtenMarkers) has(key types.NamespacedName, trigger string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	got, ok := w.last[key]
	return ok && got == trigger
}

func (w *writtenMarkers) forget(key types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.last, key)
}
