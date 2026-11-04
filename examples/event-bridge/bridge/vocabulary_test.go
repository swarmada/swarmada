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

package bridge

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		Namespace: "warehouse-a",
		Hours:     Hours{Location: "UTC", Opens: "07:00", Closes: "19:00"},
		Callers: map[string]CallerBinding{
			"door-controller": {Source: "building-doors", Types: []string{TypeDoorOpened}},
			"work-orders":     {Source: "work-orders", Types: []string{TypeWorkOrderCreated}},
			"scheduler":       {Source: "schedules", Types: []string{TypeScheduleStarted}},
			"camera-adapter":  {Source: "static-cameras", Types: []string{TypeCameraDetection}},
		},
		Zones:     []string{"warehouse-a", "loading-bay"},
		Schedules: []string{"mapping-segment"},
	}
}

func mustMapper(t *testing.T) *Mapper {
	t.Helper()
	m, err := NewMapper(testConfig())
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	return m
}

func TestNewMapperRefusesABadConfig(t *testing.T) {
	cases := map[string]func(*Config){
		"no namespace":          func(c *Config) { c.Namespace = "" },
		"no callers":            func(c *Config) { c.Callers = nil },
		"caller with no source": func(c *Config) { c.Callers["scheduler"] = CallerBinding{Types: []string{TypeScheduleStarted}} },
		"caller with no types":  func(c *Config) { c.Callers["scheduler"] = CallerBinding{Source: "schedules"} },
		"caller with unknown type": func(c *Config) {
			c.Callers["scheduler"] = CallerBinding{Source: "schedules", Types: []string{"door.closed"}}
		},
		"zone not a DNS label":     func(c *Config) { c.Zones = []string{"Loading Bay"} },
		"schedule not a DNS label": func(c *Config) { c.Schedules = []string{"mapping_segment"} },
		"unknown timezone":         func(c *Config) { c.Hours.Location = "Mars/Olympus" },
		"opens not HH:MM":          func(c *Config) { c.Hours.Opens = "7am" },
		"opens equals closes":      func(c *Config) { c.Hours.Closes = "07:00" },
		"maxAge not a duration":    func(c *Config) { c.MaxAge = "a day" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := testConfig()
			mutate(&c)
			if _, err := NewMapper(c); err == nil {
				t.Fatal("NewMapper accepted the config")
			}
		})
	}
}

func TestParseEnvelope(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Code
	}{
		{"one envelope", `{"id":"wo-1","type":"workorder.created","data":{"zone":"loading-bay"}}`, ""},
		{"unknown top-level field", `{"id":"wo-1","type":"workorder.created","draft":false}`, CodeMalformed},
		{"trailing object", `{"id":"a","type":"door.opened"}{"id":"b","type":"door.opened"}`, CodeMalformed},
		{"trailing garbage", `{"id":"a","type":"door.opened"} x`, CodeMalformed},
		{"nested data", `{"id":"a","type":"door.opened","data":{"door":{"n":1}}}`, CodeMalformed},
		{"number in data", `{"id":"a","type":"door.opened","data":{"door":1}}`, CodeMalformed},
		{"array", `[{"id":"a","type":"door.opened"}]`, CodeMalformed},
		{"empty", ``, CodeMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, r := ParseEnvelope([]byte(tc.body))
			wantCode(t, r, tc.want)
		})
	}
}

func TestMapDoorOpened(t *testing.T) {
	m := mustMapper(t)
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 9, 28, 23, 0, 0, 0, time.UTC), "door.opened.after-hours"},
		{time.Date(2026, 9, 28, 6, 59, 0, 0, time.UTC), "door.opened.after-hours"},
		{time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC), "door.opened.in-hours"},
		{time.Date(2026, 9, 28, 18, 59, 0, 0, time.UTC), "door.opened.in-hours"},
		{time.Date(2026, 9, 28, 19, 0, 0, 0, time.UTC), "door.opened.after-hours"},
	} {
		env := &Envelope{ID: "door-7-1", Type: TypeDoorOpened, Data: map[string]string{"door": "north-7"}}
		ev, r := m.Map("door-controller", env, tc.at)
		wantCode(t, r, "")
		if ev.Type != tc.want {
			t.Errorf("at %s: type %q, want %q", tc.at.Format("15:04"), ev.Type, tc.want)
		}
		if ev.Source != "building-doors" || ev.Payload["door"] != "north-7" {
			t.Errorf("event %+v", ev)
		}
		if ev.OccurredAt != nil {
			t.Errorf("occurredAt set with none given: %v", ev.OccurredAt)
		}
	}
}

