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
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// Confirm gates a destructive or safety-relevant action (estop, cancel, reject)
// behind an interactive y/N prompt, unless assumeYes (the --yes flag) is set.
//
// When --yes is absent and stdin is not an interactive terminal, it refuses
// rather than silently proceeding: an operator piping into swarmctl must opt in
// explicitly with --yes, so a scripted safety action is always a deliberate one.
func Confirm(streams IOStreams, prompt string, assumeYes bool) (bool, error) {
	if assumeYes {
		return true, nil
	}
	if !stdinIsTerminal(streams.In) {
		return false, errors.New("refusing to proceed without --yes: stdin is not an interactive terminal")
	}
	_, _ = fmt.Fprintf(streams.Err, "%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(streams.In).ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// stdinIsTerminal reports whether in is an interactive TTY. A non-*os.File
// reader (a test buffer) is treated as interactive so tests can feed input; a
// real *os.File that is not a terminal (a pipe or redirect) is not.
func stdinIsTerminal(in interface{}) bool {
	f, ok := in.(*os.File)
	if !ok {
		return in != nil
	}
	return term.IsTerminal(int(f.Fd()))
}
