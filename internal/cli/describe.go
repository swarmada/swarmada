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
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DescribeHeader writes the kubectl describe-style metadata block (Name,
// Namespace, Labels, Annotations) shared by every resource. Labels and
// annotations are printed one-per-line, sorted, with "<none>" when empty.
func DescribeHeader(w io.Writer, meta metav1.Object) {
	_, _ = fmt.Fprintf(w, "Name:         %s\n", meta.GetName())
	_, _ = fmt.Fprintf(w, "Namespace:    %s\n", meta.GetNamespace())
	writeMap(w, "Labels:", meta.GetLabels())
	writeMap(w, "Annotations:", meta.GetAnnotations())
}

func writeMap(w io.Writer, label string, m map[string]string) {
	if len(m) == 0 {
		_, _ = fmt.Fprintf(w, "%-13s %s\n", label, None)
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i == 0 {
			_, _ = fmt.Fprintf(w, "%-13s %s=%s\n", label, k, m[k])
		} else {
			_, _ = fmt.Fprintf(w, "%-13s %s=%s\n", "", k, m[k])
		}
	}
}

// DescribeConditions writes a status.conditions block as an indented table
// (TYPE STATUS REASON LAST TRANSITION MESSAGE), colorizing the STATUS column by
// True/False/Unknown. It is a no-op for an empty slice.
func DescribeConditions(w io.Writer, colorEnabled bool, conds []metav1.Condition) {
	if len(conds) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "  Conditions:")
	tbl := NewTable(colorEnabled, "TYPE", "STATUS", "REASON", "LAST TRANSITION", "MESSAGE")
	for _, c := range conds {
		tbl.AddRow(
			TextCell(c.Type),
			ConditionStatusCell(string(c.Status)),
			TextCell(orNone(c.Reason)),
			TextCell(AgePtr(&c.LastTransitionTime)),
			TextCell(orNone(c.Message)),
		)
	}
	IndentTable(w, tbl, "    ")
}

// KV is one describe detail line (label + value), used by the generic describe
// path for per-resource spec/status fields.
type KV struct {
	Label string
	Value string
}

// DescribeDetails writes a block of aligned "Label: value" lines with the value
// column fixed at valueCol so the block scans vertically, kubectl-style.
func DescribeDetails(w io.Writer, kvs []KV, valueCol int) {
	for _, kv := range kvs {
		KeyValue(w, kv.Label, kv.Value, valueCol)
	}
}

// IndentTable renders tbl into an intermediate buffer and re-emits each line
// with the given prefix, so a Table can be nested inside a describe block.
func IndentTable(w io.Writer, tbl *Table, prefix string) {
	var buf bytes.Buffer
	_ = tbl.Render(&buf)
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		_, _ = fmt.Fprintf(w, "%s%s\n", prefix, line)
	}
}

func orNone(s string) string {
	if s == "" {
		return None
	}
	return s
}
