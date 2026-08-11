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
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swarmadav1 "github.com/swarmada/swarmada/api/v1"
)

// changesBuffer is the FleetEvent channel depth. Sized so an initial cache sync
// of a normal fleet lands without blocking the informer goroutines; beyond it,
// senders block (backpressure) rather than drop — the reducer drains quickly.
const changesBuffer = 256

// cacheWatcher is the controller-runtime cache-backed FleetWatcher. It is the
// only type in swarmtop that touches client-go / controller-runtime; everything
// above it consumes FleetWatcher and view types.
type cacheWatcher struct {
	cache   cache.Cache
	changes chan FleetEvent

	mu  sync.Mutex
	err error
}

// NewCacheWatcher builds a FleetWatcher over a controller-runtime cache for the
// given REST config. A non-empty namespace scopes the watch; "" is cluster-wide.
func NewCacheWatcher(cfg *rest.Config, namespace string) (FleetWatcher, error) {
	scheme := runtime.NewScheme()
	if err := swarmadav1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register swarmada scheme: %w", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register core scheme: %w", err)
	}
	opts := cache.Options{Scheme: scheme}
	if namespace != "" {
		opts.DefaultNamespaces = map[string]cache.Config{namespace: {}}
	}
	c, err := cache.New(cfg, opts)
	if err != nil {
		return nil, fmt.Errorf("build cache: %w", err)
	}
	return &cacheWatcher{cache: c, changes: make(chan FleetEvent, changesBuffer)}, nil
}

// Changes returns the add/update/delete event stream.
func (w *cacheWatcher) Changes() <-chan FleetEvent { return w.changes }

// Err reports why the watch stopped, if it stopped abnormally.
func (w *cacheWatcher) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *cacheWatcher) setErr(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

// Start wires per-type event handlers, runs the cache, and waits for the
// initial sync. Existing objects surface as Added events during sync.
func (w *cacheWatcher) Start(ctx context.Context) error {
	if err := w.addWorkloadHandler(ctx, &swarmadav1.Robot{}, func(o client.Object, k EventKind) FleetEvent {
		v := mapRobot(o.(*swarmadav1.Robot))
		return FleetEvent{Kind: k, Robot: &v}
	}); err != nil {
		return err
	}
	if err := w.addWorkloadHandler(ctx, &swarmadav1.FleetAction{}, func(o client.Object, k EventKind) FleetEvent {
		v := mapFleetAction(o.(*swarmadav1.FleetAction))
		return FleetEvent{Kind: k, Action: &v}
	}); err != nil {
		return err
	}
	if err := w.addWorkloadHandler(ctx, &swarmadav1.FleetTask{}, func(o client.Object, k EventKind) FleetEvent {
		v := mapFleetTask(o.(*swarmadav1.FleetTask))
		return FleetEvent{Kind: k, Task: &v}
	}); err != nil {
		return err
	}
	if err := w.addWorkloadHandler(ctx, &swarmadav1.RobotProbe{}, func(o client.Object, k EventKind) FleetEvent {
		v := mapRobotProbe(o.(*swarmadav1.RobotProbe))
		return FleetEvent{Kind: k, Probe: &v}
	}); err != nil {
		return err
	}
	if err := w.addWorkloadHandler(ctx, &swarmadav1.FleetZone{}, func(o client.Object, k EventKind) FleetEvent {
		v := mapFleetZone(o.(*swarmadav1.FleetZone))
		return FleetEvent{Kind: k, Zone: &v}
	}); err != nil {
		return err
	}
	if err := w.addWorkloadHandler(ctx, &swarmadav1.FleetAdapter{}, func(o client.Object, k EventKind) FleetEvent {
		v := mapFleetAdapter(o.(*swarmadav1.FleetAdapter))
		return FleetEvent{Kind: k, Adapter: &v}
	}); err != nil {
		return err
	}
	if err := w.addEventPokeHandler(ctx); err != nil {
		return err
	}

	go func() {
		if err := w.cache.Start(ctx); err != nil && ctx.Err() == nil {
			w.setErr(err)
		}
		close(w.changes)
	}()

	if !w.cache.WaitForCacheSync(ctx) {
		return fmt.Errorf("caches did not sync before context ended")
	}
	return nil
}

// makeEvent maps a changed object to a FleetEvent.
type makeEvent func(client.Object, EventKind) FleetEvent

// addWorkloadHandler registers add/update/delete handlers that translate a
// typed object into a FleetEvent and push it onto the stream.
func (w *cacheWatcher) addWorkloadHandler(ctx context.Context, obj client.Object, mk makeEvent) error {
	inf, err := w.cache.GetInformer(ctx, obj)
	if err != nil {
		// CRD not registered in this cluster: skip this kind and run with
		// whatever kinds do exist, so swarmtop shows an empty/partial fleet
		// instead of exiting at startup.
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("get informer for %T: %w", obj, err)
	}
	_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { w.emitObj(ctx, o, EventAdded, mk) },
		UpdateFunc: func(_, o any) { w.emitObj(ctx, o, EventUpdated, mk) },
		DeleteFunc: func(o any) { w.emitObj(ctx, tombstoneObj(o), EventDeleted, mk) },
	})
	if err != nil {
		return fmt.Errorf("add handler for %T: %w", obj, err)
	}
	return nil
}

// addEventPokeHandler turns any change to a Robot-involved core/v1 Event into a
// content-free EventsChanged poke.
func (w *cacheWatcher) addEventPokeHandler(ctx context.Context) error {
	inf, err := w.cache.GetInformer(ctx, &corev1.Event{})
	if err != nil {
		return fmt.Errorf("get informer for Event: %w", err)
	}
	poke := func(o any) {
		if e, ok := o.(*corev1.Event); ok && e.InvolvedObject.Kind != "Robot" {
			return
		}
		w.emit(ctx, FleetEvent{EventsChanged: true})
	}
	_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { poke(o) },
		UpdateFunc: func(_, o any) { poke(o) },
		DeleteFunc: func(o any) { poke(tombstoneObj(o)) },
	})
	if err != nil {
		return fmt.Errorf("add Event handler: %w", err)
	}
	return nil
}

func (w *cacheWatcher) emitObj(ctx context.Context, o any, k EventKind, mk makeEvent) {
	obj, ok := o.(client.Object)
	if !ok {
		return
	}
	w.emit(ctx, mk(obj, k))
}

// emit pushes an event, honoring ctx cancellation so a stalled consumer during
// shutdown can't wedge the informer goroutine forever.
func (w *cacheWatcher) emit(ctx context.Context, ev FleetEvent) {
	select {
	case w.changes <- ev:
	case <-ctx.Done():
	}
}

// RobotEvents lists Events out of the synced cache and buckets Robot-involved
// ones by name, newest-first.
func (w *cacheWatcher) RobotEvents() map[string][]EventView {
	var events corev1.EventList
	if err := w.cache.List(context.Background(), &events); err != nil {
		return nil
	}
	return bucketRobotEvents(events.Items)
}

// tombstoneObj unwraps a cache DeletedFinalStateUnknown tombstone to the last
// known object, so a delete still carries the object's identity.
func tombstoneObj(o any) any {
	if t, ok := o.(toolscache.DeletedFinalStateUnknown); ok {
		return t.Obj
	}
	return o
}
