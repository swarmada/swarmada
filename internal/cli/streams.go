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

package cli

import (
	"io"
	"os"
)

// IOStreams bundles the three standard streams a command reads and writes, so
// tests can substitute buffers and the color/TTY decision has a concrete *os.File
// to inspect for stdout.
type IOStreams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// StdStreams returns the process's real stdin/stdout/stderr.
func StdStreams() IOStreams {
	return IOStreams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
}