func TestMapJudgesHoursByOccurredAtNotReceipt(t *testing.T) {
	// A controller delivering a backlog in the morning is reporting a door opened in
	// the night; the hours are the event's, not the bridge's.
	m := mustMapper(t)
	env := &Envelope{ID: "door-7-2", Type: TypeDoorOpened, OccurredAt: "2026-09-28T02:10:00Z",
		Data: map[string]string{"door": "north-7"}}
	ev, r := m.Map("door-controller", env, time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC))
	wantCode(t, r, "")
	if ev.Type != "door.opened.after-hours" {
		t.Fatalf("type %q", ev.Type)
	}
	if ev.OccurredAt == nil || !ev.OccurredAt.Equal(time.Date(2026, 9, 28, 2, 10, 0, 0, time.UTC)) {
		t.Fatalf("occurredAt %v", ev.OccurredAt)
	}
}

func TestMapHoursThatRunPastMidnight(t *testing.T) {
	c := testConfig()
	c.Hours = Hours{Location: "UTC", Opens: "22:00", Closes: "06:00"}
	m, err := NewMapper(c)
	if err != nil {
		t.Fatal(err)
	}
	env := &Envelope{ID: "d", Type: TypeDoorOpened, Data: map[string]string{"door": "north-7"}}
	for at, want := range map[int]string{23: "door.opened.in-hours", 3: "door.opened.in-hours", 12: "door.opened.after-hours"} {
		ev, r := m.Map("door-controller", env, time.Date(2026, 9, 28, at, 0, 0, 0, time.UTC))
		wantCode(t, r, "")
		if ev.Type != want {
			t.Errorf("at %02d:00: %q, want %q", at, ev.Type, want)
		}
	}
}

func TestMapTheOtherTypes(t *testing.T) {
	m := mustMapper(t)
	cases := []struct {
		caller   string
		env      Envelope
		wantType string
		wantKey  string
		wantVal  string
	}{
		{"work-orders", Envelope{ID: "WO-1182", Type: TypeWorkOrderCreated,
			Data: map[string]string{"workOrder": "WO-1182", "zone": "loading-bay", "task": "clean"}},
			"workorder.clean.loading-bay", "workOrder", "WO-1182"},
		{"scheduler", Envelope{ID: "sched-2026-09-28", Type: TypeScheduleStarted,
			Data: map[string]string{"schedule": "mapping-segment"}},
			"schedule.started.mapping-segment", "schedule", "mapping-segment"},
		{"camera-adapter", Envelope{ID: "cam-1-cable-1727560000000", Type: TypeCameraDetection,
			Data: map[string]string{"class": "cable", "zone": "warehouse-a", "confidence": "0.870",
				"camera": "cam-1", "model": "floor-hazards", "modelVersion": "v3",
				"frameSha256": strings.Repeat("a", 64), "box": "0.1,0.2,0.3,0.4"}},
			"camera.detection.warehouse-a", "confidence", "0.870"},
	}
	for _, tc := range cases {
		t.Run(tc.env.Type, func(t *testing.T) {
			env := tc.env
			ev, r := m.Map(tc.caller, &env, t0)
			wantCode(t, r, "")
			if ev.Type != tc.wantType {
				t.Fatalf("type %q, want %q", ev.Type, tc.wantType)
			}
			if ev.Payload[tc.wantKey] != tc.wantVal {
				t.Fatalf("payload[%s] = %q, want %q", tc.wantKey, ev.Payload[tc.wantKey], tc.wantVal)
			}
			if ev.Payload["caller"] != tc.caller || ev.Payload["externalId"] != env.ID {
				t.Fatalf("payload does not name its caller and id: %v", ev.Payload)
			}
			if len(ev.Payload) > 16 {
				t.Fatalf("payload has %d keys; the API allows 16", len(ev.Payload))
			}
		})
	}
}

