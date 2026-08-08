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

// Package tde implements the Traffic Deconfliction Engine (RFC-0001 §9.4; the
// commit-site reservation gate, ADR-0009): the
// synchronous blocking gate the Scheduler must pass before committing a action
// assignment (zone-capacity enforcement), and the reservation manager for shared
// physical resources.
//
// It is the SINGLE authority for zone capacity and shared-resource reservations
// (TDE-4) and runs on the control-plane leader. Reservation state is held
// authoritatively in-process under per-zone locks — which is what makes
// check-and-grant atomic and free of double-grants under concurrent requests —
// and mirrored to FleetZone.status (durable, operator-visible, TDE-6), from which
// it is rebuilt on restart (§9.4.7).
package tde

import (
	"context"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ReservationStatus is the outcome of a RequestReservation call.
type ReservationStatus string

// ReservationStatus values (§9.4.3).
const (
	// Granted: capacity/resources available; a Reserved entry was created.
	Granted ReservationStatus = "Granted"
	// Denied: not available; the Scheduler returns the action to Pending.
	Denied ReservationStatus = "Denied"
	// PreemptedGranted: granted by preempting a lower-priority reservation.
	PreemptedGranted ReservationStatus = "PreemptedGranted"
)

// Denial reasons (ReservationResult.DeniedReason).
const (
	DeniedZoneCapacity        = "zone_capacity"
	DeniedResourceUnavailable = "resource_unavailable"
	DeniedTDEUnavailable      = "tde_unavailable"
)

// ReservationRequest is a Scheduler (zone-capacity) or adapter (shared-resource)
// reservation request (§9.4.3).
type ReservationRequest struct {
	ActionID   string
	RobotID    string
	Namespace  string
	TargetZone string // leaf zone for capacity; empty skips the zone-capacity gate
	Priority   fleetv1.ActionPriority
	Resources  []ResourceRequest
}

// ResourceRequest names a shared resource to reserve (§9.4.3).
type ResourceRequest struct {
	ResourceName string
	ZoneName     string
	// EstimatedDurationSeconds drives PriorityWithDuration (SJF within band);
	// nil → the resource's durationFallback applies.
	EstimatedDurationSeconds *int32
}

// ReservationResult is returned synchronously to the caller (§9.4.3).
type ReservationResult struct {
	Status             ReservationStatus
	ExpiresAt          time.Time
	DeniedReason       string
	RetryAfter         time.Duration
	PreemptedActionIDs []string
}

// ZoneReservationStatus is a read-only snapshot of a zone's reservation state.
type ZoneReservationStatus struct {
	Occupied int
	Reserved int
	Max      int32
}

// TrafficDeconflictionEngine is the synchronous blocking gate between the
// Scheduler and action commitment (§9.4.3). A third-party scheduler MUST NOT bypass
// it by writing FleetAction.status.assignedRobot directly (TDE-1).
type TrafficDeconflictionEngine interface {
	// RequestReservation checks zone capacity and shared-resource availability and
	// grants, denies, or preempts. It MUST be safe for concurrent use.
	RequestReservation(ctx context.Context, req ReservationRequest) (ReservationResult, error)
	// ReleaseReservation drops a action's reservation and all its resource holds.
	ReleaseReservation(ctx context.Context, namespace, zone, actionID string) error
	// OnRobotEnteredZone transitions a Reserved entry to Occupied.
	OnRobotEnteredZone(ctx context.Context, namespace, zone, robotID string) error
	// OnRobotExitedZone releases an Occupied entry on zone exit.
	OnRobotExitedZone(ctx context.Context, namespace, zone, robotID string) error
	// OnActionPhaseChanged extends a Reserved TTL on Revoking and releases on a
	// terminal phase.
	OnActionPhaseChanged(ctx context.Context, namespace, zone, actionID string, phase fleetv1.ActionPhase) error
	// ZoneStatus returns a read-only capacity snapshot.
	ZoneStatus(ctx context.Context, namespace, zone string) (ZoneReservationStatus, error)
}

// Config carries the TDE tunables (SwarmadaConfig.spec.trafficDeconfliction,
// §9.1.11.10). The zero value is unusable; use [DefaultConfig].
type Config struct {
	ReservationTTL             time.Duration
	DisconnectedReservationTTL time.Duration
	ClockSkew                  time.Duration
}

// DefaultConfig returns the spec defaults: 120s reservation TTL, 360s
// disconnected TTL.
func DefaultConfig() Config {
	return Config{
		ReservationTTL:             120 * time.Second,
		DisconnectedReservationTTL: 360 * time.Second,
		ClockSkew:                  2 * time.Second,
	}
}

// ConfigFromTDE maps SwarmadaConfig.spec.trafficDeconfliction TTLs onto a TDE
// [Config] (§9.1.11.10). A zero (absent) field falls back to the DefaultConfig
// value per-field, so an unreadable tunable never zeroes a TTL; ClockSkew has no
// CRD surface and keeps its default. It is the single translation from the CRD
// surface to the engine's tunables.
func ConfigFromTDE(reservationTTLSeconds, disconnectedReservationTTLSeconds int32) Config {
	cfg := DefaultConfig()
	if reservationTTLSeconds > 0 {
		cfg.ReservationTTL = time.Duration(reservationTTLSeconds) * time.Second
	}
	if disconnectedReservationTTLSeconds > 0 {
		cfg.DisconnectedReservationTTL = time.Duration(disconnectedReservationTTLSeconds) * time.Second
	}
	return cfg
}

// isPreemptibleBand reports whether a reservation's band may be displaced by a
// PREEMPTOR-band action (Critical or High — see ActionPriority.CanPreempt, used by
// Engine.reserve). It matches the §C controller preemption rule: the VICTIM set is
// Normal/Low only, so a preemptor never evicts another Critical/High.
func isPreemptibleBand(p fleetv1.ActionPriority) bool {
	return p == fleetv1.ActionPriorityNormal || p == fleetv1.ActionPriorityLow
}

// priorityRank maps a band to its numeric rank (Critical=1 … Low=4). Higher =
// lower priority. Empty defaults to Normal.
func priorityRank(p fleetv1.ActionPriority) int {
	switch p {
	case fleetv1.ActionPriorityCritical:
		return 1
	case fleetv1.ActionPriorityHigh:
		return 2
	case fleetv1.ActionPriorityLow:
		return 4
	default:
		return 3
	}
}
