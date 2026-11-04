# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Published sentences about admission, outages and scope, each pinned to the code they describe.

The webhook facts are read from `config/webhook/manifests.yaml`, which `make manifests` generates
from the kubebuilder markers, so a webhook that changes its operations or its failurePolicy turns
the matching text test red. The RFC is read from the assembled dist, which is what ships.

1. The safety chapter names the held-robot delete refusal by the webhook that serves it, with
   that webhook's failurePolicy. It named the create/update webhook, with Fail.
2. No text says a manager outage never blocks a namespace deletion, including the code and
   chart comments that explain the Robot delete check. The operator texts name every kind whose
   delete check fails closed, and the operations runbook takes out exactly those checks.
3. The FleetZone sharedResources explain text says a resource name is resolved across the
   namespace and that same-name declarations are one resource, and no other FleetZone explain
   text or refusal says resources resolve along the zone tree.
4. The samples README builds swarmctl and puts it on PATH before it runs it.
5. The quickstart README says that only the default QUICKSTART_IMG works.
6. The floor-mapping guide states the keep-out placement rules the worker implements.
7. The FleetZone reservations explain text does not say an edit is refused while no webhook
   sees the status subresource.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
DIST = ROOT / "rfcs" / "dist" / "RFC-0001-core-spec.md"


def _read(rel: str) -> str:
    return (ROOT / rel).read_text(encoding="utf-8")


def _flat(text: str) -> str:
    return " ".join(text.split())


def _says(text: str, phrase: str) -> bool:
    # A plain bool, so a failing assertion does not have pytest diff a whole document.
    return phrase in text


def _dist_section(anchor: str) -> str:
    """The dist text under `anchor`'s heading, up to the next heading of the same or a higher
    level. A `#` line inside a fenced block (a YAML comment) is not a heading."""
    lines = DIST.read_text(encoding="utf-8").splitlines()
    at = lines.index(f'<a id="{anchor}"></a>')
    level = len(lines[at + 1]) - len(lines[at + 1].lstrip("#"))
    assert level, f"no heading after anchor {anchor}"
    out, fenced = [], False
    for line in lines[at + 2:]:
        if line.startswith("```"):
            fenced = not fenced
        elif not fenced and re.match(rf"#{{1,{level}}} ", line):
            break
        out.append(line)
    return "\n".join(out)


def _md_section(text: str, heading: str) -> str:
    """The text under a markdown heading line, up to the next heading of the same or a higher
    level."""
    level = len(heading) - len(heading.lstrip("#"))
    assert heading in text.splitlines(), f"no heading {heading!r}"
    body = text.split(heading + "\n", 1)[1]
    out, fenced = [], False
    for line in body.splitlines():
        if line.startswith("```"):
            fenced = not fenced
        elif not fenced and re.match(rf"#{{1,{level}}} ", line):
            break
        out.append(line)
    return "\n".join(out)


def _validating_webhooks() -> list[dict]:
    docs = [d for d in yaml.safe_load_all(_read("config/webhook/manifests.yaml")) if d]
    vwc = [d for d in docs if d.get("kind") == "ValidatingWebhookConfiguration"]
    assert len(vwc) == 1, f"found {len(vwc)} ValidatingWebhookConfigurations"
    return vwc[0]["webhooks"]


def _kinds_by_plural() -> dict[str, str]:
    out = {}
    for crd in (ROOT / "config" / "crd" / "bases").glob("swarmada.io_*.yaml"):
        names = yaml.safe_load(crd.read_text(encoding="utf-8"))["spec"]["names"]
        out[names["plural"]] = names["kind"]
    return out


def _delete_checks() -> list[tuple[str, str, list[str]]]:
    """(webhook name, failurePolicy, resources) for every validating webhook that sees DELETE."""
    out = []
    for w in _validating_webhooks():
        ops = {op for r in w["rules"] for op in r["operations"]}
        if "DELETE" in ops:
            out.append((w["name"], w["failurePolicy"],
                        sorted({res for r in w["rules"] for res in r["resources"]})))
    return out


