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

package controlstream

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Leadership is this process's hold on leadership, as internal/leadership.State reports it.
//
// A control-plane process sends on an adapter's streams only while it holds leadership, and
// closes them when it loses it (RFC-0001 #safety-robot-stop-table). The gRPC server runs on
// every replica (GRPCRunnable.NeedLeaderElection), so the rule is kept per stream: a
// ControlStream or SafetyStream opened on a process that does not lead is refused, and a
// stream opened while leading is closed when that leadership ends.
type Leadership interface {
	// Leading reports whether this process holds leadership and, when it does, a channel
	// that is closed when it loses it.
	Leading() (lost <-chan struct{}, leading bool)
}

// lead is the gate at the opening of every stream. It returns the channel that closes when
// this process stops leading, or the status the stream is refused with. With no Leadership
// wired every stream is served and the channel is nil, which never fires.
func (s *Server) lead() (<-chan struct{}, error) {
	if s.Leadership == nil {
		return nil, nil
	}
	lost, leading := s.Leadership.Leading()
	if !leading {
		return nil, notLeaderStatus()
	}
	return lost, nil
}

// notLeaderStatus refuses a stream opened on a process that does not hold leadership. It is
// Unavailable, which an adapter retries: the leader serves the stream.
func notLeaderStatus() error {
	return status.Error(codes.Unavailable,
		"not the leader: this control-plane process does not hold leadership and serves no adapter stream; "+
			"connect to the leader")
}

// leadershipLostStatus closes a stream whose process has lost leadership. It is Unavailable
// for the same reason.
func leadershipLostStatus() error {
	return status.Error(codes.Unavailable,
		"leadership lost: this control-plane process no longer holds leadership and has closed the stream; "+
			"connect to the leader")
}

// stillLeading reports whether this process holds leadership now. With no Leadership wired it
// always does. A deposed process marks no robot ready for work (RegisterNotifier).
func (s *Server) stillLeading() bool {
	if s.Leadership == nil {
		return true
	}
	_, leading := s.Leadership.Leading()
	return leading
}
