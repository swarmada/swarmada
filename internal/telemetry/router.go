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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// PlaneConfig is the resolved per-namespace telemetry configuration: which TSDB
// sink receives the high-cadence samples, and the [Config] write-policy that
// governs the low-cadence Robot.status plane. The zero value is the fail-safe
// default — an unset sink ([NoopWriter]: telemetry is dropped, never forced onto
// etcd, RA-1) with the default write policy (RFC-0001 #swarmadaconfig-telemetry-pipeline-and-status-projection).
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

	// The per-robot last-frame clock (RFC-0001 #control-plane-robot-health-and-connectivity).
	// In memory only and written by nothing else: it records when each robot's own frame
	// last arrived, so the Robot reconciler can tell a silent robot from its live adapter
	// without a status write per frame (RA-1). started anchors robots not yet heard from
	// since this process began.
	framesMu sync.Mutex
	frames   map[frameKey]time.Time
	// firsts is when each robot's first frame since this process began arrived: the
	// release baseline of a leader that finds a robot in estop Resuming. Guarded by
	// framesMu.
	firsts map[frameKey]time.Time
	// phases is the phase each robot last reported, for the estop resume
	// confirmation's check that the robot does not report itself in Error or Offline.
	// It is held here, not in status: Robot.status.phase is control-plane derived and
	// no frame writes it. Guarded by framesMu.
	phases map[frameKey]fleetv1.RobotPhase
	// unwritten records, per robot, whether its most recent frame carried a change
	// that has not reached Robot.status (Projector.Unwritten), for the estop resume
	// confirmation's check that it judges the robot's latest report. Guarded by
	// framesMu.
	unwritten map[frameKey]bool
	started   time.Time
}

// frameKey identifies a robot on the wire: the frame's namespace and robot_id.
type frameKey struct{ namespace, robotID string }

var _ interface {
	Ingest(context.Context, Frame) error
} = (*Router)(nil)

// RouterOption configures a [Router].
type RouterOption func(*Router)

// WithPositionObserver installs the per-frame zone-derivation observer on every
// namespace's plane (RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split Invariant 2: consumed upstream of any sink).
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
		resolve:   resolve,
		sink:      sink,
		newSink:   NewSink,
		now:       time.Now,
		refresh:   15 * time.Second,
		planes:    make(map[string]*routedPlane),
		frames:    make(map[frameKey]time.Time),
		firsts:    make(map[frameKey]time.Time),
		phases:    make(map[frameKey]fleetv1.RobotPhase),
		unwritten: make(map[frameKey]bool),
	}
	for _, o := range opts {
		o(r)
	}
	r.started = r.now()
	return r
}

// Ingest routes one telemetry frame to its namespace's plane. It satisfies
// controlstream.TelemetryIngestor.
//
// The namespace is the one the frame's stream is served in ([Frame.Namespace]),
// which for an adapter with a verified client certificate is the certificate's.
// Only a frame with no namespace falls back to the namespace parsed out of its
// RobotID, and a bare RobotID then routes to the "" plane. That plane resolves its
// configuration with an empty namespace — in cmd/manager, a SwarmadaConfig List
// across ALL namespaces — so it must be reached only when nothing better is known:
// a stream with no verified certificate whose hello declared no namespace.
func (r *Router) Ingest(ctx context.Context, f Frame) error {
	// Stamped once the plane has run the frame, so a reader that sees the stamp can
	// also read any status write the frame caused, and whether it failed.
	plane := r.planeFor(planeNamespace(f))
	err := plane.Ingest(ctx, f)
	r.observeFrame(f, plane.Projector.Unwritten(f.Namespace, f.RobotID))
	return err
}

// observeFrame stamps the robot's last-frame clock. Every frame counts, including one the
// projector will find unchanged: the question is whether the robot is talking, not
// whether it said anything new. unwritten is whether the frame left a change that has
// not reached Robot.status.
func (r *Router) observeFrame(f Frame, unwritten bool) {
	if f.RobotID == "" {
		return
	}
	now := r.now()
	k := frameKey{planeNamespace(f), f.RobotID}
	r.framesMu.Lock()
	r.frames[k] = now
	if _, ok := r.firsts[k]; !ok {
		r.firsts[k] = now
	}
	if f.Phase != "" {
		r.phases[k] = f.Phase
	}
	r.unwritten[k] = unwritten
	r.framesMu.Unlock()
}

// Silence reports how long it has been, on the router's clock, since a frame last
// arrived for the robot. seen is false when none has arrived since the router was
// built; silence is then measured from that instant, so a caller that knows the robot
// streamed before a restart can still time it out.
func (r *Router) Silence(namespace, robotID string) (silence time.Duration, seen bool) {
	now := r.now()
	r.framesMu.Lock()
	last, seen := r.frames[frameKey{namespace, robotID}]
	r.framesMu.Unlock()
	if !seen {
		last = r.started
	}
	return now.Sub(last), seen
}

