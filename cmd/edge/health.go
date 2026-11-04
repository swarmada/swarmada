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
	"net/http"
	"sync/atomic"
	"time"
)

// healthServer exposes /healthz (liveness) and /readyz (readiness) for the edge
// node. Liveness is up as soon as the process serves; readiness flips true only once
// the EdgeStream listener is bound (see setReady), so a load balancer or supervisor
// does not route to a node that cannot yet accept adapter streams.
type healthServer struct {
	ready atomic.Bool
	srv   *http.Server
}

func newHealthServer(addr string) *healthServer {
	h := &healthServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
	})
	h.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return h
}

func (h *healthServer) setReady(ready bool) { h.ready.Store(ready) }

// start serves the health endpoints until ctx is cancelled, then shuts down.
func (h *healthServer) start(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() {
		if err := h.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
			return
		}
		errc <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return h.srv.Shutdown(shutdownCtx)
	case err := <-errc:
		return err
	}
}
