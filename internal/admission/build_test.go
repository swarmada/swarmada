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

package admission

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The §9.1.2.5 merge order — discovered values, then the class template, then operator
// overrides, then the zone. Both admission paths run through this, so a mistake here is a
// mistake in every robot the fleet admits.

func discoveredFixture() *fleetv1.DiscoveredRobot {
	return &fleetv1.DiscoveredRobot{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-acme-a3f9", Namespace: "warehouse-a"},
		Status: fleetv1.DiscoveredRobotStatus{
			Manufacturer:   "Acme",
			Model:          "Origin",
			AdapterVersion: "1.0",
			ReportedHardware: []fleetv1.DiscoveredHardwareComponent{
				{Name: "cam", Type: fleetv1.HardwareTypeCamera, Status: fleetv1.HardwareHealthy},
			},
		},
	}
}

func classFixture() *fleetv1.RobotClass {
	return &fleetv1.RobotClass{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-class", Namespace: "warehouse-a"},
		Spec: fleetv1.RobotClassSpec{
			Manufacturer: "Acme", Model: "Origin",
			BaseAdapter: fleetv1.BaseAdapterRef{Name: "acme-adapter", Version: "2.0"},
			Hardware:    []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}},
		},
	}
}

func TestBuildRobot_ClassAppliesWholesaleAndHardwareUnions(t *testing.T) {
	p := Params{Zone: "zone-aisle-c1", RobotClass: "acme-class", Name: "amr-acme-042"}
	robot, err := BuildRobot(discoveredFixture(), p, classFixture(), "warehouse-a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if robot.Name != "amr-acme-042" || robot.Namespace != "warehouse-a" {
		t.Errorf("identity = %s/%s", robot.Namespace, robot.Name)
	}
	// The class pins the adapter version, overriding the version the robot reported: the
	// class is a statement about what this fleet runs, not about what the robot arrived with.
	if robot.Spec.Adapter.Name != "acme-adapter" || robot.Spec.Adapter.Version != "2.0" {
		t.Errorf("adapter = %+v, want acme-adapter/2.0 from the class", robot.Spec.Adapter)
	}
	if robot.Spec.Manufacturer != "Acme" || robot.Spec.Zone != "zone-aisle-c1" || robot.Spec.RobotClass != "acme-class" {
		t.Errorf("spec = %+v", robot.Spec)
	}
	// Hardware is a UNION: the robot reported a camera, the class declares a lidar, and the
	// admitted robot has both. Taking the class wholesale would erase a component that
	// physically exists; taking only the report would drop one the class knows about.
	if len(robot.Spec.Hardware) != 2 {
		t.Fatalf("hardware must union report and template, got %+v", robot.Spec.Hardware)
	}
	if robot.Spec.Hardware[0].Name != "cam" || robot.Spec.Hardware[1].Name != "lidar" {
		t.Errorf("order must be reported-then-class-only: %+v", robot.Spec.Hardware)
	}
}

func TestMergeHardware_ReportedEntryWinsOverTheTemplate(t *testing.T) {
	// The safety-relevant direction (§9.1.2.5 Step 2). A class that overstates a component —
	// here a 45 kg platform on a robot that reports 40 kg — would let the scheduler assign a
	// payload the machine cannot carry.
	forty, fortyFive := 40.0, 45.0
	reported := []fleetv1.HardwareComponent{
		{Name: "load-platform", Type: fleetv1.HardwareTypeLoadPlatform, MaxPayloadKg: &forty},
	}
	class := []fleetv1.HardwareComponent{
		{Name: "load-platform", Type: fleetv1.HardwareTypeLoadPlatform, MaxPayloadKg: &fortyFive},
		{Name: "lidar", Type: fleetv1.HardwareTypeLidar},
	}
	got := MergeHardware(reported, class)
	if len(got) != 2 {
		t.Fatalf("want the shared component once plus the class-only one, got %+v", got)
	}
	if got[0].MaxPayloadKg == nil || *got[0].MaxPayloadKg != 40 {
		t.Errorf("the reported capacity must win, got %+v", got[0].MaxPayloadKg)
	}
	if got[1].Name != "lidar" {
		t.Errorf("a class-only component must still be inherited, got %+v", got[1])
	}
}

