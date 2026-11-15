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

package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	clientgometrics "k8s.io/client-go/tools/metrics"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/kubeclientmetrics"
)

func TestApplyClientBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		qps     float64
		burst   int
		wantErr string
	}{
		{name: "defaults", qps: defaultKubeAPIQPS, burst: defaultKubeAPIBurst},
		{name: "fractional qps", qps: 7.5, burst: 1},
		{name: "the floor", qps: minKubeAPIQPS, burst: 1},
		{name: "the largest float32", qps: math.MaxFloat32, burst: 1},
		{name: "below the floor leader election needs", qps: 4.9, burst: 30, wantErr: "--kube-api-qps"},
		{name: "zero qps", qps: 0, burst: 30, wantErr: "--kube-api-qps"},
		{name: "negative qps would disable the limiter", qps: -1, burst: 30, wantErr: "--kube-api-qps"},
		{name: "NaN qps", qps: math.NaN(), burst: 30, wantErr: "--kube-api-qps"},
		{name: "infinite qps", qps: math.Inf(1), burst: 30, wantErr: "--kube-api-qps"},
		{name: "qps just beyond float32", qps: 3.403e38, burst: 30, wantErr: "--kube-api-qps"},
		{name: "qps beyond float32", qps: math.MaxFloat64, burst: 30, wantErr: "--kube-api-qps"},
		{name: "zero burst would become client-go's 10", qps: 20, burst: 0, wantErr: "--kube-api-burst"},
		{name: "negative burst", qps: 20, burst: -1, wantErr: "--kube-api-burst"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &rest.Config{}
			err := applyClientBudget(cfg, tc.qps, tc.burst)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("applyClientBudget(%v, %d) = %v, want an error naming %s", tc.qps, tc.burst, err, tc.wantErr)
				}
				if cfg.QPS != 0 || cfg.Burst != 0 {
					t.Fatalf("a refused budget was applied: QPS %v, burst %d", cfg.QPS, cfg.Burst)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyClientBudget(%v, %d): %v", tc.qps, tc.burst, err)
			}
			if cfg.QPS != float32(tc.qps) || cfg.Burst != tc.burst {
				t.Fatalf("got QPS %v, burst %d; want %v, %d", cfg.QPS, cfg.Burst, tc.qps, tc.burst)
			}
		})
	}
}

// With leader election on, a QPS too low for the renew deadline is refused: 6 divided by the
// deadline in seconds. At the default 3 s deadline the 5 QPS floor is the stricter one.
func TestCheckRenewalBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		qps      float64
		deadline time.Duration
		wantErr  bool
	}{
		{name: "the floor at the default deadline", qps: minKubeAPIQPS, deadline: 3 * time.Second},
		{name: "the default budget at the default deadline", qps: defaultKubeAPIQPS, deadline: 3 * time.Second},
		{name: "exactly 6 per second at 1s", qps: 6, deadline: time.Second},
		{name: "the floor at 1s", qps: minKubeAPIQPS, deadline: time.Second, wantErr: true},
		{name: "exactly 12 per second at 500ms", qps: 12, deadline: 500 * time.Millisecond},
		{name: "just under 12 per second at 500ms", qps: 11.9, deadline: 500 * time.Millisecond, wantErr: true},
		{name: "a deadline that is not positive is left to leader election", qps: minKubeAPIQPS, deadline: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRenewalBudget(tc.qps, tc.deadline)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("checkRenewalBudget(%v, %v): %v", tc.qps, tc.deadline, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkRenewalBudget(%v, %v) accepted it", tc.qps, tc.deadline)
			}
			for _, want := range []string{"--kube-api-qps", "--leader-election-renew-deadline", tc.deadline.String()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %s", err, want)
				}
			}
		})
	}
}

// main applies the budget to the one config the manager is built from. The manager is not
// started in a unit test, so this reads main.go: one GetConfigOrDie, the budget applied to it,
// and the same config passed to NewManager. Copying the config, applying the budget after
// NewManager, or building a second config fails it.
func TestMainAppliesTheBudgetToTheManagersConfig(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := strings.Count(src, "GetConfigOrDie()"); n != 1 {
		t.Fatalf("main.go calls GetConfigOrDie %d times; want one config for the manager", n)
	}
	steps := []string{
		"restConfig := ctrl.GetConfigOrDie()",
		"applyClientBudget(restConfig, kubeAPIQPS, kubeAPIBurst)",
		"if enableLeaderElection {",
		"checkRenewalBudget(kubeAPIQPS, leaderElectionRenewDeadline)",
		"kubeclientmetrics.Install(crmetrics.Registry)",
		"ctrl.NewManager(restConfig, ",
	}
	at := 0
	for _, step := range steps {
		i := strings.Index(src[at:], step)
		if i < 0 {
			t.Fatalf("main.go does not have %q after the step before it", step)
		}
		at += i + len(step)
	}
	if strings.Contains(src, "rest.CopyConfig(restConfig)") {
		t.Fatal("main.go copies restConfig; the manager must be built from the config the budget was applied to")
	}
}

