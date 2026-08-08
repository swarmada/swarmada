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

// Package edge is the Zone Controller edge node (RFC-0001 §9.6.2.5, §9.2.10;
// the site connectivity boundary, ADR-0010). It
// runs on facility-LAN hardware and serves the EdgeService.EdgeStream — a channel
// that survives a control-plane partition. It detects zone-boundary breaches from
// the position tee and issues headless estops, using the same confirmed-EstopAck
// discipline as the control plane: a robot is STOPPED only on an adapter-CONFIRMED
// EstopAck.state=STOPPED, never inferred from silence or a timeout. Absence of
// position frames is never a breach.
package edge

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/zone"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// eventEdgeEstop is the local audit event for a headless estop.
const eventEdgeEstop = "EDGE_ESTOP_TRIGGERED"

// Default edge timings mirror §9.6.2.3 (confirmed within the window, else Failed).
const (
	defaultDeliveryTimeout = 2 * time.Second
	defaultConfirmTimeout  = 10 * time.Second
	// defaultFeedGrace is how long a robot may be absent from the EdgeStream after
	// first appearing in the synced config before its feed is reported unavailable.
	// Several sync intervals, so a normally-connecting adapter is never flagged.
	defaultFeedGrace = 90 * time.Second
)

// ZonePolygon is a cached zone boundary the edge node guards.
type ZonePolygon struct {
	Name    string
	Floor   int32
	Polygon []zone.Point
}

// Config is the edge node's cached view of its zones and robot assignments,
// synced from the control plane and retained across a partition.
type Config struct {
	// Namespace identifies the edge node in its local audit log.
	Namespace string
	// Zones are the boundaries this node evaluates positions against.
	Zones []ZonePolygon
	// RobotZone maps a robot to its assigned zone name. A frame for a robot absent
	// from this map is ignored (the edge cannot act on an unknown/forged robot).
	RobotZone map[string]string
	// EdgeZones is the set of zone names that declare an edge node, so the feed
	// reporter knows which zones expect an EdgeStream feed. A robot whose zone is
	// not an EdgeZone is never reported as feed-unavailable.
	EdgeZones map[string]bool
}

// configView is an immutable snapshot of the node's Config plus its zone-name index.
// It is swapped atomically by SetConfig, so a boundary evaluation always reads one
// coherent config — never a half-applied one — with no lock on the hot path.
type configView struct {
	cfg        Config
	zoneByName map[string]ZonePolygon
}

// Node serves EdgeService.EdgeStream. Its zero value is not usable; use New.
type Node struct {
	fav1.UnimplementedEdgeServiceServer

	cfg   atomic.Pointer[configView] // hot-swappable via SetConfig; reads are lock-free
	audit audit.Recorder

	now             func() time.Time
	deliveryTimeout time.Duration
	confirmTimeout  time.Duration
	feedGrace       time.Duration

	mu      sync.Mutex
	conns   map[*conn]struct{}
	pending map[string]chan *fav1.EstopAck
	issued  map[string]bool // robotID → an edge estop has already been issued
	idc     uint64

	// everSeen records robots from which at least one EdgeStream PositionFrame has
	// arrived; expectedSince records when each robot first appeared in the synced
	// config. Together they drive the never-seen-beyond-grace feed report — a robot
	// whose adapter never established its EdgeStream is never in everSeen.
	everSeen      map[string]struct{}
	expectedSince map[string]time.Time
}

// New builds an edge Node with the §9.6.2 default timings.
func New(cfg Config, auditLog audit.Recorder) *Node {
	n := &Node{
		audit:           auditLog,
		deliveryTimeout: defaultDeliveryTimeout,
		confirmTimeout:  defaultConfirmTimeout,
		feedGrace:       defaultFeedGrace,
		conns:           map[*conn]struct{}{},
		pending:         map[string]chan *fav1.EstopAck{},
		issued:          map[string]bool{},
		everSeen:        map[string]struct{}{},
		expectedSince:   map[string]time.Time{},
	}
	n.SetConfig(cfg)
	return n
}

// SetConfig atomically replaces the node's cached zones and robot→zone assignments.
// The config sync path (see Syncer) calls this to keep the node current; it is safe
// to call concurrently with live EdgeStreams. It does NOT reset per-robot estop
// idempotency — a robot already estopped under the old config stays estopped.
func (n *Node) SetConfig(cfg Config) {
	byName := make(map[string]ZonePolygon, len(cfg.Zones))
	for _, z := range cfg.Zones {
		byName[z.Name] = z
	}
	n.cfg.Store(&configView{cfg: cfg, zoneByName: byName})

	// Drop feed-tracking state for robots no longer in config so the maps do not
	// grow without bound as robots come and go. Robots still present keep their
	// expectedSince/everSeen (grace and never-seen status persist across syncs).
	n.mu.Lock()
	for robot := range n.expectedSince {
		if _, ok := cfg.RobotZone[robot]; !ok {
			delete(n.expectedSince, robot)
			delete(n.everSeen, robot)
		}
	}
	n.mu.Unlock()
}