func TestMergeHardware_ReplacementIsWholeEntry(t *testing.T) {
	// Explicitly specified: no partial field merging within a list entry. A class attribute
	// discovery cannot report is DROPPED for a component the robot also reports, rather than
	// filled in — a spec blending measured and assumed values would match neither source and
	// give no way to tell which was which.
	reach := int32(850)
	reported := []fleetv1.HardwareComponent{{Name: "arm", Type: fleetv1.HardwareTypeArm}}
	class := []fleetv1.HardwareComponent{{Name: "arm", Type: fleetv1.HardwareTypeArm, ReachMm: &reach}}

	got := MergeHardware(reported, class)
	if len(got) != 1 {
		t.Fatalf("want one entry, got %+v", got)
	}
	if got[0].ReachMm != nil {
		t.Errorf("whole-entry replacement must not backfill class attributes, got reachMm=%v", *got[0].ReachMm)
	}
}

func TestMergeHardware_EmptySourcesDegradeToTheOther(t *testing.T) {
	class := []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}}
	reported := []fleetv1.HardwareComponent{{Name: "cam", Type: fleetv1.HardwareTypeCamera}}

	if got := MergeHardware(nil, class); len(got) != 1 || got[0].Name != "lidar" {
		t.Errorf("a robot reporting nothing inherits the template: %+v", got)
	}
	if got := MergeHardware(reported, nil); len(got) != 1 || got[0].Name != "cam" {
		t.Errorf("a class with no hardware leaves the report intact: %+v", got)
	}
	if got := MergeHardware(nil, nil); got != nil {
		t.Errorf("neither source means unset, not empty: %+v", got)
	}
}

func TestBuildRobot_StampsTheAnnouncedRobotID(t *testing.T) {
	// The DiscoveredRobot's name is the robot_id telemetry arrives under. With a --name
	// override the Robot's own name differs, so a Robot relying on the defaulting webhook's
	// name fallback would carry the wrong id and never resolve its telemetry.
	p := Params{Zone: "z", Adapter: "a", Name: "amr-042"}
	robot, err := BuildRobot(discoveredFixture(), p, nil, "warehouse-a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := robot.Annotations[fleetv1.RobotIDAnnotation]; got != "dr-acme-a3f9" {
		t.Errorf("robot-id = %q, want the announced id even though the Robot was renamed", got)
	}
}

func TestBuildRobot_WithoutAClassCarriesDiscoveredHardware(t *testing.T) {
	p := Params{Zone: "z", Adapter: "acme-adapter"}
	robot, err := BuildRobot(discoveredFixture(), p, nil, "warehouse-a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// No template to draw from, so the robot's own inventory is the only source. The adapter
	// version stays the reported one.
	if len(robot.Spec.Hardware) != 1 || robot.Spec.Hardware[0].Name != "cam" {
		t.Errorf("hardware = %+v, want the discovered inventory", robot.Spec.Hardware)
	}
	if robot.Spec.Adapter.Version != "1.0" {
		t.Errorf("adapter version = %q, want the reported 1.0", robot.Spec.Adapter.Version)
	}
	// Capabilities and models come only from a class. Inventing them from discovered string
	// lists would make a robot schedulable for work nobody confirmed it can do.
	if robot.Spec.Capabilities != nil || robot.Spec.InstalledModels != nil {
		t.Errorf("no class means no capabilities/models: %+v / %+v", robot.Spec.Capabilities, robot.Spec.InstalledModels)
	}
}

func TestBuildRobot_OperatorOverridesBeatDiscoveredValues(t *testing.T) {
	p := Params{Zone: "z", Adapter: "custom-adapter", Manufacturer: "Acme", Model: "X1"}
	robot, err := BuildRobot(discoveredFixture(), p, nil, "warehouse-a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if robot.Spec.Manufacturer != "Acme" || robot.Spec.Model != "X1" || robot.Spec.Adapter.Name != "custom-adapter" {
		t.Errorf("operator overrides not applied: %+v", robot.Spec)
	}
	// An unset override falls back rather than blanking the discovered value.
	if robot.Name != "dr-acme-a3f9" {
		t.Errorf("an unnamed admission keeps the discovered name, got %q", robot.Name)
	}
}

func TestBuildRobot_UnresolvableAdapterIsRefused(t *testing.T) {
	// Fail closed. A Robot with no adapter cannot be driven, and admitting one would put an
	// unreachable robot into the schedulable pool.
	_, err := BuildRobot(discoveredFixture(), Params{Zone: "z"}, nil, "warehouse-a")
	if err == nil || !strings.Contains(err.Error(), "no adapter") {
		t.Fatalf("want an adapter-resolution error, got %v", err)
	}
}

func TestParams_RoundTripThroughTheAnnotation(t *testing.T) {
	// The annotation is the only channel between the operator's command and the controller
	// that acts on it; a field lost in transit becomes a misconfigured robot, not an error.
	want := Params{
		Name: "amr-1", Zone: "z1", RobotClass: "c1",
		Adapter: "a1", Dock: "d1", Manufacturer: "Acme", Model: "X1",
	}
	encoded, err := want.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeParams(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", got, want)
	}
}

func TestDecodeParams_RejectsUnusableMarks(t *testing.T) {
	// A corrupted payload must not resolve to defaults: zone has no safe default, and
	// guessing would place a robot somewhere nobody chose.
	for _, tc := range []struct{ name, in string }{
		{"malformed", "{not json"},
		{"no zone", `{"name":"amr-1"}`},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeParams(tc.in); err == nil {
				t.Fatalf("expected %s to be refused", tc.name)
			}
		})
	}
}

