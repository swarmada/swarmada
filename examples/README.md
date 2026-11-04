# Examples

Runnable, end-to-end scenarios that show how to use Swarmada. Each example is
**executable documentation**: a complete scenario with the observable result that
means it worked. CI runs one of them,
`warehouse-quickstart` (`make quickstart-test`, on the LIVE path with a real sim
adapter); the others are run by hand.

This is why examples live here and not in `docs/` (prose) or `config/samples/`
(raw manifests): an example is a complete scenario a reader can run, with the
steps and the expected outcome stated.

## Layout

Each example is a self-contained directory:

```
examples/<name>/
├── README.md        # what it shows, prerequisites, steps, expected result
├── *.yaml           # manifests the example applies (may reuse config/samples)
└── run.sh           # optional: the steps as an executable script CI can call
```

An example's `README.md` states its prerequisites, the exact commands to run,
and the observable result that means it worked — concrete enough to assert on.

## Index

| Example | Shows |
| :------ | :---- |
| [warehouse-quickstart](warehouse-quickstart/) | Bring up a simulated fleet in one zone and schedule tasks across it. |
| [full-surface-demo](full-surface-demo/) | `make demo-test`: drive every consumed adapter field, the metrics-contract alerts, and ControlStream, SafetyStream and EdgeStream through a real control plane on `kind`, with the projected steps labelled. |
| [edge-node](edge-node/) | The Zone Controller edge node (`cmd/edge`) issuing a headless, confirmed-only emergency stop, with no hardware and no control plane. |
| [demo-ab-live](demo-ab-live/) | `run.sh` only, no README: the sample fleet with every robot driven by its own `sim_adapter.py` process over mTLS, and no status written by the script. |
| [event-bridge](event-bridge/) | Signed webhooks from outside systems become SemanticEvents, and TaskTriggers turn them into drafts a person clears; `run.sh` shows the refusals (unknown caller, bad signature, replay) and one event delivered twice making one draft. |
