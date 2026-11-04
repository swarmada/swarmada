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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"time"

	// The distroless image has no zoneinfo; the site's timezone must still resolve.
	_ "time/tzdata"
)

// The inbound vocabulary: the only envelope types the bridge maps.
const (
	TypeDoorOpened       = "door.opened"
	TypeWorkOrderCreated = "workorder.created"
	TypeScheduleStarted  = "schedule.started"
	TypeCameraDetection  = "camera.detection"
)

// Envelope is what a caller posts: one occurrence, named by the caller's own id.
type Envelope struct {
	// ID is the caller's identifier for this occurrence. The same id from the same
	// caller is the same event, however many times it is delivered.
	ID string `json:"id"`
	// Type is one of the inbound vocabulary.
	Type string `json:"type"`
	// OccurredAt is when it happened, RFC 3339. Optional; the receive time is used
	// when absent, and the SemanticEvent then carries no occurredAt.
	OccurredAt string `json:"occurredAt,omitempty"`
	// Data is the type's fields, flat strings only.
	Data map[string]string `json:"data,omitempty"`
}

// Hours is the site's working day, used to tell a door opening after hours from one
// in hours. Opens and Closes are HH:MM in Location; Closes may be before Opens for a
// day that runs past midnight.
type Hours struct {
	Location string `json:"timezone"`
	Opens    string `json:"opens"`
	Closes   string `json:"closes"`
}

// CallerBinding binds one key id to the one SemanticEventSource it raises events for,
// and the inbound types it may send.
type CallerBinding struct {
	Source string   `json:"source"`
	Types  []string `json:"types"`
}

// Config is the bridge's declared vocabulary, read from a ConfigMap at start.
type Config struct {
	Namespace string                   `json:"namespace"`
	Hours     Hours                    `json:"hours"`
	Callers   map[string]CallerBinding `json:"callers"`
	// Zones a work order or a detection may name.
	Zones []string `json:"zones"`
	// Schedules a schedule.started event may name.
	Schedules []string `json:"schedules"`
	// MaxAge is how old an occurredAt may be, as a Go duration. Default 24h.
	MaxAge string `json:"maxAge,omitempty"`
}

// Event is what a mapped envelope becomes: the SemanticEvent's fields.
type Event struct {
	Name       string
	Source     string
	Type       string
	Payload    map[string]string
	OccurredAt *time.Time
}

// fieldSpec is one data key a type declares.
type fieldSpec struct {
	required bool
	// check validates the value beyond the common length and character rules.
	check func(m *Mapper, v string) *Refusal
}

// typeSpec declares one inbound type: its data keys, and the SemanticEvent type it
// maps to. Every key not listed here is refused, so a caller cannot smuggle a field
// a trigger author never saw.
type typeSpec struct {
	fields  map[string]fieldSpec
	outType func(m *Mapper, data map[string]string, at time.Time) (string, *Refusal)
}