func TestMergeCharging_EmptyWhenNeitherSourceExists(t *testing.T) {
	if got := MergeCharging(nil, ""); got != nil {
		t.Errorf("no class default and no --dock must leave charging unset, got %+v", got)
	}
	if got := MergeCharging(nil, "dock-3"); got == nil || got.DockName != "dock-3" {
		t.Errorf("--dock alone must produce a config, got %+v", got)
	}
}

// The payload clamp (§9.1.2.5). spec.hardware being right is not enough — this is the number
// the scheduler consults at dispatch, so a class that overstates capacity here puts a load on
// a robot that cannot carry it.

func TestClampConstraints_ClassOverstatingCapacityIsLowered(t *testing.T) {
	forty, fortyFive := 40.0, 45.0
	hw := []fleetv1.HardwareComponent{{Name: "load-platform", MaxPayloadKg: &forty}}
	class := &fleetv1.ClassConstraints{MaxPayloadKg: &fortyFive}

	got := ResolveConstraintsFromHardware(class, hw)
	if got.MaxPayloadKg == nil || *got.MaxPayloadKg != 40 {
		t.Fatalf("constraint must be clamped to the reported 40 kg, got %v", got.MaxPayloadKg)
	}
	// The class object is shared with every other admission reading that RobotClass from
	// cache; clamping through its pointer would corrupt the template itself.
	if *class.MaxPayloadKg != 45 {
		t.Errorf("the class template was mutated: maxPayloadKg = %v", *class.MaxPayloadKg)
	}
}

func TestClampConstraints_DeliberatePolicyBelowCapacityIsKept(t *testing.T) {
	// A fleet capping a 40 kg platform at 25 kg has made a decision. Raising the constraint
	// to match the hardware would silently discard it — the clamp only ever lowers.
	forty, twentyFive := 40.0, 25.0
	hw := []fleetv1.HardwareComponent{{Name: "load-platform", MaxPayloadKg: &forty}}

	got := ResolveConstraintsFromHardware(&fleetv1.ClassConstraints{MaxPayloadKg: &twentyFive}, hw)
	if got.MaxPayloadKg == nil || *got.MaxPayloadKg != 25 {
		t.Errorf("a constraint below capacity must survive, got %v", got.MaxPayloadKg)
	}
}

func TestClampConstraints_UnknownCapacityChangesNothing(t *testing.T) {
	// Absence of a reading is not evidence of a limit. Hardware that reports no payload
	// rating must leave the class constraint exactly as declared, not zero it.
	fortyFive := 45.0
	hw := []fleetv1.HardwareComponent{{Name: "cam", Type: fleetv1.HardwareTypeCamera}}

	got := ResolveConstraintsFromHardware(&fleetv1.ClassConstraints{MaxPayloadKg: &fortyFive}, hw)
	if got.MaxPayloadKg == nil || *got.MaxPayloadKg != 45 {
		t.Errorf("an unrated inventory must not alter the constraint, got %v", got.MaxPayloadKg)
	}
	// Nothing declared AND nothing measured: there is no source for a limit, so the robot
	// stays unconstrained rather than acquiring a fabricated one.
	if got := ResolveConstraintsFromHardware(nil, hw); got != nil {
		t.Errorf("with no rating to derive from, the constraint stays absent, got %+v", got)
	}
}

func TestClampConstraints_CapacityIsTheLargestComponentNotTheSum(t *testing.T) {
	// Two 40 kg platforms are not an 80 kg robot unless a load can be split across both,
	// which nothing specifies. Summing would raise the cap above what either can carry.
	forty, seventy := 40.0, 70.0
	hw := []fleetv1.HardwareComponent{
		{Name: "platform-a", MaxPayloadKg: &forty},
		{Name: "platform-b", MaxPayloadKg: &forty},
	}
	got := ResolveConstraintsFromHardware(&fleetv1.ClassConstraints{MaxPayloadKg: &seventy}, hw)
	if got.MaxPayloadKg == nil || *got.MaxPayloadKg != 40 {
		t.Errorf("capacity must be the largest single component, got %v", got.MaxPayloadKg)
	}
}