def test_the_safety_chapter_names_the_robot_delete_webhook_and_its_policy():
    robot_delete = [(n, p) for n, p, res in _delete_checks() if res == ["robots"]]
    assert len(robot_delete) == 1, f"robot DELETE webhooks: {robot_delete}"
    name, policy = robot_delete[0]
    section = _flat(_dist_section("safety-estop-state-machine"))
    # A sentence ends at a period before a capital; a webhook name has periods inside it.
    sentence = re.search(r"The delete refusal is .*?\.(?= [A-Z]|$)", section)
    assert sentence, "the estop state machine no longer describes the delete refusal's webhook"
    assert _says(sentence.group(0), f"`{name}`"), (
        f"the safety chapter's delete-refusal sentence does not name {name}: "
        f"{sentence.group(0)[:160]!r}")
    assert _says(sentence.group(0), f"`failurePolicy: {policy}`"), (
        f"the safety chapter's delete-refusal sentence does not say failurePolicy {policy}")
    if policy == "Ignore":
        assert _says(sentence.group(0), "fails open"), (
            "the safety chapter does not say the delete refusal fails open")
        assert _says(sentence.group(0), "a robot that holds a stop is admitted"), (
            "the safety chapter does not say what an outage admits: the delete of a held robot")


def test_no_text_says_an_outage_never_blocks_a_namespace_deletion():
    checks = _delete_checks()
    plural_kind = _kinds_by_plural()
    closed = {n: [plural_kind[r] for r in res] for n, p, res in checks if p == "Fail"}
    assert closed, "no validating webhook checks DELETE with failurePolicy Fail"
    texts = {
        "known-limitations": _read("docs/known-limitations.md"),
        "operations.md": _read("docs/operations.md"),
        "CHANGELOG": _read("CHANGELOG.md"),
        "chart README": _read("deploy/swarmada/README.md"),
        "chart values.yaml": _read("deploy/swarmada/values.yaml"),
        "RFC dist": DIST.read_text(encoding="utf-8"),
    }
    comments = {
        rel: _read(rel)
        for rel in (
            "internal/webhook/robot_delete_guard.go",
            "internal/webhook/robot_delete_outage_envtest_test.go",
            "cmd/manager/main.go",
            "deploy/swarmada/templates/webhook.yaml",
            "hack/sync-helm-webhooks.py",
        )
    }
    for where, text in {**texts, **comments}.items():
        flat = _flat(re.sub(r"^\s*(?:#|//)\s?", "", text, flags=re.M))
        for stale in ("never blocks a namespace deletion", "never blocks a namespace delete",
                      "never block a namespace delete"):
            assert not _says(flat, stale), f"{where} says a manager outage {stale}"
    kinds = sorted({k for ks in closed.values() for k in ks})
    kl = _flat(_md_section(
        texts["known-limitations"],
        "### A namespace deleted during a manager outage stays Terminating"))
    runbook_md = _md_section(texts["operations.md"],
                             "## 4. Deleting a namespace during a manager outage")
    runbook = _flat(runbook_md)
    for kind in kinds:
        assert _says(kl, f"`{kind}`"), f"known-limitations' outage entry does not name {kind}"
        assert _says(runbook, f"`{kind}`"), f"the operations runbook does not name {kind}"
    row = next(ln for ln in texts["RFC dist"].splitlines() if ln.startswith("| `Robot` (delete) |"))
    for kind in kinds:
        assert _says(row, f"`{kind}`"), f"the RFC's Robot delete row does not name {kind}"
    taken_out = [set(re.findall(r'"([a-z]+\.swarmada\.io)"', m))
                 for m in re.findall(r"IN\(([^)]*)\)", runbook_md)]
    assert taken_out, "the operations runbook has no jq IN(...) list of webhooks"
    for names in taken_out:
        assert names == set(closed), (
            f"the operations runbook names {sorted(names)}; the fail-closed DELETE checks are "
            f"{sorted(closed)}")


def test_the_runbook_jq_filter_removes_only_delete_from_the_fail_closed_checks():
    """Run the runbook's own step-2 jq program, on the manifest, through jq."""
    import shutil
    import subprocess

    jq = shutil.which("jq")
    if jq is None:
        # The runbook needs jq; so does this check of it. A missing jq is a host gap, not a pass.
        raise AssertionError("jq is not on PATH; the operations runbook's step 2 needs it")
    runbook = _md_section(_read("docs/operations.md"),
                          "## 4. Deleting a namespace during a manager outage")
    step2 = re.search(r"# 2\..*?\njq '(.*?)' \\\n", runbook, re.S)
    assert step2, "the operations runbook has no step 2 jq program"
    vwc = {"webhooks": _validating_webhooks()}
    out = subprocess.run([jq, step2.group(1)], input=json.dumps(vwc), capture_output=True,
                         text=True, check=True).stdout
    after = {w["name"]: {op for r in w["rules"] for op in r["operations"]}
             for w in json.loads(out)["webhooks"]}
    before = {w["name"]: {op for r in w["rules"] for op in r["operations"]}
              for w in _validating_webhooks()}
    closed = {n for n, p, _ in _delete_checks() if p == "Fail"}
    for name, ops in before.items():
        want = ops - {"DELETE"} if name in closed else ops
        assert after[name] == want, (
            f"{name}: runbook leaves {sorted(after[name])}, want {sorted(want)}")


