# API Design Principles

This document defines the conventions every Swarmada custom resource and the Fleet
Adapter protocol follow. It is descriptive of the current API and prescriptive for
new additions: a change that violates these principles should be reworked or should
change this document with explicit rationale.

The normative specification is [RFC-0001](../rfcs/dist/RFC-0001-core-spec.md).
Swarmada is Kubernetes-native and follows the upstream
[Kubernetes API Conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md);
where this document is silent, those conventions apply.

## Resource shape

- **Group and version.** All resources live in `swarmada.io/v1` and are
  **namespace-scoped**. The namespace is the administrative boundary; a `FleetZone`
  hierarchy provides spatial structure within it.
- **spec / status split.** `spec` is desired state, authored by operators. `status`
  is observed state, authored only by controllers. Every resource with runtime
  state enables the `status` subresource, so status updates do not bump
  `metadata.generation` and RBAC can grant status writes separately.
- **Root objects.** Each kind embeds `metav1.TypeMeta` and `metav1.ObjectMeta`, and
  declares `Spec` and (where stateful) `Status`, plus a `List` type. Registration is
  via `SchemeBuilder.Register` in the package `init`.

## Field conventions

- **Optional scalars are pointers.** An optional numeric or boolean field is a
  pointer (`*float64`, `*int32`, `*bool`) so that *unset* is distinguishable from a
  meaningful zero. A load platform with `maxPayloadKg` unset means "unknown," not
  "0 kg." Do not use a bare value type for a field where zero is a real,
  different-from-absent value.
- **`+optional` and `omitempty` travel together.** Optional fields carry the
  `+optional` marker and an `omitempty` JSON tag; required fields carry neither and
  are validated (below).
- **Requiredness is validated, not assumed.** Required fields use
  `+kubebuilder:validation:Required` or a constraint such as
  `+kubebuilder:validation:MinLength=1`. Do not rely on Go zero values to signal
  presence.
- **Enumerations are named string types.** An enumerated field is its own `string`
  type with `+kubebuilder:validation:Enum=...` and exported named constants for
  every value. Never accept a free-form string where a closed set is intended.
- **Bounded numbers declare their bounds.** Use `+kubebuilder:validation:Minimum` /
  `Maximum`; percentages are `0`–`100`, fractions `0`–`1`.
- **Structured strings declare their format.** Versions use a semver pattern
  (`+kubebuilder:validation:Pattern`); free-form identifiers use `MinLength`.
- **Defaults belong in the schema.** Use `+kubebuilder:default=...` so the API
  server applies defaults server-side, rather than defaulting in controller code.
  *Exception (ADR-0012):* when a field participates in a multi-level resolution — a
  per-object value overriding a namespace-wide `SwarmadaConfig` default overriding a
  built-in constant — the field is a nil-able pointer with **no** schema default, so
  the controller can distinguish "unset (inherit)" from an explicit value. A schema
  default would erase that distinction. `RobotProbe.spec.{failureThreshold,
  recoveryThreshold}` are the reference case; such fields still declare their bounds
  (`Minimum=1`) and the resolution fail-safe is documented on the resolver.

## Lists and merge semantics

- **Named lists are maps.** A list keyed by a `name` field is annotated
  `+listType=map` with `+listMapKey=name`, so server-side apply merges by key rather
  than replacing the whole list. Lists of scalars use `+listType=set`.
- **Template merge is union-by-name.** When a `RobotClass` merges into a `Robot` at
  admission, scalar fields let the `Robot` value win if present, and list fields
  merge union-by-name: a `Robot` entry with the same name fully replaces the class
  entry. There is no partial per-element merge. The merge resolves once, at
  admission, and is persisted on the `Robot`; changing the class does not
  retroactively mutate admitted robots.

## Cross-field validation

Invariants that span fields are enforced in the schema with CEL
(`+kubebuilder:validation:XValidation`), not left to controllers — the API server
rejects an invalid object before any controller sees it. Examples in the current
API: a hardware component of type `Custom` must also set `customType`; a charging
config's `targetBatteryPct` must exceed its `minBatteryPctToCharge`.

## Status conventions

- **Conditions are standard.** Status carries `[]metav1.Condition` annotated
  `+listType=map` / `+listMapKey=type`, following the upstream condition contract.
- **`observedGeneration`.** Controllers record the `metadata.generation` a status
  was computed from, so readers can tell whether status reflects the latest spec.
- **Status is a throttled projection, not a live feed.** To keep high-cadence
  telemetry off etcd (see the RA-1 discussion in
  [architecture.md](architecture.md)), fields like battery level and position are
  written only on a material transition and are coarse by design. Tooling that needs
  live values queries the telemetry backend, not the Kubernetes API. Do not add a
  status field that would require a write on every telemetry tick.

## Opaque and free-form data

