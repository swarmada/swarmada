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

package controller

// The result and artifact surfaces — exercised against a REAL API
// server (envtest), not the fake client, for the same reason
// fleettask_cel_envtest_test.go gives: a schema claim that silently stopped
// being enforced (a typo, a dropped regeneration) looks identical to a
// correct one under a fake client that performs no admission at all.

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func mkPlainAction(name, ns string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
	}
}

// THE UC-C SCENARIO: a result row lands
// with a capture-time pose and an adapter-clock timestamp, referencing a
// declared ArtifactStore — not a raw path — and it round-trips through a
// real API server unchanged.
func TestEnvtest_ActionResult_UCCRowLandsWithPoseAndCaptureTimestamp(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	action := mkPlainAction("plate-read", ns)
	if err := envK8s.Create(ctx, action); err != nil {
		t.Fatalf("create action: %v", err)
	}

	// capturedAt (when the observation happened) is realistically several
	// seconds BEFORE producedAt (when the executor finished processing it) —
	// design's own UC-C example shows an 18:03:11 capture vs an 18:03:16
	// result. Kept clearly apart here (not just "now" for both) so the
	// round-trip assertion below tests the real distinction, not sub-second
	// timestamp jitter. Truncated to whole seconds: metav1.Time round-trips
	// through the API server at second precision (RFC3339), so comparing
	// sub-second values after a real Get would be comparing against
	// server-truncated data.
	capturedAt := metav1.NewTime(time.Now().Add(-5 * time.Second).Truncate(time.Second))
	producedAt := metav1.NewTime(time.Now().Truncate(time.Second))
	action.Status.Phase = fleetv1.ActionPhaseSucceeded
	action.Status.Result = &fleetv1.ActionResult{
		SchemaRef: "plate-reader",
		Values:    map[string]string{"plate": "RXD0430", "confidence": "0.754"},
		Artifacts: []fleetv1.ActionResultArtifact{{
			Name:       "frame",
			Store:      "evidence",
			Path:       "yard-west/2026-09-01T18-03-11Z.mp4#frame-0",
			MediaType:  "image/jpeg",
			Digest:     "sha256:affa48fa682b32767e6ed440696b3a5a598e672490c68a70bee9199e55e22ff6",
			CapturedAt: &capturedAt,
			Pose:       &fleetv1.ArtifactPose{X: 12.4, Y: 3.1, Theta: 1.57, Frame: "map"},
		}},
		ProducedBy: &fleetv1.ResultProducer{
			ImplementationName: "plate-reader-stub", ImplementationVersion: "0.1.0",
			ModelName: "plate-ocr", ModelVersion: "2.1", ModelDigest: "sha256:9c1e0000000000000000000000000000000000000000000000000000000000",
		},
		ProducedAt: &producedAt,
	}
	if err := envK8s.Status().Update(ctx, action); err != nil {
		t.Fatalf("writing result: %v", err)
	}

	var got fleetv1.FleetAction
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "plate-read"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Result == nil || len(got.Status.Result.Artifacts) != 1 {
		t.Fatalf("result did not round-trip: %+v", got.Status.Result)
	}
	art := got.Status.Result.Artifacts[0]
	if art.Store != "evidence" || art.Path == "" {
		t.Fatalf("artifact must reference a declared store + path, got store=%q path=%q", art.Store, art.Path)
	}
	if art.Digest == "" {
		t.Fatal("digest must round-trip — it is REQUIRED whatever the backing")
	}
	if art.CapturedAt == nil || !art.CapturedAt.Time.Equal(capturedAt.Time) {
		t.Fatalf("capture-time timestamp did not round-trip: got %v want %v", art.CapturedAt, capturedAt)
	}
	if art.Pose == nil || art.Pose.X != 12.4 || art.Pose.Y != 3.1 || art.Pose.Theta != 1.57 || art.Pose.Frame != "map" {
		t.Fatalf("pose did not round-trip: %+v", art.Pose)
	}
	if got.Status.Result.ProducedBy == nil || got.Status.Result.ProducedBy.ModelVersion != "2.1" {
		t.Fatalf("producedBy did not round-trip: %+v", got.Status.Result.ProducedBy)
	}
	// capturedAt (when the observation happened) is distinct from producedAt
	// (when the executor finished processing it) — both must survive as
	// separately meaningful timestamps, not collapsed into one.
	if got.Status.Result.ProducedAt == nil || got.Status.Result.ProducedAt.Time.Equal(art.CapturedAt.Time) {
		t.Fatal("producedAt must be tracked separately from capturedAt")
	}
}

// THE CRITICAL ONE: a task naming a raw path — no
// declared store — is rejected. There is no field for a raw path at all:
// `store` is required, so the closest an author can come to "a raw path" is
// an artifact with `path` set and `store` omitted, which the schema refuses
// outright.
func TestEnvtest_ActionResultArtifact_RawPathRejected(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	action := mkPlainAction("raw-path-action", ns)
	if err := envK8s.Create(ctx, action); err != nil {
		t.Fatalf("create action: %v", err)
	}

	action.Status.Result = &fleetv1.ActionResult{
		Artifacts: []fleetv1.ActionResultArtifact{{
			Name: "frame",
			// Store deliberately omitted — this is what "naming a raw path"
			// looks like: Path/URI without a declared ArtifactStore.
			Path:   "/var/evidence/f7.jpg",
			Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000",
		}},
	}
	err := envK8s.Status().Update(ctx, action)
	if err == nil {
		t.Fatal("the API server accepted an artifact reference with no declared store — " +
			"a bare path is not a reference; two consumers mounting different " +
			"volumes would resolve it to different bytes or none")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error (missing required field 'store'), got: %v", err)
	}
}

// digest is required whatever the backing — an artifact naming a store but
// no digest is equally rejected ("digest is required whatever
// the backing... the only part that survives the artifact being copied").
func TestEnvtest_ActionResultArtifact_MissingDigestRejected(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	action := mkPlainAction("no-digest-action", ns)
	if err := envK8s.Create(ctx, action); err != nil {
		t.Fatalf("create action: %v", err)
	}

	action.Status.Result = &fleetv1.ActionResult{
		Artifacts: []fleetv1.ActionResultArtifact{{Name: "frame", Store: "evidence", Path: "f7.jpg"}},
	}
	if err := envK8s.Status().Update(ctx, action); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error (missing required field 'digest'), got: %v", err)
	}
}

// The values bound is normative: each entry over 256 bytes is rejected
// (the 16*256=4096-byte derivation).
func TestEnvtest_ActionResult_OversizedValueRejected(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	action := mkPlainAction("oversized-value-action", ns)
	if err := envK8s.Create(ctx, action); err != nil {
		t.Fatalf("create action: %v", err)
	}

	huge := make([]byte, 300)
	for i := range huge {
		huge[i] = 'x'
	}
	action.Status.Result = &fleetv1.ActionResult{Values: map[string]string{"note": string(huge)}}
	if err := envK8s.Status().Update(ctx, action); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error (a 300-byte value exceeds the 256-byte-per-entry bound), got: %v", err)
	}
}
