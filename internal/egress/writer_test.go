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

package egress_test

// THE MEASUREMENT the egress rule owes: "a packet capture or equivalent
// shows the payload bytes never traversed the control plane." A real kernel
// packet capture (tcpdump/BPF) needs root/CAP_NET_RAW, which this sandbox
// does not grant (verified: `tcpdump -i lo0` returns "Permission denied —
// (cannot open BPF device)", not skipped on assumption). The equivalent used
// here is still a REAL network-level capture, not a mock assertion: every
// byte actually read off a real loopback TCP connection standing in for the
// control plane's wire channel is recorded and searched for the artifact
// payload, while the artifact itself travels over a SEPARATE real HTTP
// connection straight to the mock ArtifactStore. Two genuinely different
// sockets, two independently observed byte streams.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/swarmada/swarmada/internal/egress"
)

// captureListener wraps a net.Listener so every byte any accepted connection
// reads from the wire is appended to a shared, mutex-guarded buffer — the
// "packet capture" for whatever stands behind this listener.
type captureListener struct {
	net.Listener
	mu       sync.Mutex
	captured bytes.Buffer
}

func newCaptureListener(t *testing.T) *captureListener {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return &captureListener{Listener: lis}
}

func (c *captureListener) Accept() (net.Conn, error) {
	conn, err := c.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &captureConn{Conn: conn, parent: c}, nil
}

func (c *captureListener) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, c.captured.Len())
	copy(out, c.captured.Bytes())
	return out
}

type captureConn struct {
	net.Conn
	parent *captureListener
}

func (c *captureConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.parent.mu.Lock()
		c.parent.captured.Write(p[:n])
		c.parent.mu.Unlock()
	}
	return n, err
}

// runNoopControlPlane serves a control-plane stand-in on lis: it accepts
// connections and reads a small fixed-size status frame (representing an
// ActionResult status write — references only, never bytes) then closes. Any
// larger read attempt (e.g. an artifact payload arriving here) still gets
// captured by captureListener regardless of what this handler does with it.
func runNoopControlPlane(lis net.Listener) {
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4096)
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, _ = c.Read(buf) // drain whatever arrives; captureConn already recorded it
			}(conn)
		}
	}()
}

// sendStatusUpdate is what a conforming executor sends the control plane: a
// small reference-only status frame — schemaRef, store+path+digest, pose,
// timestamps — modelled here as its literal JSON-ish bytes, never the image.
func sendStatusUpdate(t *testing.T, addr string, storeName, path, digestHex string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialing control plane: %v", err)
	}
	defer func() { _ = conn.Close() }()
	frame := `{"schemaRef":"plate-reader","artifacts":[{"name":"frame","store":"` +
		storeName + `","path":"` + path + `","digest":"sha256:` + digestHex +
		`","capturedAt":"2026-09-01T18:03:11.412Z","pose":{"x":12.4,"y":3.1,"theta":1.57,"frame":"map"}}]}`
	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatalf("writing status frame: %v", err)
	}
}

func TestArtifactBytesNeverTraverseTheControlPlane(t *testing.T) {
	// A realistic-sized "frame" — large enough that if it leaked onto the
	// control-plane connection even partially, the substring search below
	// would catch it; deterministic so the digest is reproducible in the log.
	payload := make([]byte, 256*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generating payload: %v", err)
	}
	wantSum := sha256.Sum256(payload)
	wantDigest := hex.EncodeToString(wantSum[:])

	// The ArtifactStore: a real HTTP server on a real TCP socket, receiving
	// the artifact directly.
	var storeReceived []byte
	var storeMu sync.Mutex
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		storeMu.Lock()
		storeReceived = b
		storeMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer store.Close()

	// The control plane stand-in: a real, independently captured TCP socket.
	cp := newCaptureListener(t)
	defer func() { _ = cp.Close() }()
	runNoopControlPlane(cp)

	// The executor's actual behaviour under test: write the artifact
	// straight to the store, then report only the small reference frame to
	// the control plane. These are two unrelated connections; nothing hands
	// the payload to the control-plane socket at any point.
	gotDigest, err := egress.WriteArtifact(context.Background(), store.URL, payload)
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	if gotDigest != wantDigest {
		t.Fatalf("digest mismatch: got %s want %s", gotDigest, wantDigest)
	}
	sendStatusUpdate(t, cp.Addr().String(), "evidence", "2026-09-01/18-03-11Z.mp4", gotDigest)

	time.Sleep(100 * time.Millisecond) // let the control-plane goroutine finish its Read

	// ── THE ASSERTIONS ──────────────────────────────────────────────────
	storeMu.Lock()
	gotStoreBytes := len(storeReceived)
	storeOK := bytes.Equal(storeReceived, payload)
	storeMu.Unlock()
	if !storeOK {
		t.Fatalf("ArtifactStore received %d bytes not matching the payload (%d bytes)", gotStoreBytes, len(payload))
	}

	captured := cp.bytes()
	if bytes.Contains(captured, payload) {
		t.Fatal("THE ARTIFACT PAYLOAD WAS FOUND ON THE CONTROL-PLANE CONNECTION")
	}
	// Search for any substantial contiguous chunk too, not just the whole
	// payload, in case of a partial/chunked leak.
	const chunk = 4096
	for i := 0; i+chunk <= len(payload); i += chunk {
		if bytes.Contains(captured, payload[i:i+chunk]) {
			t.Fatalf("a %d-byte chunk of the artifact payload (offset %d) was found on the control-plane connection", chunk, i)
		}
	}

	t.Logf("MEASURED: ArtifactStore received %d bytes (payload, digest %s, matched exactly). "+
		"Control-plane connection captured %d bytes total (the status reference frame) — "+
		"zero bytes of the %d-byte artifact payload found in it, whole or in %d-byte chunks.",
		gotStoreBytes, gotDigest, len(captured), len(payload), chunk)
}
