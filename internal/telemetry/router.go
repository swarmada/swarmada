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

package telemetry

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// PlaneConfig is the resolved per-namespace telemetry configuration: which TSDB
// sink receives the high-cadence samples, and the [Config] write-policy that
// governs the low-cadence Robot.status plane. The zero value is the fail-safe
// default — an unset sink ([NoopWriter]: telemetry is dropped, never forced onto
// etcd, RA-1) with the default write policy (§9.1.11.1).
type PlaneConfig struct {
	// SinkType is a Sink* constant selecting the TSDB writer.
	SinkType string
	// Endpoint is the remote-write / ingest endpoint used by the real sinks.
	Endpoint string
	// Projector is the status write-policy for this namespace.
	Projector Config
}

// sinkKey identifies a distinct TSDB sink; the plane's sink is rebuilt only when
// it changes.
func (pc PlaneConfig) sinkKey() string { return pc.SinkType + "\x00" + pc.Endpoint }

// projKey identifies a distinct projection policy; the plane's Projector Config is
// updated in place (preserving per-robot state) only when it changes.
func (pc PlaneConfig) projKey() string {
	return fmt.Sprintf("%d|%d|%v",
		pc.Projector.MinStatusWriteInterval,
		pc.Projector.MaxStatusWritesPerMinute,
		pc.Projector.BatteryThresholds)
}

// PlaneResolver returns the desired [PlaneConfig] for a namespace. It MUST be
// fail-safe: on any problem (no SwarmadaConfig, an unsynced cache, a read error)
// it returns the zero PlaneConfig — a Drop sink plus the default policy — rather
// than an error, so an unreadable telemetry policy degrades to "drop the
// high-cadence samples", never to a forced etcd write (RA-1).
type PlaneResolver func(namespace string) PlaneConfig

// routedPlane is a namespace's cached telemetry plane. The Projector persists
// across reconfiguration so per-robot material state and rate-cap history survive
// a sink or policy change.
type routedPlane struct {
	sinkKey    string
	projKey    string
	projector  *Projector
	ing        *Ingestor
	resolvedAt time.Time
}

// Router is a per-namespace [TelemetryIngestor]. It routes each frame to the TSDB
// sink and Projector write-policy configured for the frame's namespace, resolving
// them from a [PlaneResolver] and caching the built plane so the high-cadence
// ingest path never resolves config — or rebuilds an http sink — per frame.
//
// It re-resolves a namespace at most once per refresh interval (bounded-staleness
// reconfiguration; a SwarmadaConfig watch may call [Router.Invalidate] to
// propagate a change immediately). A reconfiguration preserves the namespace's
// per-robot Projector state, so a policy or sink change never forces a spurious
// re-establishing status write. Safe for concurrent use across adapter streams.
type Router struct {
	resolve PlaneResolver
	sink    StatusSink
	pos     PositionObserver
	newSink func(sinkType, endpoint string) TSDBWriter
	now     func() time.Time
	refresh time.Duration

	mu     sync.Mutex
	planes map[string]*routedPlane
}

var _ interface {
	Ingest(context.Context, Frame) error
} = (*Router)(nil)

// RouterOption configures a [Router].
type RouterOption func(*Router)

// WithPositionObserver installs the per-frame zone-derivation observer on every
// namespace's plane (§9.3.7 Invariant 2: consumed upstream of any sink).
func WithPositionObserver(p PositionObserver) RouterOption {
	return func(r *Router) { r.pos = p }
}

// WithRefreshInterval sets the maximum staleness before a namespace's PlaneConfig
// is re-resolved. Smaller propagates config changes faster at the cost of more
// resolver calls; zero re-resolves on every frame (test/edge use).
func WithRefreshInterval(d time.Duration) RouterOption {
	return func(r *Router) { r.refresh = d }
}

// WithClock injects the clock used for the refresh bound (tests).
func WithClock(now func() time.Time) RouterOption {
	return func(r *Router) { r.now = now }
}

// WithSinkFactory overrides the TSDB sink constructor (defaults to [NewSink]).
// Used by tests to substitute recording sinks.
func WithSinkFactory(f func(sinkType, endpoint string) TSDBWriter) RouterOption {
	return func(r *Router) { r.newSink = f }
}

// NewRouter builds a per-namespace telemetry Router. resolve supplies the desired
// PlaneConfig for a namespace (fail-safe, see [PlaneResolver]); sink is the shared
// low-cadence Robot.status sink (namespace-independent). The default refresh
// interval is 15s and the default sink factory is [NewSink].
func NewRouter(resolve PlaneResolver, sink StatusSink, opts ...RouterOption) *Router {
	r := &Router{
		resolve: resolve,
		sink:    sink,
		newSink: NewSink,
		now:     time.Now,
		refresh: 15 * time.Second,
		planes:  make(map[string]*routedPlane),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Ingest routes one telemetry frame to its namespace's plane. It satisfies
// controlstream.TelemetryIngestor.
func (r *Router) Ingest(ctx context.Context, f Frame) error {
	return r.planeFor(namespaceOf(f.RobotID)).Ingest(ctx, f)
}

// Invalidate forces the next frame for namespace to re-resolve its PlaneConfig, so
// a SwarmadaConfig watch can propagate a telemetry-config change immediately
// rather than waiting out the refresh interval. Unknown namespaces are ignored.
func (r *Router) Invalidate(namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.planes[namespace]; p != nil {
		p.resolvedAt = time.Time{} // zero → next planeFor re-resolves
	}
}

// planeFor returns the namespace's Ingestor, re-resolving and reconciling its
// PlaneConfig on first sight or once the refresh bound has elapsed. Between
// refreshes the cached plane is reused, so the hot path never invokes the resolver
// (which may read the cached client) per frame.
func (r *Router) planeFor(ns string) *Ingestor {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.planes[ns]
	if p == nil || r.now().Sub(p.resolvedAt) >= r.refresh {
		p = r.reconcile(ns, p, r.resolve(ns))
	}
	return p.ing
}

// reconcile brings namespace ns's cached plane into line with desired, building or
// swapping only what changed. The Projector is created once and reused: a policy
// change updates it in place; a sink change installs a NEW Ingestor that shares
// the SAME Projector, so an in-flight Ingest holding the previous *Ingestor
// finishes race-free against the old sink. Caller holds r.mu.
func (r *Router) reconcile(ns string, p *routedPlane, desired PlaneConfig) *routedPlane {
	sinkKey, projKey := desired.sinkKey(), desired.projKey()
	switch {
	case p == nil:
		proj := NewProjector(desired.Projector)
		p = &routedPlane{
			sinkKey:   sinkKey,
			projKey:   projKey,
			projector: proj,
			ing:       r.ingestor(proj, r.newSink(desired.SinkType, desired.Endpoint)),
		}
		r.planes[ns] = p
	default:
		if projKey != p.projKey {
			p.projector.SetConfig(desired.Projector)
			p.projKey = projKey
		}
		if sinkKey != p.sinkKey {
			p.ing = r.ingestor(p.projector, r.newSink(desired.SinkType, desired.Endpoint))
			p.sinkKey = sinkKey
		}
	}
	p.resolvedAt = r.now()
	return p
}

// ingestor assembles an Ingestor from a projector and sink, wiring the shared
// status sink and position observer.
func (r *Router) ingestor(proj *Projector, tsdb TSDBWriter) *Ingestor {
	ing := NewIngestor(tsdb, proj, r.sink)
	ing.PositionObserver = r.pos
	return ing
}
