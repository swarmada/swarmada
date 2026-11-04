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

package executorguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A robot a stop covers keeps its record whatever proves it off the action, and blocks the
// action from every other robot; a requeue the estop-clear check admitted moves it on proof.
func TestDecide_ACoveringStopKeepsTheRecord(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	covered := Recorder{Robot: "r1", CancelConfirmed: true, Covered: "zone/z"}
	d := Decide(Input{Horizon: &past, Recorders: []Recorder{covered}, Now: now, Skew: time.Second})
	if d.Dispatchable() || len(d.Release) != 0 || d.Stop != "zone/z" || d.Wait != CoveredRecheck {
		t.Errorf("a covered recorder must block and keep its record, whatever proves it off: %+v", d)
	}
	d = Decide(Input{Horizon: &past, Recorders: []Recorder{covered}, Now: now, Skew: time.Second, MoveAuthorized: true})
	if !d.Dispatchable() || len(d.Release) != 1 {
		t.Errorf("an authorized requeue moves a covered recorder on proof: %+v", d)
	}
	d = Decide(Input{Recorders: []Recorder{{Robot: "r1", Covered: "zone/z"}}, Now: now, MoveAuthorized: true})
	if d.Dispatchable() || d.Stop != "" {
		t.Errorf("an authorized requeue still needs proof: %+v", d)
	}
}

// A robot under a stop that is moved by an authorized requeue still needs proof it is off the
// action: a confirmed stop is not one (the robot may take the action up again after its resume).
func TestDecide_AnAuthorizedMoveWaitsForProof(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	d := Decide(Input{Recorders: []Recorder{{Robot: "r1", Covered: "zone/z"}}, Now: now, Skew: time.Second, MoveAuthorized: true})
	if d.Dispatchable() || d.Stop != "" {
		t.Errorf("a covered robot with no acknowledgement and no lease death blocks an authorized move: %+v", d)
	}
}

// A Paused action whose hold released the horizon has no reference: only the adapter's word
// releases its records, never elapsed time from its creation.
func TestReferenceOf_APausedActionWithNoHorizonHasNone(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: created}}
	a.Status.Phase = fleetv1.ActionPhasePaused
	if ref := ReferenceOf(a); !ref.IsZero() {
		t.Errorf("a Paused action with no horizon has no reference, got %s", ref)
	}
	a.Status.Phase = fleetv1.ActionPhasePending
	if ref := ReferenceOf(a); !ref.Equal(created.Time) {
		t.Errorf("a Pending action counts from its creation, got %s", ref)
	}
}

func TestDeleteLosesKey(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	loses, until := DeleteLosesKey("r1", Input{Now: now, Skew: MaxSkew})
	if !loses || !until.IsZero() {
		t.Errorf("no horizon and no reference: only the adapter's word proves it off (%v %s)", loses, until)
	}
	h := now.Add(10 * time.Second)
	loses, until = DeleteLosesKey("r1", Input{Horizon: &h, Now: now, Skew: MaxSkew})
	if !loses || until.Before(h.Add(MaxSkew)) {
		t.Errorf("a live horizon refuses until it plus the widest skew (%v %s)", loses, until)
	}
	if loses, _ := DeleteLosesKey("r1", Input{Horizon: &h, Now: h.Add(MaxSkew), Skew: MaxSkew}); loses {
		t.Errorf("past the horizon plus the widest skew the delete is admitted")
	}
}

// MaxSkew is the CRD's ceiling on clockSkewMarginSeconds, read from the generated CRD.
func TestMaxSkew_IsTheCRDCeiling(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "swarmada.io_swarmadaconfigs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)clockSkewMarginSeconds:.*?maximum: (\d+)`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("no maximum under clockSkewMarginSeconds")
	}
	n, _ := strconv.Atoi(string(m[1]))
	if time.Duration(n)*time.Second != MaxSkew {
		t.Errorf("MaxSkew %s, the CRD ceiling is %d s", MaxSkew, n)
	}
}