// The budget is per client and per resource kind, as client_budget.go and the chart README
// say: with burst 1, a FleetZone GET does not wait behind a Robot GET, a second Robot GET
// does, and a second client's Robot GET does not. The wait is recorded under the Robot path
// template. The test takes about two seconds. If a controller-runtime upgrade shares one limiter across kinds or clients, this
// fails, and the documented scope must change with it.
func TestClientBudget_IsPerClientAndResourceKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// /apis/swarmada.io/v1/namespaces/<ns>/<resource>/<name>
		if len(parts) != 7 {
			http.NotFound(w, r)
			return
		}
		kind := map[string]string{"robots": "Robot", "fleetzones": "FleetZone"}[parts[5]]
		if kind == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": fleetv1.GroupVersion.String(), "kind": kind,
			"metadata": map[string]any{"name": parts[6], "namespace": parts[4]},
		})
	}))
	defer srv.Close()

	// Install points client-go's process-wide hook at a package-level histogram: start it
	// empty, so a run with -count above 1 reads only its own waits, and put the hook back.
	previous := clientgometrics.RateLimiterLatency
	t.Cleanup(func() { clientgometrics.RateLimiterLatency = previous })
	kubeclientmetrics.RateLimiterDuration.Reset()
	reg := prometheus.NewRegistry()
	kubeclientmetrics.Install(reg)

	// QPS 0.5 with burst 1: the first request of a bucket goes at once, the next waits ~2 s,
	// less the time the requests before it took. The 1 s and 1.5 s thresholds leave 500 ms
	// for those requests on a loaded runner. Set directly: applyClientBudget refuses a QPS
	// this low for the manager (leader election).
	cfg := &rest.Config{Host: srv.URL, QPS: 0.5, Burst: 1}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(fleetv1.GroupVersion.WithKind("Robot"), meta.RESTScopeNamespace)
	mapper.Add(fleetv1.GroupVersion.WithKind("FleetZone"), meta.RESTScopeNamespace)
	newClient := func() client.Client {
		c, err := client.New(cfg, client.Options{Scheme: scheme, Mapper: mapper})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first, second := newClient(), newClient()

	ctx := context.Background()
	get := func(c client.Client, obj client.Object, name string) time.Duration {
		t.Helper()
		start := time.Now()
		if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, obj); err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}
		return time.Since(start)
	}
	const quick, throttled = time.Second, 1500 * time.Millisecond

	if d := get(first, &fleetv1.Robot{}, "r1"); d > quick {
		t.Fatalf("the first Robot GET took %v; its bucket was full", d)
	}
	if d := get(first, &fleetv1.FleetZone{}, "z1"); d > quick {
		t.Fatalf("a FleetZone GET after a Robot GET took %v: kinds share a limiter", d)
	}
	if d := get(second, &fleetv1.Robot{}, "r2"); d > quick {
		t.Fatalf("a second client's Robot GET took %v: clients share a limiter", d)
	}
	if d := get(first, &fleetv1.Robot{}, "r3"); d < throttled {
		t.Fatalf("a second Robot GET on one client took %v; it should wait on the Robot bucket", d)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var robotWait float64
	for _, f := range families {
		if f.GetName() != "rest_client_rate_limiter_wait_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labels(m)["path"] == "/apis/swarmada.io/v1/namespaces/{namespace}/robots/{name}" &&
				labels(m)["verb"] == "GET" {
				robotWait = m.GetHistogram().GetSampleSum()
			}
		}
	}
	if robotWait < throttled.Seconds() {
		t.Fatalf("rest_client_rate_limiter_wait_seconds recorded %.3fs of Robot GET waits; want at least %v",
			robotWait, throttled)
	}
}

func labels(m *dto.Metric) map[string]string {
	out := map[string]string{}
	for _, l := range m.GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}
