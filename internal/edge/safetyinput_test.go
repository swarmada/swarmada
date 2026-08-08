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
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func writeLine(t *testing.T, level string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(p, []byte(level), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileSafetyInput_ActiveLow(t *testing.T) {
	// Normally-closed: 0 (or open/low) = tripped, 1 = safe.
	in := &FileSafetyInput{Path: writeLine(t, "0\n"), ActiveLow: true}
	if tripped, err := in.Tripped(context.Background()); err != nil || !tripped {
		t.Fatalf("active-low level 0: tripped=%v err=%v, want tripped", tripped, err)
	}
	in.Path = writeLine(t, "1")
	if tripped, err := in.Tripped(context.Background()); err != nil || tripped {
		t.Fatalf("active-low level 1: tripped=%v err=%v, want not tripped", tripped, err)
	}
}

func TestFileSafetyInput_ActiveHigh(t *testing.T) {
	in := &FileSafetyInput{Path: writeLine(t, "1"), ActiveLow: false}
	if tripped, err := in.Tripped(context.Background()); err != nil || !tripped {
		t.Fatalf("active-high level 1: tripped=%v err=%v, want tripped", tripped, err)
	}
	in.Path = writeLine(t, "0")
	if tripped, err := in.Tripped(context.Background()); err != nil || tripped {
		t.Fatalf("active-high level 0: tripped=%v err=%v, want not tripped", tripped, err)
	}
}

func TestFileSafetyInput_ErrorsOnMissingOrGarbage(t *testing.T) {
	missing := &FileSafetyInput{Path: filepath.Join(t.TempDir(), "nope")}
	if _, err := missing.Tripped(context.Background()); err == nil {
		t.Fatal("expected an error for a missing input file")
	}
	garbage := &FileSafetyInput{Path: writeLine(t, "banana")}
	if _, err := garbage.Tripped(context.Background()); err == nil {
		t.Fatal("expected an error for an unparseable level")
	}
}

// fakeInput drives the monitor deterministically.
type fakeInput struct {
	tripped bool
	err     error
}

func (f *fakeInput) Name() string                          { return "test-input" }
func (f *fakeInput) Tripped(context.Context) (bool, error) { return f.tripped, f.err }

func TestInputMonitor_EdgeTriggered(t *testing.T) {
	fi := &fakeInput{}
	var fires int
	m := NewInputMonitor(fi, func(context.Context, string) { fires++ }, time.Hour, logr.Discard())
	ctx := context.Background()

	m.poll(ctx) // safe → no fire
	if fires != 0 {
		t.Fatalf("fires=%d after safe poll, want 0", fires)
	}
	fi.tripped = true
	m.poll(ctx) // rising edge → fire
	m.poll(ctx) // held → no re-fire
	if fires != 1 {
		t.Fatalf("fires=%d while held, want 1 (edge-triggered)", fires)
	}
	fi.tripped = false
	m.poll(ctx) // falling → no fire, re-arm
	fi.tripped = true
	m.poll(ctx) // rising again → fire
	if fires != 2 {
		t.Fatalf("fires=%d after re-arm, want 2", fires)
	}
}

func TestInputMonitor_ReadErrorIsFailSafeTripped(t *testing.T) {
	fi := &fakeInput{tripped: false, err: errors.New("line unreadable")}
	var fires int
	m := NewInputMonitor(fi, func(context.Context, string) { fires++ }, time.Hour, logr.Discard())
	m.poll(context.Background())
	if fires != 1 {
		t.Fatalf("SAFETY: read error did not trip the estop; fires=%d, want 1", fires)
	}
}

// The monitor drives the real Node.TriggerLocalEstop, which fans a headless estop out
// to a connected EdgeStream — the full local-input → estop path.
func TestInputMonitor_TriggersNodeLocalEstop(t *testing.T) {
	n, _ := newNode(t)
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()
	// Give the stream a moment to register before we fan out.
	time.Sleep(10 * time.Millisecond)

	fi := &fakeInput{tripped: true}
	m := NewInputMonitor(fi, n.TriggerLocalEstop, time.Hour, logr.Discard())
	m.poll(context.Background())

	select {
	case msg := <-fs.out:
		est := msg.GetEstop()
		if est == nil {
			t.Fatalf("expected an Estop on the stream, got %T", msg.GetMsg())
		}
		if est.GetReason() == "" {
			t.Error("estop reason is empty; expected the safety-input reason")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no estop fanned out to the connected stream on a local safety trip")
	}
}
