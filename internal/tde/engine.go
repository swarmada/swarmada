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

package tde

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Engine is the concrete TrafficDeconflictionEngine. Reservation state is held
// authoritatively in per-zone in-process structures guarded by per-zone locks;
// each mutation is mirrored to FleetZone.status. The per-zone lock is what makes
// check-and-grant atomic — two concurrent requests for the last slot are ordered
// by lock acquisition and exactly one is granted (no double-grant).
type Engine struct {
	client client.Client
	cfg    Config
	now    func() time.Time

	// resolver, when non-nil, supplies a per-namespace Config from SwarmadaConfig
	// tunables (§9.1.11.10); nil falls back to cfg. It is consulted off the hot path
	// (once per reservation/phase change), never per telemetry tick.
	resolver func(namespace string) Config

	// recovered gates the grant path: RequestReservation fails closed until the
	// engine has rebuilt reservation state from FleetZone.status (§9.4.7), so a
	// freshly started or freshly promoted leader never over-grants against empty or
	// stale in-memory state.
	recovered atomic.Bool

	mapMu sync.Mutex
	zones map[string]*zoneState // key: namespace/zone
}

// SetRecovered arms (true) or re-arms fail-closed (false) the grant gate. The
// leader-elected [RecoveryRunnable] drives it around each leadership term; tests may
// set it directly to model a running engine.
func (e *Engine) SetRecovered(v bool) { e.recovered.Store(v) }

// Recovered reports whether the grant gate is open (reservation state rebuilt).
func (e *Engine) Recovered() bool { return e.recovered.Load() }

type zoneState struct {
	mu           sync.Mutex
	reservations []fleetv1.ZoneReservation
	resources    map[string]*fleetv1.SharedResourceQueue // resourceName → queue
}

// New builds an Engine. The client reads FleetZone spec (capacity, resource
// policy) and mirrors reservation state to FleetZone.status.
func New(c client.Client, cfg Config) *Engine {
	return &Engine{client: c, cfg: cfg, now: time.Now, zones: map[string]*zoneState{}}
}

// WithClock overrides the clock (for deterministic TTL tests).
func (e *Engine) WithClock(now func() time.Time) *Engine { e.now = now; return e }

// WithConfigResolver installs a per-namespace [Config] resolver (SwarmadaConfig
// tunables, §9.1.11.10). It MUST be fail-safe — return [DefaultConfig] on any
// problem — so an unreadable policy never zeroes a reservation TTL.
func (e *Engine) WithConfigResolver(f func(namespace string) Config) *Engine {
	e.resolver = f
	return e
}

// configFor resolves the effective Config for a namespace, falling back to the
// constructor cfg when no resolver is installed.
func (e *Engine) configFor(namespace string) Config {
	if e.resolver == nil {
		return e.cfg
	}
	return e.resolver(namespace)
}

func zoneKey(namespace, zone string) string { return namespace + "/" + zone }

func (e *Engine) stateFor(key string) *zoneState {
	e.mapMu.Lock()
	defer e.mapMu.Unlock()
	zs := e.zones[key]
	if zs == nil {
		zs = &zoneState{resources: map[string]*fleetv1.SharedResourceQueue{}}
		e.zones[key] = zs
	}
	return zs
}

