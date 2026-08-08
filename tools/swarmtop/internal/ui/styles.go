// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/swarmada/swarmtop/internal/format"
)

// styles centralizes the terminal palette. Colors are ANSI-256 with adaptive
// light/dark variants so swarmtop reads on both terminal themes.
type styles struct {
	header    lipgloss.Style
	colHeader lipgloss.Style
	help      lipgloss.Style
	selected  lipgloss.Style
	paneDivet lipgloss.Style

	good  lipgloss.Style
	warn  lipgloss.Style
	bad   lipgloss.Style
	muted lipgloss.Style
}

func newStyles() styles {
	muted := lipgloss.AdaptiveColor{Light: "245", Dark: "240"}
	return styles{
		header:    lipgloss.NewStyle().Bold(true),
		colHeader: lipgloss.NewStyle().Foreground(muted),
		help:      lipgloss.NewStyle().Foreground(muted),
		selected:  lipgloss.NewStyle().Reverse(true),
		paneDivet: lipgloss.NewStyle().Foreground(muted),

		good:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"}),
		warn:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"}),
		bad:   lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"}),
		muted: lipgloss.NewStyle().Foreground(muted),
	}
}

// level applies the color for a format.Level to the given text.
func (s styles) level(text string, lvl format.Level) string {
	switch lvl {
	case format.LevelGood:
		return s.good.Render(text)
	case format.LevelWarn:
		return s.warn.Render(text)
	case format.LevelBad:
		return s.bad.Render(text)
	default:
		return s.muted.Render(text)
	}
}
