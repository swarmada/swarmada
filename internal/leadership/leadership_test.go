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
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testNS = "swarmada-system"

func TestState_ZeroValueDoesNotLead(t *testing.T) {
	var s State
	if lost, ok := s.Leading(); ok || lost != nil {
		t.Fatalf("zero State leads: lost=%v ok=%v", lost, ok)
	}
	if term, ok := s.Term(); ok || term != 0 {
		t.Fatalf("zero State has a term: %d %v", term, ok)
	}
}

func TestState_ReleaseClosesTheChannelLeadingHandedOut(t *testing.T) {
	var s State
	if !s.Acquire(3) {
		t.Fatal("Acquire(3) refused")
	}
	lost, ok := s.Leading()
	if !ok || lost == nil {
		t.Fatal("not leading after Acquire")
	}
	if term, ok := s.Term(); !ok || term != 3 {
		t.Fatalf("Term = %d,%v, want 3,true", term, ok)
	}
	select {
	case <-lost:
		t.Fatal("lost closed while leading")
	default:
	}
	s.Release()
	select {
	case <-lost:
	default:
		t.Fatal("Release did not close the channel a stream opened under that leadership holds")
	}
	if _, ok := s.Leading(); ok {
		t.Fatal("still leading after Release")
	}
	if _, ok := s.Term(); ok {
		t.Fatal("Term reports leading after Release")
	}
	s.Release() // a second Release is a no-op, not a double close
}

func TestState_ATermIsNeverReused(t *testing.T) {
	var s State
	if s.Acquire(0) {
		t.Fatal("term 0 accepted: 0 on the wire means no term")
	}
	if !s.Acquire(5) {
		t.Fatal("Acquire(5) refused")
	}
	if s.Acquire(6) {
		t.Fatal("a second Acquire while leading was accepted")
	}
	s.Release()
	for _, term := range []uint64{4, 5} {
		if s.Acquire(term) {
			t.Fatalf("Acquire(%d) after holding 5 was accepted: a term is never reused or lowered", term)
		}
	}
	if !s.Acquire(6) {
		t.Fatal("Acquire(6) after holding 5 refused")
	}
}

func leaseScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func electionLease(transitions int32) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "swarmada-leader"},
		Spec:       coordinationv1.LeaseSpec{LeaseTransitions: &transitions},
	}
}

func floorOn(c client.Client) *Floor {
	return &Floor{Reader: c, Writer: c, Namespace: testNS, ElectionLease: "swarmada-leader"}
}

func readFloor(t *testing.T, c client.Client) string {
	t.Helper()
	l := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: FloorLeaseName}, l); err != nil {
		t.Fatalf("reading the floor: %v", err)
	}
	return l.Annotations[AnnotationTermFloor]
}

func TestFloor_TermIsMaxOfFloorAndTransitionsPlusOne(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).WithObjects(electionLease(4)).Build()
	f := floorOn(c)

	term, err := f.Next(ctx)
	if err != nil || term != 5 {
		t.Fatalf("first term = %d, %v; want 5 (no floor, 4 transitions)", term, err)
	}
	if got := readFloor(t, c); got != "5" {
		t.Fatalf("floor = %q, want 5", got)
	}
	// Same transition count (the same holder acquiring again): the floor decides.
	term, err = f.Next(ctx)
	if err != nil || term != 6 {
		t.Fatalf("second term = %d, %v; want 6", term, err)
	}
	// The count overtakes the floor.
	l := electionLease(0)
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	n := int32(20)
	l.Spec.LeaseTransitions = &n
	if err := c.Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	if term, err = f.Next(ctx); err != nil || term != 21 {
		t.Fatalf("term = %d, %v; want 21", term, err)
	}
}

func TestFloor_TermDoesNotFallWhenTheElectionLeaseIsDeleted(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).WithObjects(electionLease(9)).Build()
	f := floorOn(c)
	before, err := f.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, electionLease(0)); err != nil {
		t.Fatal(err)
	}
	after, err := f.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("term fell or repeated after the election Lease was deleted: %d then %d", before, after)
	}
	// Re-created by the next election, its count starts again at 0 (then 1).
	if err := c.Create(ctx, electionLease(1)); err != nil {
		t.Fatal(err)
	}
	again, err := f.Next(ctx)
	if err != nil || again <= after {
		t.Fatalf("term after re-creation = %d, %v; want above %d", again, err, after)
	}
}

func TestFloor_WithoutLeaderElectionTheFloorAloneDecides(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).WithObjects(electionLease(40)).Build()
	f := &Floor{Reader: c, Writer: c, Namespace: testNS}
	for want := uint64(1); want <= 3; want++ {
		if term, err := f.Next(ctx); err != nil || term != want {
			t.Fatalf("term = %d, %v; want %d", term, err, want)
		}
	}
}