Some payloads are genuinely schema-less (a task's vendor-specific parameters). Model
them explicitly, never as `map[string]interface{}` or a bare `interface{}` — those
cannot be rendered into an OpenAPI schema or deep-copied, and code generation fails
on them. Use one of:

- a typed wrapper holding raw bytes (e.g. `Payload { Raw []byte }`), or
- a field annotated `+kubebuilder:pruning:PreserveUnknownFields`, or
- an empty struct when the field is intentionally always empty.

A resource whose `spec` is meant to be empty (a read-only, status-only resource)
declares an empty spec struct rather than an untyped map.

## Versioning and compatibility

- **The contract is frozen at RFC-0001 v1.0.** The freeze covers the fleet-adapter
  wire contract version and the CRD API versions published with RFC-0001 v1.0. It does
  not mean wire contract `1.0`. Until then, `0.x` revisions keep refining the contract
  and keep changes to existing contracts to a minimum.
- **The contract-change process.** A change is a new contract version, with release
  notes and migration notes, specified by an amendment or a new RFC
  ([`rfcs/README.md`](../rfcs/README.md#amending-an-accepted-rfc)). No change is made in
  place to the proto definitions, a CRD's rules and semantics, or the normative text of
  RFC-0001. A change that does not touch the wire is a patch bump by default. A change
  that binds adapters is a minor bump, and the supported range moves. A change binds
  adapters if it adds, changes or removes an obligation for adapters. A
  conformance check that tests an obligation already stated moves the suite revision
  only. The contract version does not change. A change that binds adapters for safety
  also raises the lowest supported contract version to the new one. Normative text is a
  sentence with an RFC 2119 keyword, a schema block, a state table such a sentence
  cites, and the RBAC matrix. Maturity notes, paragraphs that describe what holds
  "at v0.4", Known-gaps cells, examples, citations and errata are informative and
  can still change. The policy is stated in RFC-0001
  [Resource model](../rfcs/dist/RFC-0001-core-spec.md#resource-model).
- **The fleet-adapter window.** At RFC-0001 v0.4 the contract is `1.4.0`, and the
  supported range is `>=1.3.0 <1.5.0`. An adapter outside it moves to a version
  inside it, in its own repository; the window is not widened.
- **What lands without a contract change.** Code that brings the reference
  implementation into line with the text. A conformance check that tests an obligation
  the text already states, as a suite revision, which does not move the contract
  version. A check that adds an obligation binds adapters, so it goes through the
  contract-change process.
- **Safety rules, at every stage.** A change to a safety rule (stops, the action table,
  the robot stop table, the invariants) goes through the contract-change process, and the
  conformance suite is updated in the same version. A change that weakens a guarantee says
  so first. Every unmet rule keeps its Not-met note.
- **Names scripts depend on are part of the contract.** Annotation names and labels are
  inside it. Event reasons, metrics, CLI flags, printer columns, doc comments and
  message strings are outside it, and a change to one is recorded in `CHANGELOG.md`.
- **CRD versions follow Kubernetes API rules.** Within `swarmada.io/v1` only
  backward-compatible changes are made, such as a new optional field. An incompatible
  schema change introduces a new API version (`v2`), served alongside `v1` for at least
  one revision, with conversion and a migration note.
- **`stage:` describes maturity and semantics, not schema compatibility.** On `alpha`,
  rules and semantics change only through the contract-change process, and a CRD never
  takes an incompatible schema change within `v1`. On `beta`, a resource changes by
  addition, and any other change needs a deprecation announced at least one revision
  ahead. A `stable` resource does not change before the next major revision. Each revision
  states which resources changed stage. At RFC-0001 v1.0 the core resources are declared
  `stable` in `v1`. The core resources are the thirteen original kinds: `Robot`,
  `RobotClass`, `DiscoveredRobot`, `FleetAction`, `FleetTask`, `FleetZone`, `FleetAdapter`,
  `RobotProbe`, `SwarmadaConfig`, `ModelPolicy`, `ModelRollout`, `FirmwareRollout` and
  `ZoneMaintenance`.
- **The rules the contract was built under.** A new optional field
  is added to `swarmada.io/v1` without a version bump, and changing the meaning of a
  field, removing a field, or tightening validation is breaking. The Fleet Adapter
  protocol keeps its field numbers stable and never reuses one. These rules still
  describe the API's shape, and the principles in this document still govern code
  that implements it.

## Discoverability

- **Short names.** Every kind declares a `+kubebuilder:resource:shortName` (`rob`,
  `fact`, `ft`, `fz`, `rc`, `fadapt`, `dr`, `rp`, `sc`, `mp`, `mr`, `fwr`, `zm`, `en`,
  `cep`, `cwt`, `rs`, `as`, `sev`, `ses`, `tt`) for CLI economy — twenty-one kinds,
  twenty-one short names.
- **Print columns.** Kinds declare `+kubebuilder:printcolumn` for the fields an
  operator scans in `kubectl get` — phase, key spec values, and age.

## Read-only resources and admission

A resource the operator observes but does not author (for example, a discovered
robot awaiting admission) has an empty `spec` and is driven entirely through
`status`. Admission and other state changes happen through a deliberate action
(`swarmctl` and, where applicable, an admission gate), never by hand-editing such a
resource. This keeps the discovery → admission boundary explicit and auditable.

## Naming

- Go identifiers are PascalCase; JSON field names are camelCase.
- Capability identifiers are dotted and lowercase (`navigation.2d`, `estop.receive`).
- Manufacturer identifiers are lowercase without spaces (`acme-robotics`).
- Enum values are PascalCase strings (`Healthy`, `TwoPhase`, `RollingUpdate`).

## Further reading

- [Kubernetes API Conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
- [architecture.md](architecture.md) — how these resources fit the control plane
- [RFC-0001](../rfcs/dist/RFC-0001-core-spec.md) — the normative specification
- [Fleet Adapter protocol](../proto/fleet_adapter/v1/fleet_adapter.proto) — the vendor-neutral gRPC contract
