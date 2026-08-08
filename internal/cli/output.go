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
	"encoding/json"
	"fmt"
	"io"

	"sigs.k8s.io/yaml"
)

// OutputFormat is the value of the -o/--output flag. The empty value is the
// default colorized table; "wide" is the same table with extra columns.
type OutputFormat string

// The supported -o values.
const (
	OutputTable OutputFormat = ""
	OutputWide  OutputFormat = "wide"
	OutputYAML  OutputFormat = "yaml"
	OutputJSON  OutputFormat = "json"
)

// ParseOutputFormat validates a -o flag value, accepting "table" as an explicit
// spelling of the default.
func ParseOutputFormat(s string) (OutputFormat, bool) {
	switch OutputFormat(s) {
	case OutputTable, OutputWide, OutputYAML, OutputJSON:
		return OutputFormat(s), true
	case "table":
		return OutputTable, true
	default:
		return "", false
	}
}

// IsTable reports whether f selects the human table view (default or wide) as
// opposed to a machine-readable marshal.
func (f OutputFormat) IsTable() bool { return f == OutputTable || f == OutputWide }

// IsWide reports whether the wide table (priority>=1 print columns) is selected.
func (f OutputFormat) IsWide() bool { return f == OutputWide }

// PrintMarshaled writes obj as YAML or JSON per f. It is only valid for the
// machine-readable formats; callers gate on IsTable first. Color never applies
// to marshaled output — it must stay pipe- and parser-safe.
func PrintMarshaled(w io.Writer, f OutputFormat, obj any) error {
	switch f {
	case OutputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(obj)
	case OutputYAML:
		out, err := yaml.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshaling YAML: %w", err)
		}
		_, err = w.Write(out)
		return err
	default:
		return fmt.Errorf("format %q is not machine-readable", f)
	}
}