func TestFloor_ARacingWriterIsReadAgainNotOverwritten(t *testing.T) {
	ctx := context.Background()
	base := fake.NewClientBuilder().WithScheme(leaseScheme(t)).WithObjects(electionLease(2)).Build()
	if _, err := floorOn(base).Next(ctx); err != nil { // floor 3
		t.Fatal(err)
	}
	raced := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == FloorLeaseName && !raced {
				raced = true
				// Another process raises the floor to 10 between this one's read and its write.
				other := &coordinationv1.Lease{}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), other); err != nil {
					return err
				}
				other.Annotations[AnnotationTermFloor] = "10"
				if err := cl.Update(ctx, other); err != nil {
					return err
				}
			}
			return cl.Update(ctx, obj, opts...)
		},
	})
	term, err := floorOn(c).Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !raced {
		t.Fatal("the race was not run")
	}
	if term != 11 {
		t.Fatalf("term = %d, want 11: the write that lost must be decided again on the floor the other process wrote", term)
	}
	if got := readFloor(t, base); got != "11" {
		t.Fatalf("floor = %q, want 11", got)
	}
}

// Two processes never issuing one term is proved on a real API server
// (internal/controller/leader_term_envtest_test.go): the fake client's Update reads, compares
// and writes the resourceVersion in three steps with no lock across them, so it lets two
// concurrent writers at one resourceVersion both land.

func TestFloor_ANonNumericFloorIsRefusedNotReadAsZero(t *testing.T) {
	ctx := context.Background()
	bad := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: FloorLeaseName,
		Annotations: map[string]string{AnnotationTermFloor: "lots"}}}
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).WithObjects(bad).Build()
	if term, err := floorOn(c).Next(ctx); err == nil {
		t.Fatalf("issued term %d from an unreadable floor", term)
	}
}

func TestFloor_AReadFailureIssuesNoTerm(t *testing.T) {
	ctx := context.Background()
	base := fake.NewClientBuilder().WithScheme(leaseScheme(t)).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewServiceUnavailable("api server down")
		},
	})
	if term, err := floorOn(c).Next(ctx); err == nil {
		t.Fatalf("issued term %d without reading the floor", term)
	}
}

// fakeTerms issues the terms it is given, failing first `fail` times.
type fakeTerms struct {
	mu    sync.Mutex
	fail  int
	terms []uint64
	calls int
}

func (f *fakeTerms) Next(context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail > 0 {
		f.fail--
		return 0, errors.New("api server down")
	}
	t := f.terms[0]
	f.terms = f.terms[1:]
	return t, nil
}

func waitLeading(t *testing.T, s *State, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.Leading(); ok == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("leading never became %v", want)
}

func TestRunnable_LeadsOnlyOnceATermIsIssuedAndReleasesOnCancel(t *testing.T) {
	s := &State{}
	terms := &fakeTerms{fail: 2, terms: []uint64{7}}
	r := &Runnable{State: s, Terms: terms, Log: logr.Discard(), Retry: 10 * time.Millisecond}
	if !r.NeedLeaderElection() {
		t.Fatal("the leadership runnable must run only on the elected leader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	waitLeading(t, s, true)
	if term, _ := s.Term(); term != 7 {
		t.Fatalf("term = %d, want 7", term)
	}
	terms.mu.Lock()
	calls := terms.calls
	terms.mu.Unlock()
	if calls != 3 {
		t.Fatalf("term source called %d times, want 3 (two failures, then a term)", calls)
	}
	lost, _ := s.Leading()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned %v", err)
	}
	select {
	case <-lost:
	default:
		t.Fatal("cancelling the runnable (the manager does on a lost election) did not close the streams' channel")
	}
	if _, ok := s.Leading(); ok {
		t.Fatal("still leading after the runnable stopped")
	}
}

func TestRunnable_ACancelBeforeATermIsIssuedNeverLeads(t *testing.T) {
	s := &State{}
	terms := &fakeTerms{fail: 1 << 30}
	r := &Runnable{State: s, Terms: terms, Log: logr.Discard(), Retry: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Leading(); ok {
		t.Fatal("led without a term")
	}
}

// Guard: the annotation is a plain decimal, readable by a person with kubectl.
func TestFloor_AnnotationIsDecimal(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).Build()
	if _, err := floorOn(c).Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.ParseUint(readFloor(t, c), 10, 64); err != nil {
		t.Fatal(err)
	}
}