// lockZones locks the involved zones' states in a deterministic (sorted-key)
// order to avoid deadlock between concurrent multi-zone requests, and returns the
// unlock func.
func (e *Engine) lockZones(namespace string, zones ...string) (map[string]*zoneState, func()) {
	set := map[string]bool{}
	for _, z := range zones {
		if z != "" {
			set[zoneKey(namespace, z)] = true
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	states := make(map[string]*zoneState, len(keys))
	ordered := make([]*zoneState, len(keys))
	for i, k := range keys {
		zs := e.stateFor(k)
		states[k] = zs
		ordered[i] = zs
	}
	for _, s := range ordered {
		s.mu.Lock()
	}
	return states, func() {
		for i := len(ordered) - 1; i >= 0; i-- {
			ordered[i].mu.Unlock()
		}
	}
}

// counts returns the number of Occupied and live (non-expired) Reserved entries.
func (zs *zoneState) counts(now time.Time) (occupied, reserved int) {
	for i := range zs.reservations {
		r := &zs.reservations[i]
		switch r.State {
		case fleetv1.ReservationOccupied:
			occupied++
		case fleetv1.ReservationReserved:
			if r.ExpiresAt == nil || r.ExpiresAt.After(now) {
				reserved++
			}
		}
	}
	return occupied, reserved
}

// RequestReservation implements the §9.4.4 zone-capacity gate plus §9.4.6 Critical
// preemption, and grants/queues any requested shared resources (§9.4.5).
func (e *Engine) RequestReservation(ctx context.Context, req ReservationRequest) (ReservationResult, error) {
	// Fail closed until reservation state has been recovered from FleetZone.status
	// (§9.4.7). Denying (never granting) is the safe direction; the caller requeues.
	if !e.recovered.Load() {
		return ReservationResult{Status: Denied, DeniedReason: DeniedTDEUnavailable}, nil
	}

	// Resolve the namespace's tunables before locking, so the lock hold covers only
	// the check-and-grant and a single immutable Config governs this reservation.
	cfg := e.configFor(req.Namespace)

	zoneNames := []string{req.TargetZone}
	for _, r := range req.Resources {
		zoneNames = append(zoneNames, r.ZoneName)
	}
	states, unlock := e.lockZones(req.Namespace, zoneNames...)
	defer unlock()

	now := e.now()

	// ── Zone capacity ──────────────────────────────────────────────────────────
	if req.TargetZone != "" {
		zoneSpec, err := e.getZone(ctx, req.Namespace, req.TargetZone)
		if err != nil {
			return ReservationResult{Status: Denied, DeniedReason: DeniedTDEUnavailable}, err
		}
		zs := states[zoneKey(req.Namespace, req.TargetZone)]
		occupied, reserved := zs.counts(now)
		maxC := zoneSpec.Spec.MaxConcurrentRobots

		if maxC != 0 && occupied+reserved >= int(maxC) {
			// Full. A preemptor band (Critical or High) may preempt a lower-priority
			// reservation (§9.4.6).
			if req.Priority.CanPreempt() {
				if preempted := e.preempt(zs); preempted != nil {
					e.appendReservation(zs, req, now, cfg)
					e.grantResources(ctx, req, states, now)
					e.mirrorAll(ctx, req.Namespace, states)
					return ReservationResult{
						Status: PreemptedGranted, ExpiresAt: now.Add(cfg.ReservationTTL),
						PreemptedActionIDs: []string{preempted.ActionID},
					}, nil
				}
			}
			return ReservationResult{
				Status: Denied, DeniedReason: DeniedZoneCapacity,
				RetryAfter: e.earliestExpiry(zs, now, cfg),
			}, nil
		}
	}

	// Capacity OK → grant the zone reservation and enqueue/grant resources.
	if req.TargetZone != "" {
		zs := states[zoneKey(req.Namespace, req.TargetZone)]
		e.appendReservation(zs, req, now, cfg)
	}
	e.grantResources(ctx, req, states, now)
	e.mirrorAll(ctx, req.Namespace, states)
	return ReservationResult{Status: Granted, ExpiresAt: now.Add(cfg.ReservationTTL)}, nil
}

func (e *Engine) appendReservation(zs *zoneState, req ReservationRequest, now time.Time, cfg Config) {
	exp := metav1.NewTime(now.Add(cfg.ReservationTTL))
	zs.reservations = append(zs.reservations, fleetv1.ZoneReservation{
		RobotID:   req.RobotID,
		ActionID:  req.ActionID,
		Priority:  req.Priority,
		State:     fleetv1.ReservationReserved,
		GrantedAt: metav1.NewTime(now),
		ExpiresAt: &exp,
	})
}

// preempt evicts the lowest-priority non-Critical reservation to free a slot
// (§9.4.6). It prefers a Reserved victim (clean eviction) over an Occupied one
// (transient +1), then lowest priority, then most-recently-granted. Returns the
// evicted entry, or nil if none is preemptible.
func (e *Engine) preempt(zs *zoneState) *fleetv1.ZoneReservation {
	best := -1
	for i := range zs.reservations {
		r := &zs.reservations[i]
		if !isPreemptibleBand(r.Priority) {
			// Same rule as the §C controller preemption: a preemptor band (Critical or
			// High) may evict only a Normal/Low reservation — never another Critical or
			// High (FIFO within the preemptor bands). This is intentionally stricter
			// than §9.4.6's literal "priority != Critical".
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		if lessPreemptible(r, &zs.reservations[best]) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	victim := zs.reservations[best]
	zs.reservations = append(zs.reservations[:best], zs.reservations[best+1:]...)
	return &victim
}

// lessPreemptible reports whether a is a "better" preemption victim than b:
// Reserved before Occupied, then lower priority, then more-recently granted, then
// actionID for determinism.
func lessPreemptible(a, b *fleetv1.ZoneReservation) bool {
	if (a.State == fleetv1.ReservationReserved) != (b.State == fleetv1.ReservationReserved) {
		return a.State == fleetv1.ReservationReserved
	}
	if priorityRank(a.Priority) != priorityRank(b.Priority) {
		return priorityRank(a.Priority) > priorityRank(b.Priority) // lower priority first
	}
	if !a.GrantedAt.Time.Equal(b.GrantedAt.Time) {
		return a.GrantedAt.After(b.GrantedAt.Time) // most recent first
	}
	return a.ActionID < b.ActionID
}

func (e *Engine) earliestExpiry(zs *zoneState, now time.Time, cfg Config) time.Duration {
	var earliest time.Time
	for i := range zs.reservations {
		if exp := zs.reservations[i].ExpiresAt; exp != nil {
			if earliest.IsZero() || exp.Time.Before(earliest) {
				earliest = exp.Time
			}
		}
	}
	if earliest.IsZero() {
		return cfg.ReservationTTL
	}
	if d := earliest.Sub(now); d > 0 {
		return d
	}
	return time.Second
}

// grantResources grants or enqueues each requested shared resource (§9.4.5).
func (e *Engine) grantResources(ctx context.Context, req ReservationRequest, states map[string]*zoneState, now time.Time) {
	for _, r := range req.Resources {
		zs := states[zoneKey(req.Namespace, r.ZoneName)]
		if zs == nil {
			continue
		}
		policy := e.resourcePolicy(ctx, req.Namespace, r.ZoneName, r.ResourceName)
		capacity := e.resourceCapacity(ctx, req.Namespace, r.ZoneName, r.ResourceName)
		q := zs.resources[r.ResourceName]
		if q == nil {
			q = &fleetv1.SharedResourceQueue{ResourceName: r.ResourceName}
			zs.resources[r.ResourceName] = q
		}
		// Grant up to capacity concurrent holders (TDE-5); queue the rest.
		if int32(len(q.CurrentHolders)) < capacity {
			q.CurrentHolders = append(q.CurrentHolders, fleetv1.ResourceHolder{
				RobotID: req.RobotID, ActionID: req.ActionID, Priority: req.Priority,
				HeldSince: metav1.NewTime(now),
			})
			continue
		}
		insertByPolicy(q, fleetv1.WaitQueueEntry{
			RobotID: req.RobotID, ActionID: req.ActionID, Priority: req.Priority,
			RequestedAt:              metav1.NewTime(now),
			EstimatedDurationSeconds: r.EstimatedDurationSeconds,
		}, policy)
	}
}

// insertByPolicy inserts a wait-queue entry ordered per the resource's policy
// (§9.4.5).
func insertByPolicy(q *fleetv1.SharedResourceQueue, entry fleetv1.WaitQueueEntry, policy fleetv1.ReservationPolicy) {
	if policy == fleetv1.ReservationFIFO || policy == "" {
		q.WaitQueue = append(q.WaitQueue, entry)
		return
	}
	idx := len(q.WaitQueue)
	for i := range q.WaitQueue {
		if orderBefore(entry, q.WaitQueue[i], policy) {
			idx = i
			break
		}
	}
	q.WaitQueue = append(q.WaitQueue, fleetv1.WaitQueueEntry{})
	copy(q.WaitQueue[idx+1:], q.WaitQueue[idx:])
	q.WaitQueue[idx] = entry
}

// orderBefore reports whether a should be queued ahead of b under the policy.
// Priority never inverts between bands.
func orderBefore(a, b fleetv1.WaitQueueEntry, policy fleetv1.ReservationPolicy) bool {
	ra, rb := priorityRank(a.Priority), priorityRank(b.Priority)
	if ra != rb {
		return ra < rb // higher priority (lower rank) first
	}
	if policy == fleetv1.ReservationPriorityWithDuration {
		da, db := durationOrMax(a.EstimatedDurationSeconds), durationOrMax(b.EstimatedDurationSeconds)
		if da != db {
			return da < db // shortest job first within band
		}
	}
	return a.RequestedAt.Time.Before(b.RequestedAt.Time) // FIFO within band
}

func durationOrMax(d *int32) int64 {
	if d == nil {
		return int64(^uint32(0) >> 1) // MaxInt32 (Deprioritize fallback; FIFO handled by requestedAt)
	}
	return int64(*d)
}

// ReleaseReservation removes a action's zone reservation and releases any shared
// resource it holds, promoting the next queued waiter.
func (e *Engine) ReleaseReservation(ctx context.Context, namespace, zone, actionID string) error {
	states, unlock := e.lockZones(namespace, zone)
	defer unlock()
	zs := states[zoneKey(namespace, zone)]
	zs.reservations = removeByAction(zs.reservations, actionID)
	for _, q := range zs.resources {
		capacity := e.resourceCapacity(ctx, namespace, zone, q.ResourceName)
		releaseHolder(q, actionID, e.now(), capacity)
	}
	e.mirrorAll(ctx, namespace, states)
	return nil
}

func removeByAction(rs []fleetv1.ZoneReservation, actionID string) []fleetv1.ZoneReservation {
	out := rs[:0]
	for _, r := range rs {
		if r.ActionID != actionID {
			out = append(out, r)
		}
	}
	return out
}

// releaseHolder releases a resource held by actionID and promotes the head of the
// wait queue (§9.4.5 onResourceHolderReleased).
func releaseHolder(q *fleetv1.SharedResourceQueue, actionID string, now time.Time, capacity int32) {
	// Remove any holder matching actionID.
	held := false
	kept := q.CurrentHolders[:0]
	for _, h := range q.CurrentHolders {
		if h.ActionID == actionID {
			held = true
			continue
		}
		kept = append(kept, h)
	}
	q.CurrentHolders = kept
	if !held {
		// Not a holder: drop a queued (not-yet-holding) waiter if present.
		q.WaitQueue = removeQueued(q.WaitQueue, actionID)
		return
	}
	// Backfill freed capacity from the head of the (policy-ordered) queue (TDE-5).
	for len(q.WaitQueue) > 0 && int32(len(q.CurrentHolders)) < capacity {
		next := q.WaitQueue[0]
		q.WaitQueue = q.WaitQueue[1:]
		q.CurrentHolders = append(q.CurrentHolders, fleetv1.ResourceHolder{
			RobotID: next.RobotID, ActionID: next.ActionID, Priority: next.Priority,
			HeldSince: metav1.NewTime(now),
		})
	}
}

func removeQueued(wq []fleetv1.WaitQueueEntry, actionID string) []fleetv1.WaitQueueEntry {
	out := wq[:0]
	for _, w := range wq {
		if w.ActionID != actionID {
			out = append(out, w)
		}
	}
	return out
}

// OnRobotEnteredZone transitions the robot's Reserved entry to Occupied.
func (e *Engine) OnRobotEnteredZone(ctx context.Context, namespace, zone, robotID string) error {
	states, unlock := e.lockZones(namespace, zone)
	defer unlock()
	zs := states[zoneKey(namespace, zone)]
	now := e.now()
	for i := range zs.reservations {
		r := &zs.reservations[i]
		if r.RobotID == robotID && r.State == fleetv1.ReservationReserved {
			r.State = fleetv1.ReservationOccupied
			r.ExpiresAt = nil
			entered := metav1.NewTime(now)
			r.EnteredAt = &entered
			break
		}
	}
	e.mirrorAll(ctx, namespace, states)
	return nil
}

// OnRobotExitedZone releases the robot's Occupied entry.
func (e *Engine) OnRobotExitedZone(ctx context.Context, namespace, zone, robotID string) error {
	states, unlock := e.lockZones(namespace, zone)
	defer unlock()
	zs := states[zoneKey(namespace, zone)]
	out := zs.reservations[:0]
	for _, r := range zs.reservations {
		if r.RobotID != robotID || r.State != fleetv1.ReservationOccupied {
			out = append(out, r)
		}
	}
	zs.reservations = out
	e.mirrorAll(ctx, namespace, states)
	return nil
}

// OnActionPhaseChanged extends a Reserved entry's TTL on Revoking (so the slot is
// held while the robot may reconnect) and releases everything on a terminal phase.
func (e *Engine) OnActionPhaseChanged(ctx context.Context, namespace, zone, actionID string, phase fleetv1.ActionPhase) error {
	switch phase {
	case fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseFailed, fleetv1.ActionPhaseCancelled:
		return e.ReleaseReservation(ctx, namespace, zone, actionID)
	case fleetv1.ActionPhaseRevoking:
		cfg := e.configFor(namespace)
		states, unlock := e.lockZones(namespace, zone)
		defer unlock()
		zs := states[zoneKey(namespace, zone)]
		now := e.now()
		for i := range zs.reservations {
			r := &zs.reservations[i]
			if r.ActionID == actionID && r.State == fleetv1.ReservationReserved {
				exp := metav1.NewTime(now.Add(cfg.DisconnectedReservationTTL))
				r.ExpiresAt = &exp
			}
		}
		e.mirrorAll(ctx, namespace, states)
	}
	return nil
}

// ZoneStatus returns a read-only capacity snapshot.
func (e *Engine) ZoneStatus(ctx context.Context, namespace, zone string) (ZoneReservationStatus, error) {
	states, unlock := e.lockZones(namespace, zone)
	defer unlock()
	zs := states[zoneKey(namespace, zone)]
	occ, res := zs.counts(e.now())
	out := ZoneReservationStatus{Occupied: occ, Reserved: res}
	if z, err := e.getZone(ctx, namespace, zone); err == nil {
		out.Max = z.Spec.MaxConcurrentRobots
	}
	return out, nil
}

func (e *Engine) getZone(ctx context.Context, namespace, zone string) (*fleetv1.FleetZone, error) {
	fz := &fleetv1.FleetZone{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: zone, Namespace: namespace}, fz); err != nil {
		return nil, fmt.Errorf("get FleetZone %s/%s: %w", namespace, zone, err)
	}
	return fz, nil
}

// resourcePolicy resolves a shared resource's reservationPolicy from the FleetZone
// spec, defaulting to FIFO.
func (e *Engine) resourcePolicy(ctx context.Context, namespace, zone, resourceName string) fleetv1.ReservationPolicy {
	z, err := e.getZone(ctx, namespace, zone)
	if err != nil {
		return fleetv1.ReservationFIFO
	}
	for i := range z.Spec.SharedResources {
		if z.Spec.SharedResources[i].Name == resourceName {
			if p := z.Spec.SharedResources[i].ReservationPolicy; p != "" {
				return p
			}
		}
	}
	return fleetv1.ReservationFIFO
}

// resourceCapacity resolves a shared resource's capacity from the FleetZone spec,
// defaulting to 1 (a lift/door). Capacity is the maximum number of simultaneous
// holders (TDE-5).
func (e *Engine) resourceCapacity(ctx context.Context, namespace, zone, resourceName string) int32 {
	z, err := e.getZone(ctx, namespace, zone)
	if err != nil {
		return 1
	}
	for i := range z.Spec.SharedResources {
		if z.Spec.SharedResources[i].Name == resourceName {
			if c := z.Spec.SharedResources[i].Capacity; c > 0 {
				return c
			}
		}
	}
	return 1
}

// mirrorAll writes each affected zone's in-process state to its FleetZone.status.
func (e *Engine) mirrorAll(ctx context.Context, namespace string, states map[string]*zoneState) {
	if e.client == nil {
		return
	}
	for key, zs := range states {
		zone := key[len(namespace)+1:]
		e.mirror(ctx, namespace, zone, zs)
	}
}

func (e *Engine) mirror(ctx context.Context, namespace, zone string, zs *zoneState) {
	fz := &fleetv1.FleetZone{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: zone, Namespace: namespace}, fz); err != nil {
		return // zone gone; nothing to mirror
	}
	original := fz.DeepCopy()
	fz.Status.Reservations = append([]fleetv1.ZoneReservation(nil), zs.reservations...)
	fz.Status.SharedResourceQueues = queuesSlice(zs.resources)
	occ, _ := zs.counts(e.now())
	//nolint:gosec // occ is a small, non-negative count of Occupied reservations.
	fz.Status.CurrentConcurrentRobots = int32(occ)
	_ = e.client.Status().Patch(ctx, fz, client.MergeFrom(original))
}

func queuesSlice(m map[string]*fleetv1.SharedResourceQueue) []fleetv1.SharedResourceQueue {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]fleetv1.SharedResourceQueue, 0, len(names))
	for _, n := range names {
		out = append(out, *m[n])
	}
	return out
}
