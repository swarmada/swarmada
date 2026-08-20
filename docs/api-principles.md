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

- **Additive within a version.** New optional fields may be added to `swarmada.io/v1`
  without a version bump. Changing the meaning of a field, removing a field, or
  tightening validation is breaking.
- **Breaking changes get a new version.** A breaking API change introduces
  `swarmada.io/v2` with conversion, rather than mutating `v1` in place. The Fleet
  Adapter protocol follows the same rule at the proto layer: field numbers are
  stable and never reused, and a breaking change moves to a new package
  (`fleet_adapter.v2`), negotiated at connect time.

## Discoverability

- **Short names.** Every kind declares a `+kubebuilder:resource:shortName` (`rob`,
  `fact`, `ft`, `fz`, `rc`, `fa`, `dr`, `rp`, `sc`, `mp`, `mr`, `fwr`, `zm`) for CLI
  economy — thirteen kinds, thirteen short names.
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
