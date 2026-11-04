package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// SignatureVerified and FIRMWARE_SIGNATURE_VERIFIED assert that a signature was checked
// against a trust root. When SwarmadaConfig does not enforce signing, nothing is checked,
// so neither may claim it — whether or not the rollout names a signature reference.
// RFC-0001 #safety-required-events: FIRMWARE_SIGNATURE_VERIFIED means the control plane
// verified the artifact signature before any dispatch.
func TestFirmwareRollout_UnenforcedSigningIsNotReportedVerified(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sigRef string
	}{
		{"signature ref set", "oci://registry.example/fw.sig"},
		{"no signature ref", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &captureRecorder{}
			r, c := newFirmwareReconciler(t, fwRollout(tc.sigRef), signingConfig(false),
				targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
			r.Audit = rec

			reconcileFirmware(t, r)

			// Behaviour is unchanged: unenforced signing still dispatches.
			if !robotPending(t, c, "r1") {
				t.Fatal("an unenforced-signing rollout should still dispatch to the eligible robot")
			}
			fw := getFirmwareRollout(t, c)
			var cond *metav1.Condition
			for i := range fw.Status.Conditions {
				if fw.Status.Conditions[i].Type == conditionSignatureVerified {
					cond = &fw.Status.Conditions[i]
				}
			}
			if cond == nil {
				t.Fatalf("no SignatureVerified condition: %+v", fw.Status.Conditions)
			}
			if cond.Status == metav1.ConditionTrue {
				t.Errorf("SignatureVerified=True (reason %q, message %q) although no signature was verified",
					cond.Reason, cond.Message)
			}
			if cond.Reason != fleetv1.ReasonVerificationDisabled || !strings.Contains(cond.Message, "not enforced") {
				t.Errorf("SignatureVerified reason/message = %q / %q; want reason VerificationDisabled and a message saying verification is not enforced",
					cond.Reason, cond.Message)
			}
			if n := len(entriesOfType(rec, audit.EventFirmwareSignatureVerified)); n != 0 {
				t.Errorf("sealed %d FIRMWARE_SIGNATURE_VERIFIED entries although no signature was verified", n)
			}
		})
	}
}
