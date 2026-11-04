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
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/swarmada/swarmada/internal/signing"
)

// newSignCommand is `swarmctl sign`, the producer side of the signature the
// FleetAdapter controller verifies.
//
// Conformance is SELF-certified (ADR-0007): the project issues no certificate and keeps
// no trust list, so this signature is not a project endorsement. It is the adapter
// author's attestation that a report is genuinely theirs and was not altered in transit.
// Whether an operator trusts a given signer is the operator's decision, expressed by
// putting that key in SwarmadaConfig.spec.signing.trustRoots — a per-cluster trust
// choice, not a central authority.
func newSignCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sign",
		Short: "Sign an artifact the control plane verifies",
	}
	cmd.AddCommand(newSignConformanceCommand(o))
	return cmd
}

func newSignConformanceCommand(_ *options) *cobra.Command {
	var reportPath, keyPath string
	cmd := &cobra.Command{
		Use:   "conformance-report",
		Short: "Sign a conformance report, emitting the inline signature the ConfigMap carries",
		Long: `Sign a conformance report produced by the conformance harness.

The FleetAdapter controller verifies this signature over the report bytes whenever the
namespace sets signing.requireSignatureVerification (the default). Before this command
existed nothing in the tree produced such a signature, so on a signing-enforced cluster
no adapter could reach conformance=Passed and therefore no Robot naming it could be
admitted.

The signature is printed in the inline "bundle:<base64>" form, which is what the report
ConfigMap's "signature" key holds. The payload signed is the report file EXACTLY as
given: the controller verifies the ConfigMap's report bytes, so any reformatting between
signing and applying invalidates the signature.

Conformance is self-certified (ADR-0007): this attests that the report is yours, not that
the project endorses it. An operator chooses to trust a signer by adding its public key to
SwarmadaConfig.spec.signing.trustRoots.`,
		Example: `  # Sign, then put the report and signature in the ConfigMap the FleetAdapter names.
  swarmctl sign conformance-report --report report.json --key vendor-signing.pem
  kubectl create configmap sim-fleet-adapter-conformance -n warehouse-a \
    --from-file=report.json=report.json \
    --from-literal=signature="$(swarmctl sign conformance-report --report report.json --key vendor-signing.pem)"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := os.ReadFile(reportPath)
			if err != nil {
				return fmt.Errorf("reading conformance report: %w", err)
			}
			keyPEM, err := os.ReadFile(keyPath)
			if err != nil {
				return fmt.Errorf("reading signing key: %w", err)
			}
			signer, err := signing.ParseSigningKey(keyPEM)
			if err != nil {
				return err
			}
			sig, err := signing.Sign(report, signer)
			if err != nil {
				return fmt.Errorf("signing conformance report: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), signing.EncodeInline(sig))
			return err
		},
	}
	cmd.Flags().StringVar(&reportPath, "report", "", "Path to the conformance report JSON (required)")
	cmd.Flags().StringVar(&keyPath, "key", "", "Path to the PEM signing key, PKCS#8 or SEC1 EC (required)")
	_ = cmd.MarkFlagRequired("report")
	_ = cmd.MarkFlagRequired("key")
	return cmd
}
