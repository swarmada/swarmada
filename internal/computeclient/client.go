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

// Package computeclient is the control plane's client for the Grpc binding's
// wire contract (compute.v1.ComputeRuntimeService,
// the compute design). Unlike
// internal/command, which multiplexes commands DOWN an adapter-initiated
// stream, this package DIALS OUT to an addressable executor — the inverse
// topology (see compute.proto's service-level comment).
package computeclient

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"

	fleetadapterv1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"

	computev1 "github.com/swarmada/swarmada/proto/compute/v1"
)

// DefaultCallTimeout bounds a single RPC so a hung or slow executor cannot
// become a control-plane outage — the same discipline
// internal/controller's wireTimeout applies to fleet_adapter pushes, sized for
// a control-plane-to-cluster-Service hop rather than a NAT'd robot round trip.
const DefaultCallTimeout = 5 * time.Second

// IdempotencyKey derives the idempotency key: identical whether the
// caller is Swarmada's own retryPolicy re-issuing the SAME attempt or the
// transport retrying the SAME request, and DIFFERENT across attempts (each
// requeue is a new attempt), so a genuine retry after a real change is never
// conflated with a duplicate. Shape matches the Http binding's
// `invocation.id` envelope field so a runtime serving both
// bindings dedups identically.
func IdempotencyKey(namespace, action string, runGeneration int64, attempt int32) string {
	return fmt.Sprintf("%s/%s/%d/%d", namespace, action, runGeneration, attempt)
}

// Client is a connection to one resolved compute endpoint address.
type Client struct {
	conn *grpc.ClientConn
	rpc  computev1.ComputeRuntimeServiceClient
}

// Dial opens a connection to a SPECIFIC address — e.g. "10.42.3.17:9443" — a
// resolved Pod/EndpointSlice address for a contract: Service endpoint
// (internal/compute.ResolvePinnedEndpoint), or the resolved address of an
// existing/workload-provisioned App-contract endpoint. It MUST NOT be a
// Kubernetes Service DNS name or ClusterIP/VIP: for contract: Service, dialing
// the VIP would silently reintroduce per-connection load balancing across
// replicas, which is exactly the hazard streamPolicy.pinEndpoint
// exists to prevent. Transport credentials are the caller's choice (opts) —
// this package is transport-agnostic.
func Dial(address string, opts ...grpc.DialOption) (*Client, error) {
	conn, err := grpc.NewClient(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("dialing compute endpoint %q: %w", address, err)
	}
	return &Client{conn: conn, rpc: computev1.NewComputeRuntimeServiceClient(conn)}, nil
}

// Close releases the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

// Activate starts (or idempotently re-acknowledges) a run. The caller MUST
// bound ctx (e.g. context.WithTimeout(ctx, DefaultCallTimeout)) — Activate
// itself applies no timeout, matching internal/controller's convention that
// the CALLER owns the bound (pushAssignAction/pushRenewLease).
func (c *Client) Activate(ctx context.Context, invocation Invocation, parameters map[string]string) (accepted bool, message string, err error) {
	resp, err := c.rpc.Activate(ctx, &computev1.ActivateRequest{
		Invocation: invocation.toProto(),
		Parameters: parameters,
	})
	if err != nil {
		return false, "", fmt.Errorf("activate: %w", err)
	}
	return resp.GetAccepted(), resp.GetMessage(), nil
}

// RenewLease keeps a Service-contract run alive past its resolved lease
// horizon (design Principle 5). A missed renewal is the only way such a run
// ends without an explicit Cancel or a self-reported terminal TaskEvent.
func (c *Client) RenewLease(ctx context.Context, invocationID string, leaseExpiresAt time.Time) (renewed bool, message string, err error) {
	resp, err := c.rpc.RenewLease(ctx, &computev1.RenewLeaseRequest{
		InvocationId:   invocationID,
		LeaseExpiresMs: leaseExpiresAt.UnixMilli(),
	})
	if err != nil {
		return false, "", fmt.Errorf("renew_lease: %w", err)
	}
	return resp.GetRenewed(), resp.GetMessage(), nil
}