// markSeen records that an EdgeStream PositionFrame has arrived for robotID, so it
// is never reported as feed-unavailable (its adapter established its EdgeStream).
func (n *Node) markSeen(robotID string) {
	n.mu.Lock()
	n.everSeen[robotID] = struct{}{}
	n.mu.Unlock()
}

// MissingFeeds reports, per edge-node zone, the robots assigned to it from which no
// EdgeStream PositionFrame has arrived within the feed-grace window since they first
// appeared in the synced config — i.e. an adapter serving them never established its
// EdgeStream (§9.2.10). Every EdgeZone is present in the result (an empty slice when
// all its feeds are live), so a caller can both raise and clear the report. The
// grace window is seeded lazily on first observation, so a just-synced robot is
// never flagged before it has had a chance to connect.
func (n *Node) MissingFeeds() map[string][]string {
	cfg := n.view().cfg
	now := n.clock()

	report := make(map[string][]string, len(cfg.EdgeZones))
	for zoneName, isEdge := range cfg.EdgeZones {
		if isEdge {
			report[zoneName] = nil
		}
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	for robot, zoneName := range cfg.RobotZone {
		if !cfg.EdgeZones[zoneName] {
			continue // robot not in an edge-node zone: no feed expected
		}
		since, seeded := n.expectedSince[robot]
		if !seeded {
			n.expectedSince[robot] = now // first observation: start the grace window
			continue
		}
		if _, seen := n.everSeen[robot]; seen {
			continue // feed established at least once
		}
		if now.Sub(since) > n.feedGrace {
			report[zoneName] = append(report[zoneName], robot)
		}
	}
	for zoneName := range report {
		sort.Strings(report[zoneName])
	}
	return report
}

// view returns the current config snapshot (never nil after New).
func (n *Node) view() *configView { return n.cfg.Load() }

// Namespace returns the namespace this node serves (stable across config swaps).
func (n *Node) Namespace() string { return n.view().cfg.Namespace }

func (n *Node) clock() time.Time {
	if n.now != nil {
		return n.now()
	}
	return time.Now()
}

// conn is one adapter's EdgeStream connection.
type conn struct {
	mu     sync.Mutex
	stream fav1.EdgeService_EdgeStreamServer
}

func (c *conn) send(m *fav1.EdgeControlMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stream.Send(m)
}

// EdgeStream terminates one adapter's EdgeStream: it evaluates PositionFrames for
// boundary breaches and routes EstopAcks to the confirmed-estop tracker.
func (n *Node) EdgeStream(stream fav1.EdgeService_EdgeStreamServer) error {
	ctx := stream.Context()
	c := &conn{stream: stream}
	n.register(c)
	defer n.deregister(c)

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch m := msg.GetMsg().(type) {
		case *fav1.AdapterEdgeMessage_Position:
			n.onPosition(ctx, c, m.Position)
		case *fav1.AdapterEdgeMessage_EstopAck:
			n.routeAck(m.EstopAck)
		}
	}
}

// onPosition evaluates one pose for a boundary breach. A breach is confirmed from
// the position data; absence of a position is never a breach.
func (n *Node) onPosition(ctx context.Context, c *conn, frame *fav1.PositionFrame) {
	robotID := frame.GetRobotId()
	v := n.view()
	zoneName, known := v.cfg.RobotZone[robotID]
	if !known {
		return // unknown/forged robot: the edge cannot act on it
	}
	// The feed exists: this robot's adapter established its EdgeStream, so it can
	// never be reported as feed-unavailable (§9.2.10).
	n.markSeen(robotID)
	zp, ok := v.zoneByName[zoneName]
	if !ok {
		return // no cached polygon for the assigned zone → cannot evaluate; no estop
	}
	pos := frame.GetPosition()
	if pos == nil || pos.X == nil || pos.Y == nil {
		return // no coordinates → cannot confirm a breach → no estop (absence ≠ breach)
	}

	inside := pos.GetFloor() == zp.Floor && zone.PointInPolygon(pos.GetX(), pos.GetY(), zp.Polygon)
	if inside {
		return
	}
	// Confirmed out-of-zone position: issue a headless estop. Run it off the
	// receive loop so the loop can still deliver the confirming EstopAck.
	go n.triggerEstop(ctx, c, robotID, fmt.Sprintf("zone-boundary-breach: robot %s outside zone %s", robotID, zoneName))
}

// TriggerLocalEstop issues a zone-wide headless estop on every connected stream —
// the path for a hardware safety input (light curtain, e-stop button, safety PLC)
// wired to the edge node (§9.6.2.5).
func (n *Node) TriggerLocalEstop(ctx context.Context, reason string) {
	n.mu.Lock()
	conns := make([]*conn, 0, len(n.conns))
	for c := range n.conns {
		conns = append(conns, c)
	}
	n.mu.Unlock()
	for _, c := range conns {
		go n.triggerEstop(ctx, c, "", "local-safety-input: "+reason)
	}
}

