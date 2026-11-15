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

// Package kubeclientmetrics exports how long the manager's Kubernetes API requests wait on
// their client-side rate limiter. It is not part of the RFC-0001 metrics contract
// (internal/metrics holds that set); like controller-runtime's rest_client_requests_total, it
// describes the manager's own client.
package kubeclientmetrics

import (
	"context"
	"net/url"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/prometheus/client_golang/prometheus"
	clientgometrics "k8s.io/client-go/tools/metrics"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RateLimiterDuration is the time a request waited on its client-side rate limiter, by verb
// and by the request's path template. client-go writes the namespace and the object name of a
// path as {namespace} and {name}, so the path names the resource kind and subresource, and the
// set of label values stays bounded.
//
// Each client and resource kind has a limiter of its own (cmd/manager, --kube-api-qps and
// --kube-api-burst). The path names the kind, not the client: a list of Robots may have waited
// on the informer's limiter or on the uncached reader's, and one limiter's waits can appear
// under several paths (a Robot's writes and its status writes share one).
//
// A request whose context deadline falls before its turn is refused at once, takes no token,
// and is recorded here as a wait of about 0 s; the caller gets "client rate limiter Wait
// returned an error". So under the heaviest throttling the recorded waits fall: read them with
// those errors, which no HTTP request follows and rest_client_requests_total does not count.
//
// The name is not Kubernetes' rest_client_rate_limiter_duration_seconds, which is labelled by
// verb and host, so a dashboard built for that metric does not read this one by mistake.
var RateLimiterDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "rest_client_rate_limiter_wait_seconds",
	Help: "Time a Kubernetes API request of the manager waited on its client-side rate limiter, " +
		"by verb and path template. Each client and resource kind has its own limiter " +
		"(--kube-api-qps, --kube-api-burst). A request its deadline refused records about 0 s.",
	// Seven buckets, not Kubernetes' twelve: this metric has a label set per verb and path, so
	// each bucket costs a series per kind the manager touches. They separate no wait (10 ms), a
	// few tokens at the default QPS (100 ms), a backlog (0.5 s to 4 s), and waits near and past
	// a request's deadline (15 s, 60 s).
	Buckets: []float64{0.01, 0.1, 0.5, 1, 4, 15, 60},
}, []string{"verb", "path"})

// UncachedReadDuration is how long each uncached read of the manager took, by the part of the
// manager that made it: the wait on the client-side rate limiter plus the request itself. Its
// count is the number of reads. rest_client_rate_limiter_wait_seconds says how long a resource
// kind's limiter made requests wait; this says whose reads they were, so a measurement can tell
// what each caller costs and what a change to one caller saved. Only reads made through Reader
// are counted.
var UncachedReadDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "swarmada_manager_uncached_read_duration_seconds",
	Help: "Time an uncached read of the Kubernetes API by the manager took, rate-limiter wait " +
		"included, by the caller that made it, the verb (get, list), the object's Go type and the " +
		"result (ok, notfound, error). A read its deadline refused at the rate limiter is an error " +
		"of about 0 s.",
	Buckets: []float64{0.01, 0.1, 0.5, 1, 4, 15, 60},
}, []string{"caller", "verb", "resource", "result"})

// Reader returns r with each Get and List recorded in UncachedReadDuration under caller. caller
// is a fixed name for a part of the manager (a constant, never a value from a request), so the
// label set stays bounded. A nil r gives nil, so a component's "no uncached reader" stays nil.
func Reader(r client.Reader, caller string) client.Reader {
	if r == nil {
		return nil
	}
	return observedReader{r: r, caller: caller}
}

type observedReader struct {
	r      client.Reader
	caller string
}

// Get reads through the wrapped reader and records the read under the caller.
func (o observedReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	start := time.Now()
	err := o.r.Get(ctx, key, obj, opts...)
	UncachedReadDuration.WithLabelValues(o.caller, "get", typeName(obj), result(err)).Observe(time.Since(start).Seconds())
	return err
}

// List reads through the wrapped reader and records the read under the caller.
func (o observedReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	start := time.Now()
	err := o.r.List(ctx, list, opts...)
	UncachedReadDuration.WithLabelValues(o.caller, "list", typeName(list), result(err)).Observe(time.Since(start).Seconds())
	return err
}

// result names a read's outcome: a deadline the rate limiter refused is an error, so a measurement
// can tell it from a fast success.
func result(err error) string {
	switch {
	case err == nil:
		return "ok"
	case apierrors.IsNotFound(err):
		return "notfound"
	default:
		return "error"
	}
}

// typeName is the object's Go type without its package, Robot or FleetZoneList: bounded by the
// types the scheme knows.
func typeName(obj any) string {
	t := reflect.TypeOf(obj)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return ""
	}
	return t.Name()
}

// Install registers RateLimiterDuration and UncachedReadDuration in r and points client-go's rate-limiter hook at it.
// client-go's metrics.Register runs once per process, and controller-runtime's metrics package
// has already called it from its init, so the hook is set directly. Call Install once, before
// the manager builds its clients. It panics on a duplicate registration in r.
func Install(r prometheus.Registerer) {
	r.MustRegister(RateLimiterDuration, UncachedReadDuration)
	clientgometrics.RateLimiterLatency = latency{RateLimiterDuration}
}

type latency struct{ m *prometheus.HistogramVec }

// Observe records one wait. u is the request's URL with its namespace and name replaced by
// templates (client-go's finalURLTemplate); only its path is kept.
func (l latency) Observe(_ context.Context, verb string, u url.URL, d time.Duration) {
	l.m.WithLabelValues(verb, u.Path).Observe(d.Seconds())
}
