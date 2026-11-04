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

// Command event-bridge receives signed events from systems outside the cluster and
// turns each one it can map into a SemanticEvent, or refuses it with the reason.
//
//	caller ──HTTPS, HMAC-signed──► event-bridge serve ──create──► SemanticEvent
//	    ──► TaskTrigger controller ──► a DRAFT FleetTask a person clears
//
// It is a task source (ADR-0006): a client of the Kubernetes API whose Role creates
// SemanticEvents and nothing else. It never drafts, clears or dispatches anything.
//
//	event-bridge serve  -config FILE -keys-dir DIR -tls-cert FILE -tls-key FILE
//	event-bridge send   -url URL -ca FILE -key-id ID -type TYPE -id ID [-data k=v ...]
//
// send reads the key from the environment variable EVENT_BRIDGE_KEY, never from a
// flag, so it does not appear in a process listing.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/examples/event-bridge/bridge"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: event-bridge serve|send [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "send":
		err = send(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q: want serve or send", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "event-bridge:", err)
		os.Exit(1)
	}
}

// loadKeys reads one key per file: the file name is the key id. A mounted Secret
// volume also holds ..data and timestamped directories, which are skipped.
func loadKeys(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading keys: %w", err)
	}
	keys := map[string][]byte{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: the mounted key directory is the input.
		if err != nil {
			return nil, fmt.Errorf("reading key %s: %w", e.Name(), err)
		}
		b = bytes.TrimSpace(b)
		if len(b) < bridge.MinKeyBytes {
			return nil, fmt.Errorf("key %s is %d bytes; the bridge needs at least %d", e.Name(), len(b), bridge.MinKeyBytes)
		}
		keys[e.Name()] = b
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no keys in %s", dir)
	}
	return keys, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/event-bridge/config.json", "the bridge's vocabulary and caller bindings")
	keysDir := fs.String("keys-dir", "/etc/event-bridge/keys", "one HMAC key per file, named by key id")
	certFile := fs.String("tls-cert", "/etc/event-bridge/tls/tls.crt", "serving certificate")
	keyFile := fs.String("tls-key", "/etc/event-bridge/tls/tls.key", "serving key")
	listen := fs.String("listen", ":8443", "listen address")
	window := fs.Duration("window", 5*time.Minute, "how far a signed timestamp may be from the bridge's clock")
	maxSeen := fs.Int("max-seen", 10000, "replay cache size; when full, requests are refused, not evicted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var cfg bridge.Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	mapper, err := bridge.NewMapper(cfg)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	keys, err := loadKeys(*keysDir)
	if err != nil {
		return err
	}
	// A key with no binding authenticates a caller that may send nothing, and a
	// binding with no key can never be used: both are configuration mistakes.
	for id := range keys {
		if _, ok := cfg.Callers[id]; !ok {
			return fmt.Errorf("key %q has no caller binding in the config", id)
		}
	}
	for id := range cfg.Callers {
		if _, ok := keys[id]; !ok {
			return fmt.Errorf("caller %q has no key", id)
		}
	}

	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		return err
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	h := &bridge.Handler{
		Verifier:  bridge.NewVerifier(keys, *window, *maxSeen, time.Now),
		Mapper:    mapper,
		Creator:   c,
		Namespace: cfg.Namespace,
		Now:       time.Now,
		Log:       log,
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/events", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	callers := make([]string, 0, len(keys))
	for id := range keys {
		callers = append(callers, id)
	}
	log.Info("serving", "listen", *listen, "namespace", cfg.Namespace, "callers", callers,
		"window", window.String(), "maxSeen", *maxSeen)
	if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type dataFlags map[string]string

func (d dataFlags) String() string { return fmt.Sprint(map[string]string(d)) }
func (d dataFlags) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("%q is not key=value", s)
	}
	d[k] = v
	return nil
}

// send signs one envelope and posts it. -repeat sends the identical signed request
// again (a replay); -resend sends the same envelope again with a fresh timestamp and
// signature (a second delivery); -tamper changes the body after signing.
func send(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	url := fs.String("url", "https://localhost:8443/v1/events", "the bridge's events URL")
	caFile := fs.String("ca", "", "CA certificate to verify the bridge with (required)")
	serverName := fs.String("server-name", "", "name to verify the bridge's certificate against (default: the URL's host)")
	keyID := fs.String("key-id", "", "the caller's key id")
	typ := fs.String("type", "", "envelope type")
	id := fs.String("id", "", "the caller's id for this occurrence")
	occurredAt := fs.String("occurred-at", "", "RFC 3339; empty for none")
	skew := fs.Duration("timestamp-offset", 0, "sign with a timestamp this far from now (to show the window)")
	repeat := fs.Int("repeat", 1, "send the identical signed request this many times")
	resend := fs.Bool("resend", false, "then send the same envelope again, freshly signed")
	tamper := fs.Bool("tamper", false, "change the body after signing")
	data := dataFlags{}
	fs.Var(data, "data", "key=value, repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key := []byte(os.Getenv("EVENT_BRIDGE_KEY"))
	if len(key) == 0 || *caFile == "" || *keyID == "" {
		return errors.New("EVENT_BRIDGE_KEY, -ca and -key-id are required")
	}
	caPEM, err := os.ReadFile(*caFile)
	if err != nil {
		return fmt.Errorf("reading CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("no certificate in the CA file")
	}
	httpc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: *serverName, MinVersion: tls.VersionTLS12},
	}}
	env := bridge.Envelope{ID: *id, Type: *typ, OccurredAt: *occurredAt}
	if len(data) > 0 {
		env.Data = data
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}

	do := func(label string, stamp string, sig string, b []byte) error {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, *url, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(bridge.HeaderKeyID, *keyID)
		req.Header.Set(bridge.HeaderTimestamp, stamp)
		req.Header.Set(bridge.HeaderSignature, sig)
		resp, err := httpc.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		fmt.Printf("%s %d %s\n", label, resp.StatusCode, bytes.TrimSpace(out))
		return nil
	}
	stamp := strconv.FormatInt(time.Now().Add(*skew).Unix(), 10)
	sig := bridge.Sign(key, *keyID, stamp, body)
	sent := body
	if *tamper {
		sent = bytes.Replace(body, []byte(*id), []byte(*id+"-x"), 1)
	}
	for i := 1; i <= *repeat; i++ {
		if err := do(fmt.Sprintf("send#%d", i), stamp, sig, sent); err != nil {
			return err
		}
	}
	if *resend {
		time.Sleep(1100 * time.Millisecond) // a new second, so a new timestamp and signature
		stamp = strconv.FormatInt(time.Now().Unix(), 10)
		if err := do("resend", stamp, bridge.Sign(key, *keyID, stamp, body), body); err != nil {
			return err
		}
	}
	return nil
}
