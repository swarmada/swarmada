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

	"github.com/swarmada/swarmada/internal/cli"
)

// options is the shared state the cobra root resolves once, from the persistent
// flags, and hands to every subcommand: the client factory, the IO streams, and
// the resolved output/color decisions.
type options struct {
	streams cli.IOStreams

	// Raw persistent-flag values, bound by cobra.
	kubeconfig string
	context    string
	namespace  string
	output     string
	colorFlag  string

	// Resolved during PersistentPreRunE.
	factory     *cli.Factory
	outputFmt   cli.OutputFormat
	colorMode   cli.ColorMode
	colorStdout bool
}

// newRootCommand builds the swarmctl command tree. The returned *options is
// populated during PersistentPreRunE and is what main() reads to colorize a
// top-level error consistently with the rest of the session.
func newRootCommand(streams cli.IOStreams) (*cobra.Command, *options) {
	o := &options{streams: streams}

	root := &cobra.Command{
		Use:   "swarmctl",
		Short: "Operate a Swarmada robot fleet from the command line",
		Long: `swarmctl is the operator CLI for Swarmada — "Kubernetes for robots".

It talks to the same Kubernetes API as kubectl, using your ~/.kube/config, and
renders the Swarmada CRDs (Robot, FleetAction, FleetZone, and the rest) with
kubectl-familiar tables and describe views. Read verbs mirror the CRDs' own
print columns; lifecycle and safety verbs (admit, delete, cancel, estop,
estop-clear) go through the RBAC-gated custom-verb path so authorization and
audit apply.`,
		SilenceErrors: true, // main() renders errors kubectl-style ("error: ...").
		SilenceUsage:  true, // a runtime error is not a usage error.
		// A bare `swarmctl` with no subcommand prints help rather than erroring.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return o.complete()
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&o.kubeconfig, "kubeconfig", "", "Path to the kubeconfig file to use (overrides $KUBECONFIG)")
	pf.StringVar(&o.context, "context", "", "Name of the kubeconfig context to use")
	pf.StringVarP(&o.namespace, "namespace", "n", "", "Namespace scope for this request")
	pf.StringVarP(&o.output, "output", "o", "", "Output format: one of table|wide|yaml|json")
	pf.StringVar(&o.colorFlag, "color", string(cli.ColorAuto), "When to colorize output: always|auto|never")

	root.AddCommand(
		newGetCommand(o),
		newDescribeCommand(o),
		newAdmitCommand(o),
		newDeleteCommand(o),
		newCancelCommand(o),
		newEstopCommand(o),
		newModelPolicyCommand(o),
		newEstopClearCommand(o),
		newRobotClassCommand(o),
		newVerifyCommand(o),
		newExportCommand(o),
		newVersionCommand(o),
	)

	return root, o
}

// complete validates the persistent flags and builds the client factory. It runs
// before every subcommand's RunE.
func (o *options) complete() error {
	fmtVal, ok := cli.ParseOutputFormat(o.output)
	if !ok {
		return fmt.Errorf("invalid --output %q: must be one of table|wide|yaml|json", o.output)
	}
	o.outputFmt = fmtVal

	mode, ok := cli.ParseColorMode(o.colorFlag)
	if !ok {
		return fmt.Errorf("invalid --color %q: must be one of always|auto|never", o.colorFlag)
	}
	o.colorMode = mode
	o.colorStdout = cli.ColorEnabled(mode, stdoutFile(o.streams))

	o.factory = cli.NewFactory(cli.ConfigFlags{
		Kubeconfig: o.kubeconfig,
		Context:    o.context,
		Namespace:  o.namespace,
	})
	return nil
}

// colorErr reports whether errors written to stderr should be colorized.
func (o *options) colorErr() bool {
	return cli.ColorEnabled(o.colorMode, stderrFile(o.streams))
}

// stdoutFile / stderrFile recover the concrete *os.File behind the streams for
// the TTY check; a test buffer yields nil, which ColorEnabled treats as not a
// terminal (color off), which is what we want under `go test`.
func stdoutFile(s cli.IOStreams) *os.File {
	if f, ok := s.Out.(*os.File); ok {
		return f
	}
	return nil
}

func stderrFile(s cli.IOStreams) *os.File {
	if f, ok := s.Err.(*os.File); ok {
		return f
	}
	return nil
}
