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
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/swarmada/swarmada/internal/cli"
)

// Overridable at build time via -ldflags "-X main.version=..."; otherwise the
// values are recovered from the embedded build info (module version, VCS
// commit) so a `go install`ed binary still reports something meaningful.
var (
	version   = ""
	gitCommit = ""
)

// versionInfo is the machine-readable shape for `version -o yaml|json`.
type versionInfo struct {
	Version   string `json:"version"`
	GitCommit string `json:"gitCommit"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

func newVersionCommand(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the swarmctl client version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := collectVersion()
			if !o.outputFmt.IsTable() {
				return cli.PrintMarshaled(o.streams.Out, o.outputFmt, info)
			}
			_, _ = fmt.Fprintf(o.streams.Out, "swarmctl version %s (%s)\n", info.Version, info.GitCommit)
			_, _ = fmt.Fprintf(o.streams.Out, "  go: %s  platform: %s\n", info.GoVersion, info.Platform)
			return nil
		},
	}
}

func collectVersion() versionInfo {
	v, commit := version, gitCommit
	if v == "" || commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			if v == "" {
				v = bi.Main.Version
			}
			if commit == "" {
				for _, s := range bi.Settings {
					if s.Key == "vcs.revision" {
						commit = s.Value
					}
				}
			}
		}
	}
	if v == "" {
		v = "dev"
	}
	if commit == "" {
		commit = "unknown"
	}
	return versionInfo{
		Version:   v,
		GitCommit: commit,
		GoVersion: runtime.Version(),
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}