// LastFrame reports when a frame for the robot was last ingested, on the router's
// clock; ok is false when none has arrived since the router was built. Unlike
// [Router.Silence] it never measures from the router's start, because a robot not yet
// heard from has sent no frame. It is the estop resume witness
// (internal/safety.Dispatcher.Telemetry).
func (r *Router) LastFrame(namespace, robotID string) (time.Time, bool) {
	r.framesMu.Lock()
	defer r.framesMu.Unlock()
	at, ok := r.frames[frameKey{namespace, robotID}]
	return at, ok
}

// FirstFrame reports when the first frame for the robot was ingested, on the router's
// clock; ok is false when none has arrived since the router was built. A process is served
// adapter streams only while it leads, so this is the first frame it received as leader:
// the release baseline of a leader that finds the robot in estop Resuming
// (internal/safety.Dispatcher.ConfirmResume).
func (r *Router) FirstFrame(namespace, robotID string) (time.Time, bool) {
	r.framesMu.Lock()
	defer r.framesMu.Unlock()
	at, ok := r.firsts[frameKey{namespace, robotID}]
	return at, ok
}

// LastReportedPhase is the phase in the robot's most recent frame that carried one;
// ok is false when none has since the router was built. The estop resume confirmation
// reads it (internal/safety.Dispatcher.Telemetry): an operator's release is refused
// while the robot's own report puts it in Error or Offline.
func (r *Router) LastReportedPhase(namespace, robotID string) (fleetv1.RobotPhase, bool) {
	r.framesMu.Lock()
	defer r.framesMu.Unlock()
	p, ok := r.phases[frameKey{namespace, robotID}]
	return p, ok
}

// LastReportUnwritten reports whether the robot's most recent frame left a change that
// has not reached Robot.status: its status write failed, or the projection rate cap is
// holding it. The estop resume confirmation reads it (internal/safety.Dispatcher.Telemetry):
// an operator's release is refused while the status it judges is older than the robot's
// own latest report.
func (r *Router) LastReportUnwritten(namespace, robotID string) bool {
	r.framesMu.Lock()
	defer r.framesMu.Unlock()
	return r.unwritten[frameKey{namespace, robotID}]
}

// planeNamespace is the namespace whose telemetry configuration governs f.
func planeNamespace(f Frame) string {
	if f.Namespace != "" {
		return f.Namespace
	}
	return namespaceOf(f.RobotID)
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

// ForgetRobot drops the recorded material state of the robot with this wire
// robotID in namespace from every namespace plane, so that robot's next frame is
// treated as first sight (see [Projector.Forget]). It reports whether any plane
// held an entry.
//
// The fan-out across PLANES is deliberate rather than lazy. A frame is routed by
// its stream's namespace when it has one, and otherwise by the namespace parsed out
// of its RobotID; a bare wire robot_id — the reference adapter sends a bare name —
// on a stream with no namespace routes to the "" plane. So the plane a robot's
// state is held in is not derivable from the Robot object's namespace. Clearing
// every plane does not depend on knowing which one it is.
//
// Within each plane the drop is scoped by namespace. The control plane does not
// enforce robot_id uniqueness across namespaces (RFC-0001
// #discoveredrobot-conflict-handling-duplicate-robot-id, Case 3), and the projector
// keeps one entry per (namespace, robot_id), so a rebinding of the robot in one
// namespace drops that robot's entry and leaves the other namespace's robot alone.
// Dropping another robot's entry is not harmless: its next frame is first sight and
// issues a status patch, and a Robot reconcile that dropped both robots' entries on
// every call is a status patch per reconcile. An empty namespace means the caller
// does not know it, and the id is dropped under every namespace.
func (r *Router) ForgetRobot(namespace, robotID string) bool {
	r.mu.Lock()
	forgotten := false
	for _, p := range r.planes {
		if p.projector.Forget(namespace, robotID) {
			forgotten = true
		}
	}
	r.mu.Unlock()
	// The last-frame clock is dropped under the same scoping, because it too describes
	// the object that was replaced: kept, it would time the new Robot's silence from the
	// old one's last frame.
	r.framesMu.Lock()
	for k := range r.frames {
		if k.robotID != robotID {
			continue
		}
		if namespace != "" && k.namespace != namespace && k.namespace != "" {
			continue
		}
		delete(r.frames, k)
		delete(r.firsts, k)    // set with the first frame, so keyed as frames is
		delete(r.unwritten, k) // set on every frame, so keyed as frames is
		forgotten = true
	}
	for k := range r.phases {
		if k.robotID != robotID {
			continue
		}
		if namespace != "" && k.namespace != namespace && k.namespace != "" {
			continue
		}
		delete(r.phases, k)
	}
	r.framesMu.Unlock()
	return forgotten
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