def test_the_runbook_reads_the_four_checks_back_at_fail():
    """Step 5's own jq, run on the shipped manifest, prints exactly the lines the runbook tells
    the operator to expect, and each says failurePolicy=Fail with DELETE back."""
    import shutil
    import subprocess

    jq = shutil.which("jq")
    if jq is None:
        raise AssertionError("jq is not on PATH; the operations runbook's step 5 needs it")
    runbook = _md_section(_read("docs/operations.md"),
                          "## 4. Deleting a namespace during a manager outage")
    assert re.search(r"^# 4\. Put the checks back\.", runbook, re.M), (
        "the runbook has no restore step")
    assert re.search(r'^kubectl replace -f "\$VWC\.backup\.json"$', runbook, re.M), (
        "the runbook's restore step does not re-apply the saved configuration")
    step5 = re.search(r"# 5\..*?\n  \| jq -r '(.*?)'\n```", runbook, re.S)
    assert step5, "the runbook has no step 5 read-back"
    expected = re.search(r"Step 5 must print these four lines[^`]*```\n(.*?)```", runbook, re.S)
    assert expected, "the runbook does not show the read-back it expects"
    want = sorted(expected.group(1).split("\n")[:-1])
    out = subprocess.run([jq, "-r", step5.group(1)],
                         input=json.dumps({"webhooks": _validating_webhooks()}),
                         capture_output=True, text=True, check=True).stdout
    assert sorted(out.splitlines()) == want, (
        f"step 5 prints {out!r}; the runbook expects {want}")
    closed = sorted(n for n, p, _ in _delete_checks() if p == "Fail")
    assert [ln.split()[0] for ln in want] == closed, (
        f"read-back names {want}; Fail DELETE checks {closed}")
    for ln in want:
        assert ln.endswith(" failurePolicy=Fail CREATE,DELETE,UPDATE"), f"read-back line {ln!r}"


def test_the_sharedresources_explain_text_says_names_are_namespace_wide():
    crd = yaml.safe_load(_read("config/crd/bases/swarmada.io_fleetzones.yaml"))
    spec = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]
    desc = _flat(spec["properties"]["sharedResources"]["description"])
    assert not _says(desc, "descendant zones may reserve"), (
        "the sharedResources explain text scopes a resource to a zone's descendants")
    assert _says(desc, "whole namespace"), "the explain text does not say namespace-wide"
    assert _says(desc, "same name"), "the explain text does not say same-name declarations merge"
    others = {
        "spec": _flat(spec["description"]),
        "spec.memberZones": _flat(spec["properties"]["memberZones"]["description"]),
        **{f"spec CEL rule {i}": _flat(v["message"])
           for i, v in enumerate(spec.get("x-kubernetes-validations", []))},
    }
    for where, text in others.items():
        for stale in ("walks the containment tree", "walks containment edges",
                      "sharedResources inheritance"):
            assert not _says(text, stale), f"the FleetZone {where} text says resolution {stale}"
    resolver = _read("internal/tde/resource.go")
    assert _says(resolver, "client.InNamespace(namespace)") and _says(resolver, "z.Name < owner"), (
        "the TDE no longer resolves a resource name across the namespace by lowest zone name; "
        "re-read the sharedResources texts")
    kl = _read("docs/known-limitations.md")
    assert _says(kl, "### A shared-resource name is resolved across the namespace"), (
        "known-limitations has no entry on namespace-wide resource names")


def test_the_reservations_explain_text_says_what_a_status_edit_meets():
    status_hooks = [w["name"] for w in _validating_webhooks()
                    if any("fleetzones/status" in r["resources"] for r in w["rules"])]
    crd = yaml.safe_load(_read("config/crd/bases/swarmada.io_fleetzones.yaml"))
    status = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["status"]
    desc = _flat(status["properties"]["reservations"]["description"])
    if not status_hooks:
        assert not _says(desc, "rejected by the FleetZone admission webhook"), (
            "the reservations explain text says edits are rejected; "
            "no webhook sees fleetzones/status")
        assert _says(desc, "system:masters"), (
            "the reservations explain text does not say who is not refused")


