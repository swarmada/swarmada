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

package controlstream

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The stop latch's SafetyStream messages (contract 1.4.0; RFC-0001
// #fleet-adapter-protocol-the-stop-latch) carry the field numbers the specification fixes. The
// check reads the compiled descriptors by name, not the generated Go types, so stubs that drop a
// message or a field, or renumber one, fail it at run time rather than failing to build.
func TestStopLatchWire_FieldNumbersAndRoundTrip(t *testing.T) {
	_ = fav1.File_fleet_adapter_v1_fleet_adapter_proto // registers the file's descriptors
	want := map[protoreflect.FullName]map[protoreflect.Name]protoreflect.FieldNumber{
		"fleet_adapter.v1.EstopClear":      {"estop_id": 1, "cleared_by": 2, "cleared_at_ms": 3, "ends_local_set": 4, "leader_term": 5},
		"fleet_adapter.v1.EstopClearAck":   {"estop_id": 1, "lifted_at_ms": 2},
		"fleet_adapter.v1.LocalStopReport": {"latched": 1, "changed_at_ms": 2, "source": 3, "set_id": 4, "unreadable": 5},
		// the oneof arms
		"fleet_adapter.v1.ControlPlaneSafetyMessage": {"estop": 3, "pause": 4, "estop_clear": 5},
		"fleet_adapter.v1.AdapterSafetyMessage":      {"estop_ack": 3, "pause_result": 4, "estop_clear_ack": 5, "local_stop": 6},
		// version-frozen
		"fleet_adapter.v1.Estop": {"estop_id": 1, "reason": 2, "issued_by": 3, "issued_at_ms": 4},
	}
	for name, fields := range want {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
		if err != nil {
			t.Errorf("%s is not in fleet_adapter.v1: %v", name, err)
			continue
		}
		md := d.(protoreflect.MessageDescriptor)
		for fname, num := range fields {
			f := md.Fields().ByName(fname)
			if f == nil || f.Number() != num {
				t.Errorf("%s.%s: %v, want field %d", name, fname, f, num)
			}
		}
		if name == "fleet_adapter.v1.LocalStopReport" || name == "fleet_adapter.v1.EstopClear" || name == "fleet_adapter.v1.EstopClearAck" {
			if md.Fields().Len() != len(fields) {
				t.Errorf("%s has %d fields, want %d", name, md.Fields().Len(), len(fields))
			}
			// A round trip through the descriptor: every field set, encoded, decoded, equal.
			m := dynamicpb.NewMessage(md)
			for i := 0; i < md.Fields().Len(); i++ {
				f := md.Fields().Get(i)
				switch f.Kind() {
				case protoreflect.BoolKind:
					m.Set(f, protoreflect.ValueOfBool(true))
				case protoreflect.StringKind:
					m.Set(f, protoreflect.ValueOfString("6f1c2b1e-9a43-4f3e-8d55-0f3f1c2d9e10"))
				case protoreflect.Int64Kind:
					m.Set(f, protoreflect.ValueOfInt64(1760000000000))
				case protoreflect.Uint64Kind:
					m.Set(f, protoreflect.ValueOfUint64(9))
				}
			}
			b, err := proto.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			got := dynamicpb.NewMessage(md)
			if err := proto.Unmarshal(b, got); err != nil || !proto.Equal(m, got) {
				t.Errorf("%s did not round-trip: %v", name, err)
			}
		}
	}
}
