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
	"fmt"
	"io"
	"strings"
)

// PrintError writes a kubectl/git-style "error: <message>" line to w, with the
// "error:" prefix in bold red when color is enabled. A multi-line message (for
// example a CEL cross-field admission rejection with a bullet list) is printed
// verbatim after the prefixed first line, so API-server validation errors
// surface exactly as the server phrased them.
func PrintError(w io.Writer, colorEnabled bool, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	prefix := paint(colorEnabled, sgrBoldRed, "error:")
	// Split so the prefix attaches to the first line only.
	first, rest, multiline := strings.Cut(msg, "\n")
	_, _ = fmt.Fprintf(w, "%s %s\n", prefix, first)
	if multiline {
		_, _ = fmt.Fprintln(w, rest)
	}
}