// Cancel requests the run stop, reusing fleet_adapter.v1.CancelDisposition.
func (c *Client) Cancel(ctx context.Context, invocationID, reason string) (acknowledged bool, disposition fleetadapterv1.CancelDisposition, message string, err error) {
	resp, err := c.rpc.Cancel(ctx, &computev1.CancelRequest{
		InvocationId: invocationID,
		Reason:       reason,
	})
	if err != nil {
		return false, fleetadapterv1.CancelDisposition_CANCEL_DISPOSITION_UNSPECIFIED, "", fmt.Errorf("cancel: %w", err)
	}
	return resp.GetAcknowledged(), resp.GetDisposition(), resp.GetMessage(), nil
}

// Result returns the run's typed outcome: the single write-once
// terminal ComputeResult for an App-contract run, or the running summary for a
// Service-contract run.
func (c *Client) Result(ctx context.Context, invocationID string) (*computev1.ComputeResult, error) {
	res, err := c.rpc.Result(ctx, &computev1.ResultRequest{InvocationId: invocationID})
	if err != nil {
		return nil, fmt.Errorf("result: %w", err)
	}
	return res, nil
}

// Invocation identifies one dispatch attempt (compute.v1.Invocation).
type Invocation struct {
	ID            string
	RunGeneration uint64
	Attempt       uint32
	DeadlineMs    int64
}

func (inv Invocation) toProto() *computev1.Invocation {
	return &computev1.Invocation{
		Id:            inv.ID,
		RunGeneration: inv.RunGeneration,
		Attempt:       inv.Attempt,
		DeadlineMs:    inv.DeadlineMs,
	}
}

// ── StreamStatus ──────────────────────────────────────────────────────────────

// EventStream is one open StreamStatus server-stream. Recv returns the next
// TaskEvent, io.EOF when the runtime closes the stream normally, or a transport
// error. The caller owns the ctx passed to StreamStatus and closes the stream by
// cancelling it — there is no Close method, because a server-stream's lifetime
// IS its context's and a second way to end it would be a second thing to get
// wrong.
type EventStream interface {
	Recv() (*fleetadapterv1.TaskEvent, error)
}

// StreamStatus opens the run's server-stream of semantic events, resuming after
// sinceSeq.
//
// sinceSeq IS NOT A TUNABLE. It MUST be the highest seq the control plane has
// ever accepted for this run (FleetAction.status.eventSeqHigh), never the oldest
// entry still retained in status.events[]. The two differ the moment retention
// evicts anything, and asking from the retained window's floor would replay
// events already evicted, evict newer ones to make room, and ask for them again
// on the next reconnect — a resume that cannot converge. Asking from the
// high-water mark makes retention a bounded, permanent loss instead, which is
// what status.events[]' godoc states a consumer is not guaranteed against.
//
// 0 means "from the start" on the wire, and is correct for a first connection
// precisely because seq is documented as monotonic from the run's beginning: an
// action that has accepted nothing has an eventSeqHigh of 0 and wants
// everything.
//
// Unlike this package's unary calls, the caller MUST NOT bound ctx with
// DefaultCallTimeout: a status stream is open for the run's duration by design,
// and a 5-second deadline would tear it down mid-run. The caller bounds it by
// the run's own lifetime (cancel on terminal, on lease loss, or on shutdown).
func (c *Client) StreamStatus(ctx context.Context, invocationID string, sinceSeq uint64) (EventStream, error) {
	stream, err := c.rpc.StreamStatus(ctx, &computev1.StreamStatusRequest{
		InvocationId: invocationID,
		SinceSeq:     sinceSeq,
	})
	if err != nil {
		return nil, fmt.Errorf("stream_status: %w", err)
	}
	return stream, nil
}
