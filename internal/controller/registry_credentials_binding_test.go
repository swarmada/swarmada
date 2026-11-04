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

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/registry"
)

// The registry credentials Secret's `registries` key names the hosts its credentials are
// for. A fetch from any other host is anonymous, and a Secret that names none is presented
// nowhere, with one Warning Event on the object whose fetch went anonymous.

// credentialsUnboundReason is the Event reason the controllers write for a Secret that lists
// no host.
const credentialsUnboundReason = "RegistryCredentialsUnbound"

func credentialsSecret(data map[string]string) *corev1.Secret {
	d := make(map[string][]byte, len(data))
	for k, v := range data {
		d[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryCredentialsSecret, Namespace: rolloutNS},
		Data:       d,
	}
}

// countReasonEvents drains rec and counts the events whose text contains reason.
func countReasonEvents(rec *record.FakeRecorder, reason string) int {
	n := 0
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				n++
			}
		default:
			return n
		}
	}
}

func TestFirmwareRollout_SignatureFetchFromUnlistedHostIsAnonymous(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	creds := credentialsSecret(map[string]string{"token": "registry-tok", "registries": "other.example"})
	r, c := newFirmwareReconciler(t, fwRollout("https://artifacts.example.com/x.sig"),
		signingConfig(true), secret, creds, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	ff := &fakeFetcher{sig: rawSig}
	r.Fetcher = ff

	reconcileFirmware(t, r)

	if ff.calls != 1 || ff.gotCred != nil {
		t.Fatalf("fetch from an unlisted host: calls=%d credential=%+v, want one anonymous fetch", ff.calls, ff.gotCred)
	}
	if !robotPending(t, c, "r1") {
		t.Fatal("the anonymous fetch verified, so the rollout should dispatch")
	}
}

func TestFirmwareRollout_OCISignatureFetchFromUnlistedRegistryIsAnonymous(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	creds := credentialsSecret(map[string]string{"token": "registry-tok", "registries": "reg.example"})
	r, _ := newFirmwareReconciler(t, fwRollout(ociRef(rawSig)),
		signingConfig(true), secret, creds, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	oci := &fakeOCIFetcher{blob: rawSig}
	r.OCIFetcher = oci

	reconcileFirmware(t, r)

	// The ref's registry is reg.example:5000; the Secret lists reg.example, another host.
	if oci.callCount != 1 || oci.gotCred != nil {
		t.Fatalf("oci fetch from an unlisted registry: calls=%d credential=%+v, want anonymous", oci.callCount, oci.gotCred)
	}
}

func TestFirmwareRollout_CredentialsListingNoHostAreNotPresentedAndWarnOnce(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	creds := credentialsSecret(map[string]string{"username": "u", "password": "p"})
	r, _ := newFirmwareReconciler(t, fwRollout("https://artifacts.example.com/x.sig"),
		signingConfig(true), secret, creds, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	ff := &fakeFetcher{sig: rawSig}
	r.Fetcher = ff
	rec := record.NewFakeRecorder(50)
	r.Recorder = rec

	reconcileFirmware(t, r)
	reconcileFirmware(t, r)

	if ff.calls != 2 || ff.gotCred != nil {
		t.Fatalf("calls=%d credential=%+v, want anonymous fetches", ff.calls, ff.gotCred)
	}
	if n := countReasonEvents(rec, credentialsUnboundReason); n != 1 {
		t.Errorf("%s Events over two reconciles = %d, want 1", credentialsUnboundReason, n)
	}
}

// An entry written as a URL matches no host. The fetch is anonymous, and one Warning Event
// says why, however many passes read the Secret.
func TestFirmwareRollout_RegistriesEntryWrittenAsAURLWarnsOnce(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	creds := credentialsSecret(map[string]string{"token": "registry-tok", "registries": "https://artifacts.example.com"})
	r, _ := newFirmwareReconciler(t, fwRollout("https://artifacts.example.com/x.sig"),
		signingConfig(true), secret, creds, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	ff := &fakeFetcher{sig: rawSig}
	r.Fetcher = ff
	rec := record.NewFakeRecorder(50)
	r.Recorder = rec

	reconcileFirmware(t, r)
	reconcileFirmware(t, r)

	if ff.gotCred != nil {
		t.Fatalf("an entry written as a URL matched host artifacts.example.com: credential=%+v", ff.gotCred)
	}
	if n := countReasonEvents(rec, "RegistryCredentialsHostIsURL"); n != 1 {
		t.Errorf("RegistryCredentialsHostIsURL Events over two reconciles = %d, want 1", n)
	}
}

// A well-formed list writes no such Event.
func TestFirmwareRollout_RegistriesWrittenAsHostsDoNotWarn(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	creds := credentialsSecret(map[string]string{"token": "registry-tok", "registries": "artifacts.example.com"})
	r, _ := newFirmwareReconciler(t, fwRollout("https://artifacts.example.com/x.sig"),
		signingConfig(true), secret, creds, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Fetcher = &fakeFetcher{sig: rawSig}
	rec := record.NewFakeRecorder(50)
	r.Recorder = rec

	reconcileFirmware(t, r)

	if n := countReasonEvents(rec, "RegistryCredentialsHostIsURL"); n != 0 {
		t.Errorf("RegistryCredentialsHostIsURL Events = %d for a list of hosts, want 0", n)
	}
}

func TestRegistryWatch_RegistriesEntryWrittenAsAURLWarnsOnce(t *testing.T) {
	reg := &credRecordingRegistry{fakeRegistry: fakeRegistry{tags: []string{}}}
	creds := credentialsSecret(map[string]string{"token": "registry-tok", "registries": "oci://reg.example:5000"})
	r, _ := newPolicyReconciler(t, registryWatchPolicy(), creds)
	r.Registry = reg
	rec := record.NewFakeRecorder(50)
	r.Recorder = rec

	reconcilePolicy(t, r)
	reconcilePolicy(t, r)

	for i, c := range reg.creds {
		if c != nil {
			t.Fatalf("poll %d carried credential %+v from an entry written as a URL", i, c)
		}
	}
	if n := countReasonEvents(rec, "RegistryCredentialsHostIsURL"); n != 1 {
		t.Errorf("RegistryCredentialsHostIsURL Events over %d polls = %d, want 1", reg.calls, n)
	}
}

// credRecordingRegistry is fakeRegistry that records the credential each call carried.
type credRecordingRegistry struct {
	fakeRegistry
	calls int
	creds []*registry.Credential
}

func (f *credRecordingRegistry) ListTags(ctx context.Context, reg, repo string, cred *registry.Credential) ([]string, error) {
	f.calls++
	f.creds = append(f.creds, cred)
	return f.fakeRegistry.ListTags(ctx, reg, repo, cred)
}

func TestRegistryWatch_PollOfUnlistedRegistryIsAnonymous(t *testing.T) {
	reg := &credRecordingRegistry{fakeRegistry: fakeRegistry{tags: []string{}}}
	creds := credentialsSecret(map[string]string{"token": "registry-tok", "registries": "ghcr.io"})
	r, _ := newPolicyReconciler(t, registryWatchPolicy(), creds)
	r.Registry = reg

	reconcilePolicy(t, r)

	if reg.calls == 0 {
		t.Fatal("the registry was not polled")
	}
	for i, c := range reg.creds {
		if c != nil {
			t.Fatalf("poll %d of an unlisted registry carried credential %+v", i, c)
		}
	}
}

func TestRegistryWatch_PollOfListedRegistryCarriesCredential(t *testing.T) {
	reg := &credRecordingRegistry{fakeRegistry: fakeRegistry{tags: []string{}}}
	creds := credentialsSecret(map[string]string{"token": "registry-tok", "registries": "reg.example:5000"})
	r, _ := newPolicyReconciler(t, registryWatchPolicy(), creds)
	r.Registry = reg

	reconcilePolicy(t, r)

	if reg.calls == 0 || reg.creds[0] == nil || reg.creds[0].BearerToken != "registry-tok" {
		t.Fatalf("poll of a listed registry: calls=%d creds=%+v, want the bearer", reg.calls, reg.creds)
	}
}

func TestRegistryWatch_CredentialsListingNoHostAreNotPresentedAndWarnOnce(t *testing.T) {
	reg := &credRecordingRegistry{fakeRegistry: fakeRegistry{tags: []string{}}}
	creds := credentialsSecret(map[string]string{"token": "registry-tok"})
	r, _ := newPolicyReconciler(t, registryWatchPolicy(), creds)
	r.Registry = reg
	rec := record.NewFakeRecorder(50)
	r.Recorder = rec

	reconcilePolicy(t, r)
	reconcilePolicy(t, r)

	if reg.calls != 2 {
		t.Fatalf("the registry was polled %d times, want 2", reg.calls)
	}
	for i, c := range reg.creds {
		if c != nil {
			t.Fatalf("poll %d carried credential %+v from a Secret that lists no host", i, c)
		}
	}
	if n := countReasonEvents(rec, credentialsUnboundReason); n != 1 {
		t.Errorf("%s Events over %d polls = %d, want 1", credentialsUnboundReason, reg.calls, n)
	}
}
