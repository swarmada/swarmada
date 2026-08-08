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

package edge

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// FeedReporter periodically writes each edge-node FleetZone's
// status.edgeFeedUnavailable from the Node's never-seen-beyond-grace detection, so
// the control-plane Zone Controller can emit an EdgeFeedUnavailable Warning while an
// adapter serving a zone's robots has no established EdgeStream (§9.2.10). It
// requires RBAC to update fleetzones/status in its namespace. It is best-effort: a
// write error is logged and retried on the next tick, and it never touches the
// headless-estop safety path — a control-plane outage only stalls the report.
type FeedReporter struct {
	node      *Node
	writer    client.Client
	namespace string
	interval  time.Duration
	log       logr.Logger
}

// NewFeedReporter builds a reporter. A non-positive interval uses DefaultSyncInterval.
func NewFeedReporter(node *Node, writer client.Client, namespace string, interval time.Duration, log logr.Logger) *FeedReporter {
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	return &FeedReporter{node: node, writer: writer, namespace: namespace, interval: interval, log: log}
}

// Run reports every interval until ctx is cancelled. It never returns an error for a
// failed report — a failed write is retried on the next tick, not fatal.
func (r *FeedReporter) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.reportOnce(ctx)
		}
	}
}

// reportOnce reconciles each edge zone's status.edgeFeedUnavailable to the current
// missing-feed set. Only a changed set is written (no status write when nothing
// changed), and a recovered zone is written back to empty so the warning clears.
func (r *FeedReporter) reportOnce(ctx context.Context) {
	for zoneName, robots := range r.node.MissingFeeds() {
		fz := &fleetv1.FleetZone{}
		if err := r.writer.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: zoneName}, fz); err != nil {
			r.logger().Info("edge feed report: fetch zone failed", "zone", zoneName, "error", err.Error())
			continue
		}
		if equality.Semantic.DeepEqual(fz.Status.EdgeFeedUnavailable, robots) {
			continue
		}
		orig := fz.DeepCopy()
		fz.Status.EdgeFeedUnavailable = robots
		if err := r.writer.Status().Patch(ctx, fz, client.MergeFrom(orig)); err != nil {
			r.logger().Info("edge feed report: patch status failed", "zone", zoneName, "error", err.Error())
		}
	}
}

func (r *FeedReporter) logger() logr.Logger {
	if r.log.GetSink() == nil {
		return logr.Discard()
	}
	return r.log
}
