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

package leadership

import (
	"context"
	"time"

	"github.com/go-logr/logr"
)

// TermSource issues the term for one acquisition of leadership. *Floor is the production
// source.
type TermSource interface {
	Next(ctx context.Context) (uint64, error)
}

// defaultRetry is how long Runnable waits before asking TermSource again after a failure.
const defaultRetry = 2 * time.Second

// Runnable takes leadership for State. The manager starts it only on the elected leader
// (NeedLeaderElection), and at once when leader election is off. It issues a term, records it
// in State, and holds it until its context is cancelled, which the manager does when it stops,
// and when the election Lease is lost; it then releases State, which closes every stream
// opened under that leadership.
//
// Until a term is issued, State does not lead: streams are refused and no Command is sent. A
// term source that keeps failing is retried, and logged each time, rather than leading
// without a term.
type Runnable struct {
	State *State
	Terms TermSource
	Log   logr.Logger
	// Retry is the wait between failed attempts to issue a term. Zero means defaultRetry.
	Retry time.Duration
}

// NeedLeaderElection reports true: only the elected leader leads.
func (r *Runnable) NeedLeaderElection() bool { return true }

// Start issues a term, leads under it until ctx is cancelled, and then gives leadership up.
func (r *Runnable) Start(ctx context.Context) error {
	retry := r.Retry
	if retry == 0 {
		retry = defaultRetry
	}
	var term uint64
	for {
		t, err := r.Terms.Next(ctx)
		if err == nil && r.State.Acquire(t) {
			term = t
			break
		}
		if err == nil {
			r.Log.Error(nil, "the leader term issued is not above the term this process last held; "+
				"not leading, retrying", "issued", t)
		} else {
			r.Log.Error(err, "could not issue a leader term; not leading: streams are refused and no "+
				"command is sent until one is issued", "retryIn", retry.String())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
	r.Log.Info("leading: serving adapter streams and stamping commands with the leader term", "leaderTerm", term)
	<-ctx.Done()
	r.State.Release()
	r.Log.Info("leadership given up: adapter streams closed, no command is sent", "leaderTerm", term)
	return nil
}
