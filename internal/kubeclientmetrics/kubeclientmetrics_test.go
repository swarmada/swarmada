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

package kubeclientmetrics

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type stubReader struct{ err error }

func (s stubReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return s.err
}

func (s stubReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return s.err
}

func count(t *testing.T, caller, verb, resource, result string) uint64 {
	t.Helper()
	h, ok := UncachedReadDuration.WithLabelValues(caller, verb, resource, result).(prometheus.Histogram)
	if !ok {
		t.Fatal("UncachedReadDuration is not a histogram")
	}
	m := &dto.Metric{}
	if err := h.Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// Each Get and List made through Reader is recorded under its caller, verb and the object's
// type, whether it succeeds or not, and the read's own result is returned unchanged.
func TestReader_RecordsEachReadByCallerVerbAndType(t *testing.T) {
	failed := errors.New("refused")
	r := Reader(stubReader{err: failed}, "test_caller")
	gets, lists := count(t, "test_caller", "get", "Pod", "error"), count(t, "test_caller", "list", "PodList", "error")

	if err := r.Get(context.Background(), client.ObjectKey{Name: "p"}, &corev1.Pod{}); !errors.Is(err, failed) {
		t.Fatalf("Get returned %v, want the reader's own error", err)
	}
	if err := r.List(context.Background(), &corev1.PodList{}); !errors.Is(err, failed) {
		t.Fatalf("List returned %v, want the reader's own error", err)
	}
	if d := count(t, "test_caller", "get", "Pod", "error") - gets; d != 1 {
		t.Errorf("recorded %d gets, want 1", d)
	}
	if d := count(t, "test_caller", "list", "PodList", "error") - lists; d != 1 {
		t.Errorf("recorded %d lists, want 1", d)
	}
}

// The result label tells a success, a missing object and a failure apart.
func TestReader_RecordsTheResult(t *testing.T) {
	notFound := apierrors.NewNotFound(corev1.Resource("pods"), "p")
	for _, tc := range []struct {
		err  error
		want string
	}{{nil, "ok"}, {notFound, "notfound"}, {errors.New("client rate limiter Wait returned an error"), "error"}} {
		before := count(t, "test_result", "get", "Pod", tc.want)
		_ = Reader(stubReader{err: tc.err}, "test_result").Get(context.Background(), client.ObjectKey{Name: "p"}, &corev1.Pod{})
		if d := count(t, "test_result", "get", "Pod", tc.want) - before; d != 1 {
			t.Errorf("a read returning %v recorded %d under result=%s, want 1", tc.err, d, tc.want)
		}
	}
}

// A nil reader stays nil, so a component's "no uncached reader" keeps its meaning.
func TestReader_OfNilIsNil(t *testing.T) {
	if Reader(nil, "test_caller") != nil {
		t.Fatal("Reader(nil) is not nil")
	}
}