func TestMapRefusals(t *testing.T) {
	m := mustMapper(t)
	door := func(d map[string]string) Envelope { return Envelope{ID: "d-1", Type: TypeDoorOpened, Data: d} }
	order := func(d map[string]string) Envelope { return Envelope{ID: "wo-1", Type: TypeWorkOrderCreated, Data: d} }
	cam := func(conf string) Envelope {
		return Envelope{ID: "c-1", Type: TypeCameraDetection, Data: map[string]string{
			"class": "cable", "zone": "warehouse-a", "confidence": conf, "camera": "cam-1"}}
	}
	cases := []struct {
		name   string
		caller string
		env    Envelope
		want   Code
	}{
		{"type outside the vocabulary", "door-controller", Envelope{ID: "x", Type: "door.closed"}, CodeUnknownType},
		{"type the caller is not bound to", "door-controller", order(map[string]string{"workOrder": "1", "zone": "loading-bay", "task": "clean"}), CodeNotPermitted},
		{"no id", "door-controller", Envelope{Type: TypeDoorOpened, Data: map[string]string{"door": "n"}}, CodeMissingField},
		{"id with a space", "door-controller", Envelope{ID: "d 1", Type: TypeDoorOpened, Data: map[string]string{"door": "n"}}, CodeBadValue},
		{"id too long", "door-controller", Envelope{ID: strings.Repeat("a", 129), Type: TypeDoorOpened, Data: map[string]string{"door": "n"}}, CodeBadValue},
		{"undeclared data key", "door-controller", door(map[string]string{"door": "n", "draft": "false"}), CodeUnknownField},
		{"missing door", "door-controller", door(map[string]string{}), CodeMissingField},
		{"door names an undeclared zone", "door-controller", door(map[string]string{"door": "n", "zone": "roof"}), CodeUndeclaredZone},
		{"value too long", "door-controller", door(map[string]string{"door": strings.Repeat("n", 129)}), CodeBadValue},
		{"value with a newline", "door-controller", door(map[string]string{"door": "n\nx"}), CodeBadValue},
		{"work order names an undeclared zone", "work-orders", order(map[string]string{"workOrder": "1", "zone": "roof", "task": "clean"}), CodeUndeclaredZone},
		{"work order for a task not mapped", "work-orders", order(map[string]string{"workOrder": "1", "zone": "loading-bay", "task": "repaint"}), CodeUnsupportedWorkOrder},
		{"work order without a zone", "work-orders", order(map[string]string{"workOrder": "1", "task": "clean"}), CodeMissingField},
		{"undeclared schedule", "scheduler", Envelope{ID: "s", Type: TypeScheduleStarted, Data: map[string]string{"schedule": "night-shift"}}, CodeUndeclaredSchedule},
		{"confidence above one", "camera-adapter", cam("1.5"), CodeBadValue},
		{"confidence not a number", "camera-adapter", cam("high"), CodeBadValue},
		{"confidence NaN", "camera-adapter", cam("NaN"), CodeBadValue},
		{"occurredAt not RFC 3339", "door-controller", Envelope{ID: "d", Type: TypeDoorOpened, OccurredAt: "yesterday", Data: map[string]string{"door": "n"}}, CodeBadOccurredAt},
		{"occurredAt in the future", "door-controller", Envelope{ID: "d", Type: TypeDoorOpened, OccurredAt: t0.Add(10 * time.Minute).Format(time.RFC3339), Data: map[string]string{"door": "n"}}, CodeBadOccurredAt},
		{"occurredAt older than maxAge", "door-controller", Envelope{ID: "d", Type: TypeDoorOpened, OccurredAt: t0.Add(-25 * time.Hour).Format(time.RFC3339), Data: map[string]string{"door": "n"}}, CodeBadOccurredAt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			_, r := m.Map(tc.caller, &env, t0)
			wantCode(t, r, tc.want)
		})
	}
}

func TestEventName(t *testing.T) {
	label := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	a := EventName("door-controller", "door-7-1")
	if a != EventName("door-controller", "door-7-1") {
		t.Fatal("EventName is not deterministic")
	}
	if a == EventName("work-orders", "door-7-1") {
		t.Fatal("two callers' same id share a name; one caller could suppress another's event")
	}
	if a == EventName("door-controller", "door-7-2") {
		t.Fatal("two ids share a name")
	}
	if !strings.HasPrefix(a, "evb-") || !label.MatchString(a) || len(a) > 24 {
		// The TaskTrigger controller names a draft <trigger>-<event> and labels it
		// with the event name; a label value is at most 63 characters.
		t.Fatalf("name %q: want evb-<hex>, a DNS label, short enough to leave room for the trigger name", a)
	}
}