def test_the_samples_readme_builds_the_swarmctl_it_runs():
    """A fenced `swarmctl` line runs only after a fenced block has built bin/swarmctl and put
    bin/ on PATH. (cmd/swarmctl's admit test reads the fenced `swarmctl admit robot` lines.)"""
    text = _read("config/samples/README.md")
    lines = [ln.strip() for b in re.findall(r"```[^\n]*\n(.*?)```", text, re.S)
             for ln in b.splitlines()]
    runs = [i for i, ln in enumerate(lines) if re.match(r"swarmctl\s", ln)]
    assert runs, "the samples README runs no swarmctl command"
    built = lines.index("make swarmctl") if "make swarmctl" in lines else None
    on_path = lines.index('export PATH="$PWD/bin:$PATH"') \
        if 'export PATH="$PWD/bin:$PATH"' in lines else None
    assert built is not None and on_path is not None, (
        "the samples README runs swarmctl without building it and putting bin/ on PATH")
    assert built < on_path < runs[0], (
        "the samples README runs swarmctl before it builds it and puts bin/ on PATH")
    assert re.search(r"^swarmctl:.*\n\t.*-o bin/swarmctl ", _read("Makefile"), re.M), (
        "make swarmctl no longer builds bin/swarmctl")


def test_the_quickstart_img_knob_says_only_the_default_works():
    readme = _read("examples/warehouse-quickstart/README.md")
    row = next(ln for ln in readme.splitlines() if ln.startswith("| `QUICKSTART_IMG`"))
    run = _read("examples/warehouse-quickstart/run.sh")
    if re.search(r"set image|edit set image|image\.tag=", run):
        raise AssertionError("run.sh now overrides the deployed image; the QUICKSTART_IMG "
                             "caveat in the README may be stale")
    assert _says(row, "Only the default is supported"), (
        "the QUICKSTART_IMG knob row does not say that only the default works")
    assert _says(_read("config/default/kustomization.yaml"), "newTag: dev"), (
        "config/default no longer pins the :dev tag; re-read the QUICKSTART_IMG caveat")


def test_the_floor_mapping_guide_states_the_keepout_placement_rules():
    worker = _read("examples/floor-mapping/worker/zone_proposal_worker.py")
    for rule in ("nearest room", "lies across more than one room"):
        assert _says(worker, rule), f"the zone-proposal worker no longer says {rule!r}"
    guide = _flat(_read("docs/floor-mapping.md"))
    assert not _says(guide, "isn't inside exactly one room refuses the whole proposal"), (
        "floor-mapping.md says a keep-out outside one room refuses the proposal")
    for rule in ("**Inside one room:**", "**Over the edge of one room**",
                 "**In no room of the map:** the nearest room",
                 "**Across more than one room:** the whole proposal is refused"):
        assert _says(guide, rule), f"floor-mapping.md does not state {rule!r}"


def test_every_stays_held_text_says_an_error_only_hold_ends_offline():
    """A hold by phase Error alone ends when a silent robot is marked Offline (HeldBy counts
    phase Error, and the Robot controller moves a silent robot to Offline). Each text that says
    a robot that never reports again stays held must also say that."""
    held = _flat(_read("internal/safety/held.go"))
    assert _says(held, "robot.Status.Phase == fleetv1.RobotPhaseError"), (
        "HeldBy no longer reads phase Error; re-read the stays-held texts")
    assert _says(held, "marks it Offline"), "held.go no longer says Offline ends an Error hold"
    kl = _read("docs/known-limitations.md")
    texts = {
        "known-limitations, stop kept on the Robot": _md_section(
            kl, "### A stop is kept on the Robot object only"),
        "known-limitations, shared-resource hold": _md_section(
            kl, "### A shared-resource hold outlives a robot that never returns"),
        "operations.md": _read("docs/operations.md").split(
            "**A robot that holds a stop cannot be deleted.**", 1)[1].split("\n\n", 1)[0],
        "the RFC's Robot delete row": next(
            ln for ln in DIST.read_text(encoding="utf-8").splitlines()
            if ln.startswith("| `Robot` (delete) |")),
        "the RFC's TDE-7": _dist_section("tde-normative-invariants"),
    }
    for where, text in texts.items():
        flat = _flat(text)
        assert _says(flat, "reports again"), f"{where} no longer says a held robot stays held"
        assert re.search(r"`Error`[^.]*(?:alone|only)[^.]*`Offline`|"
                         r"(?:alone|only)[^.]*`Error`[^.]*`Offline`", flat), (
            f"{where} does not say a hold by Error alone ends when the robot is marked Offline")