var (
	dnsLabel  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	idChars   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	hhmm      = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// maxValueLen bounds every data value. A payload is something a trigger reads and an
// operator scans, not a data channel.
const maxValueLen = 128

func optional() fieldSpec { return fieldSpec{} }
func required() fieldSpec { return fieldSpec{required: true} }

func zoneField(req bool) fieldSpec {
	return fieldSpec{required: req, check: func(m *Mapper, v string) *Refusal {
		if !slices.Contains(m.cfg.Zones, v) {
			return refuse(CodeUndeclaredZone, "zone %q is not one this bridge was configured with (%v)", v, m.cfg.Zones)
		}
		return nil
	}}
}

var vocabulary = map[string]typeSpec{
	// A door opening becomes door.opened.after-hours or door.opened.in-hours. The
	// split is here because a TaskTrigger selects on type alone and carries no
	// condition (ADR-0063): the only way to trigger on "after hours" is a type that
	// says so.
	TypeDoorOpened: {
		fields: map[string]fieldSpec{"door": required(), "zone": zoneField(false)},
		outType: func(m *Mapper, _ map[string]string, at time.Time) (string, *Refusal) {
			if m.inHours(at) {
				return "door.opened.in-hours", nil
			}
			return "door.opened.after-hours", nil
		},
	},
	// A work order naming a zone becomes workorder.<task>.<zone>, one type per
	// declared zone, for the same reason: a trigger cannot read the zone out of the
	// payload (no parameter substitution, ADR-0063), so a zone's trigger is keyed by
	// a type that names it.
	TypeWorkOrderCreated: {
		fields: map[string]fieldSpec{
			"workOrder": required(),
			"zone":      zoneField(true),
			"task": {required: true, check: func(_ *Mapper, v string) *Refusal {
				if v != "clean" {
					return refuse(CodeUnsupportedWorkOrder, "work orders for task %q are not mapped; only \"clean\" is", v)
				}
				return nil
			}},
			"summary": optional(),
		},
		outType: func(_ *Mapper, d map[string]string, _ time.Time) (string, *Refusal) {
			return "workorder." + d["task"] + "." + d["zone"], nil
		},
	},
	TypeScheduleStarted: {
		fields: map[string]fieldSpec{
			"schedule": {required: true, check: func(m *Mapper, v string) *Refusal {
				if !slices.Contains(m.cfg.Schedules, v) {
					return refuse(CodeUndeclaredSchedule, "schedule %q is not one this bridge was configured with (%v)", v, m.cfg.Schedules)
				}
				return nil
			}},
		},
		outType: func(_ *Mapper, d map[string]string, _ time.Time) (string, *Refusal) {
			return "schedule.started." + d["schedule"], nil
		},
	},
	// A static camera's detection, with the fields the camera adapter puts on the
	// SemanticEvents it creates itself, for a camera adapter that reaches this bridge
	// and not the API server.
	TypeCameraDetection: {
		fields: map[string]fieldSpec{
			"class":  required(),
			"zone":   zoneField(true),
			"camera": required(),
			"confidence": {required: true, check: func(_ *Mapper, v string) *Refusal {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil || math.IsNaN(f) || f < 0 || f > 1 {
					return refuse(CodeBadValue, "confidence %q is not a number from 0 to 1", v)
				}
				return nil
			}},
			"model":        optional(),
			"modelVersion": optional(),
			"box":          optional(),
			"frameSha256": {check: func(_ *Mapper, v string) *Refusal {
				if !sha256Hex.MatchString(v) {
					return refuse(CodeBadValue, "frameSha256 is not 64 lower-case hex characters")
				}
				return nil
			}},
		},
		outType: func(_ *Mapper, d map[string]string, _ time.Time) (string, *Refusal) {
			return "camera.detection." + d["zone"], nil
		},
	},
}

// Mapper maps an authenticated envelope to an Event, or refuses it with the reason.
type Mapper struct {
	cfg    Config
	loc    *time.Location
	opens  int
	closes int
	maxAge time.Duration
	// skew is how far in the future an occurredAt may be: the callers' clocks are
	// not the bridge's.
	skew time.Duration
}

func minutes(s string) (int, error) {
	if !hhmm.MatchString(s) {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	h, _ := strconv.Atoi(s[:2])
	mm, _ := strconv.Atoi(s[3:])
	return h*60 + mm, nil
}

// NewMapper validates the config and builds a Mapper.
func NewMapper(cfg Config) (*Mapper, error) {
	m := &Mapper{cfg: cfg, maxAge: 24 * time.Hour, skew: 5 * time.Minute}
	if !dnsLabel.MatchString(cfg.Namespace) {
		return nil, fmt.Errorf("namespace %q is not a DNS label", cfg.Namespace)
	}
	if len(cfg.Callers) == 0 {
		return nil, errors.New("no callers are configured")
	}
	for id, b := range cfg.Callers {
		if !dnsLabel.MatchString(id) {
			return nil, fmt.Errorf("caller %q is not a DNS label", id)
		}
		if b.Source == "" {
			return nil, fmt.Errorf("caller %q names no source", id)
		}
		if len(b.Types) == 0 {
			return nil, fmt.Errorf("caller %q may send no types", id)
		}
		for _, t := range b.Types {
			if _, ok := vocabulary[t]; !ok {
				return nil, fmt.Errorf("caller %q: type %q is not in the vocabulary", id, t)
			}
		}
	}
	// Zones and schedules become part of a SemanticEvent type, which a TaskTrigger
	// author types by hand: keep them to one spelling.
	for _, z := range cfg.Zones {
		if !dnsLabel.MatchString(z) {
			return nil, fmt.Errorf("zone %q is not a DNS label", z)
		}
	}
	for _, s := range cfg.Schedules {
		if !dnsLabel.MatchString(s) {
			return nil, fmt.Errorf("schedule %q is not a DNS label", s)
		}
	}
	loc, err := time.LoadLocation(cfg.Hours.Location)
	if err != nil {
		return nil, fmt.Errorf("hours.timezone: %w", err)
	}
	m.loc = loc
	if m.opens, err = minutes(cfg.Hours.Opens); err != nil {
		return nil, fmt.Errorf("hours.opens: %w", err)
	}
	if m.closes, err = minutes(cfg.Hours.Closes); err != nil {
		return nil, fmt.Errorf("hours.closes: %w", err)
	}
	if m.opens == m.closes {
		return nil, errors.New("hours.opens equals hours.closes; say which hours are working hours")
	}
	if cfg.MaxAge != "" {
		if m.maxAge, err = time.ParseDuration(cfg.MaxAge); err != nil || m.maxAge <= 0 {
			return nil, fmt.Errorf("maxAge %q is not a positive duration", cfg.MaxAge)
		}
	}
	return m, nil
}

// inHours reports whether t falls in working hours, in the site's timezone.
func (m *Mapper) inHours(t time.Time) bool {
	lt := t.In(m.loc)
	now := lt.Hour()*60 + lt.Minute()
	if m.opens < m.closes {
		return now >= m.opens && now < m.closes
	}
	return now >= m.opens || now < m.closes
}

// ParseEnvelope decodes exactly one envelope with no unknown fields.
func ParseEnvelope(body []byte) (*Envelope, *Refusal) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var env Envelope
	if err := dec.Decode(&env); err != nil {
		return nil, refuse(CodeMalformed, "body is not one envelope {id, type, occurredAt, data}: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, refuse(CodeMalformed, "body has data after the envelope")
	}
	return &env, nil
}

func checkValue(key, v string) *Refusal {
	if len(v) > maxValueLen {
		return refuse(CodeBadValue, "%s is %d bytes; the limit is %d", key, len(v), maxValueLen)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return refuse(CodeBadValue, "%s contains a control character", key)
		}
	}
	return nil
}

// Map turns the caller's envelope into an Event.
func (m *Mapper) Map(caller string, env *Envelope, now time.Time) (*Event, *Refusal) {
	spec, ok := vocabulary[env.Type]
	if !ok {
		return nil, refuse(CodeUnknownType, "type %q is not in the vocabulary (%s, %s, %s, %s)", env.Type,
			TypeDoorOpened, TypeWorkOrderCreated, TypeScheduleStarted, TypeCameraDetection)
	}
	binding, ok := m.cfg.Callers[caller]
	if !ok || !slices.Contains(binding.Types, env.Type) {
		return nil, refuse(CodeNotPermitted, "caller %q may not send %q (may send %v)", caller, env.Type, binding.Types)
	}
	if env.ID == "" {
		return nil, refuse(CodeMissingField, "id is required: it is what makes a second delivery the same event")
	}
	if !idChars.MatchString(env.ID) {
		return nil, refuse(CodeBadValue, "id must be 1-128 characters of A-Z a-z 0-9 . _ : -")
	}
	keys := make([]string, 0, len(env.Data))
	for k := range env.Data {
		keys = append(keys, k)
	}
	slices.Sort(keys) // one refusal for one request, whatever the map order
	for _, k := range keys {
		if _, ok := spec.fields[k]; !ok {
			return nil, refuse(CodeUnknownField, "%s declares no field %q", env.Type, k)
		}
		if r := checkValue(k, env.Data[k]); r != nil {
			return nil, r
		}
	}
	names := make([]string, 0, len(spec.fields))
	for k := range spec.fields {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		f := spec.fields[k]
		v, present := env.Data[k]
		if !present || v == "" {
			if f.required {
				return nil, refuse(CodeMissingField, "%s requires %q", env.Type, k)
			}
			continue
		}
		if f.check != nil {
			if r := f.check(m, v); r != nil {
				return nil, r
			}
		}
	}

	at := now
	var occurred *time.Time
	if env.OccurredAt != "" {
		t, err := time.Parse(time.RFC3339, env.OccurredAt)
		if err != nil {
			return nil, refuse(CodeBadOccurredAt, "occurredAt %q is not RFC 3339", env.OccurredAt)
		}
		if t.After(now.Add(m.skew)) {
			return nil, refuse(CodeBadOccurredAt, "occurredAt %s is in the future", env.OccurredAt)
		}
		if now.Sub(t) > m.maxAge {
			return nil, refuse(CodeBadOccurredAt, "occurredAt %s is older than %s", env.OccurredAt, m.maxAge)
		}
		at, occurred = t, &t
	}
	outType, r := spec.outType(m, env.Data, at)
	if r != nil {
		return nil, r
	}
	payload := map[string]string{"caller": caller, "externalId": env.ID}
	for k, v := range env.Data {
		if v != "" {
			payload[k] = v
		}
	}
	return &Event{
		Name:       EventName(caller, env.ID),
		Source:     binding.Source,
		Type:       outType,
		Payload:    payload,
		OccurredAt: occurred,
	}, nil
}

// EventName is the SemanticEvent's name for one caller's occurrence id: the same
// (caller, id) is always the same name, so a second delivery collides on create and
// drafts nothing new. The caller is in the hash so that one caller cannot pre-empt
// another's event by guessing its id. Short, because the TaskTrigger controller
// names a draft <trigger>-<event> and labels it with the event name.
func EventName(caller, id string) string {
	sum := sha256.Sum256([]byte(caller + "\n" + id))
	return "evb-" + hex.EncodeToString(sum[:])[:16]
}
