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

// Package cli holds the presentation and client plumbing shared by every
// swarmctl command: colorized table/describe rendering, -o yaml|json output,
// structured errors, and the Kubernetes client factory. It intentionally holds
// no api/v1 knowledge beyond the generated types so the command packages stay
// thin.
package cli

import (
	"os"

	"golang.org/x/term"
)

// Standard 8-color ANSI SGR sequences (30-37 + bold/dim), following the CLI
// style rule "Respect the terminal, don't override it." No 256-color or truecolor codes,
// so the user's own terminal theme decides the actual RGB.
const (
	sgrReset  = "\033[0m"
	sgrBold   = "\033[1m"
	sgrDim    = "\033[2m"
	sgrGreen  = "\033[32m"
	sgrYellow = "\033[33m"
	sgrRed    = "\033[31m"
	sgrCyan   = "\033[36m"
	// bold red — used for the "error:" prefix and 0-19% battery.
	sgrBoldRed = "\033[1;31m"
	// bold white on red (reverse-video) — the unmissable Offline state.
	sgrOffline = "\033[1;37;41m"
)

// ColorMode is the value of the --color flag.
type ColorMode string

// The three --color modes, per the style guide ("Color is conditional, not
// assumed"). auto is the default.
const (
	ColorAuto   ColorMode = "auto"
	ColorAlways ColorMode = "always"
	ColorNever  ColorMode = "never"
)

// ParseColorMode validates a --color flag value.
func ParseColorMode(s string) (ColorMode, bool) {
	switch ColorMode(s) {
	case ColorAuto, ColorAlways, ColorNever:
		return ColorMode(s), true
	default:
		return "", false
	}
}

// ColorEnabled resolves whether colorized output should be emitted to w under
// mode. It honors NO_COLOR (any value disables color, per no-color.org) and,
// in auto mode, only colorizes when w is a TTY — so `swarmctl get robot | cat`
// and redirected output are plain text automatically.
func ColorEnabled(mode ColorMode, w *os.File) bool {
	switch mode {
	case ColorNever:
		return false
	case ColorAlways:
		return true
	default: // ColorAuto
		if _, ok := os.LookupEnv("NO_COLOR"); ok {
			return false
		}
		return w != nil && term.IsTerminal(int(w.Fd()))
	}
}

// paint wraps text in an SGR sequence when enabled; otherwise it returns text
// unchanged. Color is always a second channel here — callers never rely on it
// as the only signal, so a monochrome terminal loses emphasis, not meaning.
func paint(enabled bool, sgr, text string) string {
	if !enabled || sgr == "" {
		return text
	}
	return sgr + text + sgrReset
}