func TestBuildRobot_ClampsAgainstTheMergedHardware(t *testing.T) {
	// End to end, and the ordering that makes it work: the clamp runs against the UNION, so
	// it sees the robot's reported platform rather than the class's claim about it. Running
	// it before the merge would clamp against the template and change nothing.
	forty, fortyFive := 40.0, 45.0
	dr := discoveredFixture()
	dr.Status.ReportedHardware = []fleetv1.DiscoveredHardwareComponent{
		{Name: "load-platform", Type: fleetv1.HardwareTypeLoadPlatform, MaxPayloadKg: &forty},
	}
	class := classFixture()
	class.Spec.Hardware = []fleetv1.HardwareComponent{
		{Name: "load-platform", Type: fleetv1.HardwareTypeLoadPlatform, MaxPayloadKg: &fortyFive},
	}
	class.Spec.DefaultConstraints = &fleetv1.ClassConstraints{MaxPayloadKg: &fortyFive}

	robot, err := BuildRobot(dr, Params{Zone: "z", RobotClass: "acme-class"}, class, "warehouse-a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if robot.Spec.Constraints == nil || *robot.Spec.Constraints.MaxPayloadKg != 40 {
		t.Fatalf("constraint must match the robot's actual platform, got %+v", robot.Spec.Constraints)
	}
	if *class.Spec.DefaultConstraints.MaxPayloadKg != 45 {
		t.Errorf("the RobotClass template was mutated by an admission")
	}
}

// Derivation: where nothing declares a payload cap, the hardware's own rating becomes one.
// An absent constraint reads as unlimited at dispatch, which is the worse failure than an
// overstated one — an overstated cap is at least bounded by the class author's intent.

func TestResolveConstraints_DerivedWhenNothingDeclaresACap(t *testing.T) {
	forty := 40.0
	hw := []fleetv1.HardwareComponent{{Name: "load-platform", MaxPayloadKg: &forty}}

	// No constraints object at all.
	got := ResolveConstraintsFromHardware(nil, hw)
	if got == nil || got.MaxPayloadKg == nil || *got.MaxPayloadKg != 40 {
		t.Fatalf("want a derived 40 kg cap, got %+v", got)
	}

	// A constraints object that declares other fields but no payload cap. The siblings must
	// survive — deriving one field is not licence to rebuild the object.
	floor := int32(20)
	partial := &fleetv1.ClassConstraints{MinBatteryPctForAction: &floor}
	got = ResolveConstraintsFromHardware(partial, hw)
	if got.MaxPayloadKg == nil || *got.MaxPayloadKg != 40 {
		t.Fatalf("want a derived cap alongside the declared fields, got %+v", got)
	}
	if got.MinBatteryPctForAction == nil || *got.MinBatteryPctForAction != 20 {
		t.Errorf("an unrelated declared constraint was lost: %+v", got)
	}
	if partial.MaxPayloadKg != nil {
		t.Error("the caller's constraints object was mutated")
	}
}

func TestResolveConstraints_DerivesFromTheRobotsOwnReportWithNoClass(t *testing.T) {
	// The no-class admission path. The robot's report is the only statement about it, and it
	// is enough to bound what the scheduler may assign.
	forty := 40.0
	dr := discoveredFixture()
	dr.Status.ReportedHardware = []fleetv1.DiscoveredHardwareComponent{
		{Name: "load-platform", Type: fleetv1.HardwareTypeLoadPlatform, MaxPayloadKg: &forty},
	}
	robot, err := BuildRobot(dr, Params{Zone: "z", Adapter: "a"}, nil, "warehouse-a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if robot.Spec.Constraints == nil || *robot.Spec.Constraints.MaxPayloadKg != 40 {
		t.Fatalf("a robot admitted without a class is still bounded by its own report, got %+v", robot.Spec.Constraints)
	}
}

func TestResolveConstraints_ReportedZeroIsAReadingNotAnAbsence(t *testing.T) {
	// MapReportedHardware preserves the difference between "unknown" (nil) and "measured
	// zero" all the way from the wire; discarding it here would put that distinction back.
	// A platform rated 0 kg carries nothing, and the cap must say so.
	zero := 0.0
	hw := []fleetv1.HardwareComponent{{Name: "load-platform", MaxPayloadKg: &zero}}

	got := ResolveConstraintsFromHardware(nil, hw)
	if got == nil || got.MaxPayloadKg == nil || *got.MaxPayloadKg != 0 {
		t.Fatalf("a reported zero must derive a zero cap, got %+v", got)
	}
}