def test_the_texts_say_which_namespaces_never_finish_deleting():
    """While the FleetTask and rollout delete checks do not exempt namespace teardown, and the
    ZoneMaintenance check re-validates a finalizer removal, known-limitations says those objects
    hold a namespace with the manager serving, and the runbook says so where it restores it."""
    fleettask = _read("internal/webhook/fleettask_webhook.go")
    rollout = _read("internal/webhook/rollout_webhook.go")
    zm = _read("internal/webhook/zonemaintenance_webhook.go")
    if "namespaceTerminating" in fleettask + rollout or "DeletionTimestamp" in zm:
        raise AssertionError("a teardown exemption was added; re-read known-limitations' entry on "
                             "namespaces that are never deleted and operations.md section 4")
    kl = _flat(_md_section(
        _read("docs/known-limitations.md"),
        "### A namespace holding an active maintenance window, a running task or an unfinished "
        "model rollout can stay Terminating"))
    for kind in ("ZoneMaintenance", "FleetTask", "FirmwareRollout", "ModelRollout"):
        assert _says(kl, f"`{kind}`"), (
            f"known-limitations' never-deleted entry does not name {kind}")
    runbook = _flat(_md_section(_read("docs/operations.md"),
                                "## 4. Deleting a namespace during a manager outage"))
    assert not _says(runbook, "Restore the manager for such a namespace"), (
        "the runbook says restoring the manager releases a ZoneMaintenance namespace")
    assert _says(runbook, "hold it with the manager serving too"), (
        "the runbook's restore section does not say which namespaces it does not release")


def test_the_runbook_reads_the_zonemaintenance_check_back_at_fail():
    """The ZoneMaintenance escape's step 2 removes only UPDATE from that check, and its step 5,
    run on the shipped manifest, prints the line the runbook tells the operator to expect."""
    import shutil
    import subprocess

    jq = shutil.which("jq")
    if jq is None:
        raise AssertionError("jq is not on PATH; the operations runbook needs it")
    sec = _md_section(_read("docs/operations.md"), "### Remove a ZoneMaintenance finalizer")
    assert re.search(r'^kubectl replace -f "\$VWC\.backup\.json"$', sec, re.M), (
        "the ZoneMaintenance escape does not put the check back")
    step2 = re.search(r"# 2\..*?\njq '(.*?)' \\\n", sec, re.S)
    step5 = re.search(r"# 5\..*?\n  \| jq -r '(.*?)'\n```", sec, re.S)
    assert step2 and step5, "the ZoneMaintenance escape lacks step 2 or step 5"
    hooks = {"webhooks": _validating_webhooks()}
    after = json.loads(subprocess.run([jq, step2.group(1)], input=json.dumps(hooks),
                                      capture_output=True, text=True, check=True).stdout)
    for w in after["webhooks"]:
        ops = {op for r in w["rules"] for op in r["operations"]}
        before = next({op for r in b["rules"] for op in r["operations"]}
                      for b in hooks["webhooks"] if b["name"] == w["name"])
        want = before - {"UPDATE"} if w["name"] == "vzonemaintenance.swarmada.io" else before
        assert ops == want, f"{w['name']}: step 2 leaves {sorted(ops)}, want {sorted(want)}"
    printed = subprocess.run([jq, "-r", step5.group(1)], input=json.dumps(hooks),
                             capture_output=True, text=True, check=True).stdout.strip()
    expected = re.search(r"Step 5 must print `([^`]+)`", sec)
    assert expected and printed == expected.group(1), (
        f"step 5 prints {printed!r}; the runbook expects {expected and expected.group(1)!r}")
    assert printed.endswith("failurePolicy=Fail CREATE,UPDATE"), printed


def test_the_samples_readme_restarts_the_adapter_after_deleting_its_namespace():
    """A connected adapter does not announce a robot again once its Robot is gone (the control
    plane answers only a Register with NOT_ADMITTED), so the README restarts it."""
    server = _read("internal/controlstream/server.go")
    refuse = server.split("func (s *Server) refuse(", 1)[1]
    if not refuse.lstrip().split("\n", 2)[1].strip().startswith("if msg.GetRegister() == nil"):
        raise AssertionError("refuse() no longer answers only a Register; re-read the samples "
                             "README's adapter restart")
    text = _read("config/samples/README.md")
    assert re.search(r"kubectl delete namespace warehouse-a --wait\n"
                     r"kubectl -n swarmada-system rollout restart deployment/sim-fleet-adapter\n",
                     text), "the samples README does not restart the adapter after the delete"