// triggerEstop pushes an Estop down a stream and resolves its CONFIRMED outcome.
// It records Stopped ONLY on an EstopAck.state=STOPPED; a dropped ack, silence, an
// adapter FAILED, or STOPPING-without-STOPPED all resolve to Failed — never
// Stopped. Idempotent per robot: a breach already estopped is not re-issued.
func (n *Node) triggerEstop(ctx context.Context, c *conn, robotID, reason string) {
	if robotID != "" {
		n.mu.Lock()
		if n.issued[robotID] {
			n.mu.Unlock()
			return
		}
		n.issued[robotID] = true
		n.mu.Unlock()
	}

	estopID := n.nextEstopID(robotID)
	ch := make(chan *fav1.EstopAck, 4)
	n.setPending(estopID, ch)
	defer n.clearPending(estopID)

	sentAt := n.clock()
	if err := c.send(&fav1.EdgeControlMessage{Msg: &fav1.EdgeControlMessage_Estop{Estop: &fav1.Estop{
		EstopId: estopID, Reason: reason, IssuedBy: "edge:" + n.Namespace(), IssuedAtMs: sentAt.UnixMilli(),
	}}}); err != nil {
		n.recordEstop(robotID, reason, fleetv1.RobotEstopFailed, false)
		return
	}

	state, confirmed := n.awaitConfirmed(ctx, ch)
	n.recordEstop(robotID, reason, state, confirmed)
}

// awaitConfirmed applies the §9.6.2.3 confirmed-only discipline. STOPPED is
// returned only on an explicit EstopAck.state=STOPPED; every other path (no ack,
// FAILED, or STOPPING with no STOPPED in the window) returns Failed.
func (n *Node) awaitConfirmed(ctx context.Context, ch chan *fav1.EstopAck) (fleetv1.RobotEstopState, bool) {
	var first *fav1.EstopAck
	select {
	case first = <-ch:
	case <-time.After(n.deliveryTimeout):
		return fleetv1.RobotEstopFailed, false // dropped estop — never Stopped
	case <-ctx.Done():
		return fleetv1.RobotEstopFailed, false
	}

	switch first.GetState() {
	case fav1.EstopState_ESTOP_STATE_STOPPED:
		return fleetv1.RobotEstopStopped, true
	case fav1.EstopState_ESTOP_STATE_FAILED:
		return fleetv1.RobotEstopFailed, false
	}

	deadline := time.After(n.confirmTimeout)
	for {
		select {
		case ack := <-ch:
			switch ack.GetState() {
			case fav1.EstopState_ESTOP_STATE_STOPPED:
				return fleetv1.RobotEstopStopped, true
			case fav1.EstopState_ESTOP_STATE_FAILED:
				return fleetv1.RobotEstopFailed, false
			}
		case <-deadline:
			return fleetv1.RobotEstopFailed, false // no confirmation → never assumed stopped
		case <-ctx.Done():
			return fleetv1.RobotEstopFailed, false
		}
	}
}

// recordEstop appends the headless estop to the tamper-evident local audit log
// (§9.6.5), for later sync to the control plane when connectivity resumes.
func (n *Node) recordEstop(robotID, reason string, state fleetv1.RobotEstopState, confirmed bool) {
	if n.audit == nil {
		return
	}
	detail := map[string]string{
		"reason":    reason,
		"state":     string(state),
		"confirmed": fmt.Sprintf("%t", confirmed),
		"source":    "edge-node",
	}
	if robotID != "" {
		detail["robot"] = robotID
	}
	ns := n.Namespace()
	_, _ = n.audit.Record(audit.Entry{
		Namespace: ns,
		EventType: eventEdgeEstop,
		Action:    "estop-trigger",
		Outcome:   audit.OutcomeAllowed, // issuing the estop is always an allowed safety action
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "edge:" + ns},
		Resource:  audit.Resource{Kind: "Robot", Namespace: ns, Name: robotID},
		Detail:    detail,
	})
}

// ── registry / correlation ──────────────────────────────────────────────────

func (n *Node) register(c *conn) {
	n.mu.Lock()
	n.conns[c] = struct{}{}
	n.mu.Unlock()
}

func (n *Node) deregister(c *conn) {
	n.mu.Lock()
	delete(n.conns, c)
	n.mu.Unlock()
}

func (n *Node) nextEstopID(robotID string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.idc++
	return fmt.Sprintf("edge-estop-%s-%d", robotID, n.idc)
}

func (n *Node) setPending(id string, ch chan *fav1.EstopAck) {
	n.mu.Lock()
	n.pending[id] = ch
	n.mu.Unlock()
}

func (n *Node) clearPending(id string) {
	n.mu.Lock()
	delete(n.pending, id)
	n.mu.Unlock()
}

func (n *Node) routeAck(ack *fav1.EstopAck) {
	if ack == nil {
		return
	}
	n.mu.Lock()
	ch := n.pending[ack.GetEstopId()]
	n.mu.Unlock()
	if ch != nil {
		select {
		case ch <- ack:
		default:
		}
	}
}
