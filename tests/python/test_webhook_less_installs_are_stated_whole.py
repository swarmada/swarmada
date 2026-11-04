# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The published text names every install that runs without admission webhooks.

Why this file exists. Four ways of running Swarmada install no admission webhooks:
`make deploy-minimal` (`config/default`), the quickstart overlays, the Helm chart with
`webhooks.enabled=false`, and `make run` (`ENABLE_WEBHOOKS=false`). On all four the estop
checks, the `cancel` and `append` checks, the `robots/status` guard and the delete refusal
do not run. The texts that listed webhook-less installs named the first two only, so a
reader of the chart README could switch every guard off "for a dev cluster" without being
told, and `docs/deploy-minikube.md` counted fourteen webhooks where the manifests declare
fifteen, leaving out the `robots/status` guard.

1. Every paragraph that lists two or more of those installs as webhook-less names all four.
2. deploy-minikube's webhook count and its per-kind list match `config/webhook/manifests.yaml`.
3. The chart README and the chart's values.yaml warn where they offer `webhooks.enabled=false`,
   and known-limitations states what the four installs leave open.
4. known-limitations says what `make run` prints: it lists what the install leaves open. The
   entry once said it printed only that no webhooks were served.

The samples README, `docs/adapters.md` and the chart's values.yaml are read too: the first is
where a user picks a posture, and the lists in all three had left installs out.
"""

from __future__ import annotations

import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
MANIFESTS = ROOT / "config" / "webhook" / "manifests.yaml"
MINIKUBE = ROOT / "docs" / "deploy-minikube.md"
CHART_README = ROOT / "deploy" / "swarmada" / "README.md"
LIMITATIONS = ROOT / "docs" / "known-limitations.md"
SAMPLES_README = ROOT / "config" / "samples" / "README.md"
VALUES = ROOT / "deploy" / "swarmada" / "values.yaml"
MAKEFILE = ROOT / "Makefile"
KL_ENTRY = "### An install without admission webhooks checks nothing at admission"

# The published texts: docs, the changelog, the chart README and the assembled RFC (the
# chapter sources are not promoted; the dist is).
TEXTS = (sorted((ROOT / "docs").rglob("*.md"))
         + [ROOT / "CHANGELOG.md", CHART_README, ROOT / "rfcs" / "dist" / "RFC-0001-core-spec.md",
            SAMPLES_README, VALUES])

# Each install, and the words that name it.
INSTALLS = {
    "make deploy-minimal": re.compile(r"deploy-minimal|config/default"),
    "the quickstart overlays": re.compile(r"quickstart overlay|quickstart-(?:mtls|projected|dev)"),
    "the chart's webhooks.enabled=false": re.compile(r"webhooks\.enabled=false"),
    "make run": re.compile(r"`make run`|\bmake run\b"),
}
NO_WEBHOOKS = re.compile(
    r"no admission webhook|without (?:the )?(?:admission )?webhooks|without them|install(?:s)? none"
    r"|webhooks (?:are )?\**(?:off|disabled)|ENABLE_WEBHOOKS=false|none of the webhook rows"
    r"|disables? them",
    re.I)

_NUMBER = {n: w for w, n in (("one", 1), ("two", 2), ("three", 3), ("four", 4), ("five", 5),
                             ("six", 6), ("seven", 7), ("eight", 8), ("nine", 9), ("ten", 10),
                             ("eleven", 11), ("twelve", 12), ("thirteen", 13), ("fourteen", 14),
                             ("fifteen", 15), ("sixteen", 16), ("seventeen", 17), ("eighteen", 18),
                             ("nineteen", 19), ("twenty", 20))}


def _paragraphs(path: Path) -> list[str]:
    # A paragraph, one top-level list item (a changelog section is one run of items) or one
    # table row. In YAML, one run of comment lines.
    text = path.read_text(encoding="utf-8")
    if path.suffix == ".yaml":
        text = "\n".join(ln.strip().lstrip("#") if ln.strip().startswith("#") else ""
                         for ln in text.splitlines())
    parts = re.split(r"\n\s*\n|\n(?=- )|\n(?=\|)", text)
    return [" ".join(p.split()) for p in parts]


def test_every_list_of_webhook_less_installs_names_all_four():
    short = []
    for path in TEXTS:
        for para in _paragraphs(path):
            if not NO_WEBHOOKS.search(para):
                continue
            named = [k for k, rx in INSTALLS.items() if rx.search(para)]
            if len(named) < 2:
                continue  # not a list of installs
            missing = [k for k in INSTALLS if k not in named]
            if missing:
                short.append(f"{path.relative_to(ROOT)}: names {named}, not {missing}: "
                             f"{para[:160]!r}")
    assert not short, "a list of webhook-less installs leaves some out:\n" + "\n".join(short)


def _webhooks() -> tuple[int, int]:
    mutating = validating = 0
    for doc in yaml.safe_load_all(MANIFESTS.read_text()):
        if not doc:
            continue
        n = len(doc.get("webhooks", []))
        if doc["kind"] == "MutatingWebhookConfiguration":
            mutating += n
        elif doc["kind"] == "ValidatingWebhookConfiguration":
            validating += n
    return mutating, validating


def test_deploy_minikube_counts_the_webhooks_the_manifests_declare():
    mutating, validating = _webhooks()
    assert validating >= 10 and mutating >= 5, f"read {mutating} mutating, {validating} validating"
    text = " ".join(MINIKUBE.read_text(encoding="utf-8").split())
    m = re.search(r"declares (\w+) admission webhooks — (\w+) mutating \(([^)]*)\) and (\w+) "
                  r"validating \(([^)]*)\)", text)
    assert m, "deploy-minikube.md no longer states the webhook count in the form this test reads"
    total, mut, mut_list, val, val_list = m.groups()
    assert (total, mut, val) == (_NUMBER[mutating + validating], _NUMBER[mutating],
                                 _NUMBER[validating]), (
        f"deploy-minikube.md says {total} ({mut} mutating, {val} validating); "
        f"{MANIFESTS.relative_to(ROOT)} declares {mutating + validating} "
        f"({mutating} mutating, {validating} validating)")
    assert len(mut_list.split(",")) == mutating, (
        f"mutating list {mut_list!r} is not {mutating} long")
    assert len(val_list.split(",")) == validating, (
        f"validating list {val_list!r} is not {validating} long")
    for guard in ("robots/status", "delete"):
        assert guard in text.split("admission webhooks are disabled in this overlay", 1)[-1], (
            f"deploy-minikube.md's list of what is lost does not name {guard!r}")


def test_the_chart_readme_warns_where_it_offers_webhooks_enabled_false():
    text = CHART_README.read_text(encoding="utf-8")
    at = [" ".join(p.split()) for p in re.split(r"\n(?=- )|\n\s*\n", text)
          if "webhooks.enabled=false" in p]
    assert at, "the chart README no longer mentions webhooks.enabled=false"
    for para in at:
        for claim in ("robots/status", "known-limitations.md"):
            assert claim in para, (
                f"the chart README offers webhooks.enabled=false without naming {claim!r}: "
                f"{para[:160]!r}")


def test_values_yaml_warns_at_webhooks_enabled():
    # The comment block directly above `webhooks:` is what a reader of the values file sees
    # when they switch it off.
    lines = VALUES.read_text(encoding="utf-8").splitlines()
    at = next(i for i, ln in enumerate(lines) if ln.startswith("webhooks:"))
    block = []
    for ln in reversed(lines[:at]):
        if not ln.startswith("#"):
            break
        block.append(ln.lstrip("#").strip())
    comment = " ".join(" ".join(reversed(block)).split())
    for claim in ("false", "robots/status", "can be deleted", "known-limitations.md"):
        assert claim in comment, (
            f"values.yaml's comment above `webhooks:` does not say {claim!r}: {comment!r}")


def test_known_limitations_states_what_the_four_installs_leave_open():
    text = LIMITATIONS.read_text(encoding="utf-8")
    assert KL_ENTRY in text, f"known-limitations.md has no entry {KL_ENTRY!r}"
    entry = " ".join(text.split(KL_ENTRY, 1)[1].split("\n### ", 1)[0].split())
    for name, rx in INSTALLS.items():
        assert rx.search(entry), f"the entry does not name {name}"
    for opened in ("estop-trigger", "estop-clear", "cancel", "append", "activate", "robots/status",
                   "can be deleted"):
        assert opened in entry, f"the entry does not say that {opened!r} goes unchecked"


def test_known_limitations_says_what_make_run_prints():
    run = MAKEFILE.read_text(encoding="utf-8").split("\nrun:", 1)[1].split("\n\n", 1)[0]
    assert "No admission check runs" in run and "robots/status guard" in run, (
        "make run no longer prints what an install without webhooks leaves open; "
        "re-read known-limitations' entry")
    text = LIMITATIONS.read_text(encoding="utf-8")
    entry = " ".join(text.split(KL_ENTRY, 1)[1].split("\n### ", 1)[0].split())
    seen = entry.split("**What you will see.**", 1)[1].split("**Instead.**", 1)[0]
    assert not re.search(r"`make run`[^.]*but not what that opens", seen), (
        f"known-limitations says `make run` does not print what it opens; it does: {seen!r}")
    assert re.search(r"`make run` prints [^.]*(?:what|each check)", seen), (
        f"known-limitations does not say that `make run` prints what is left open: {seen!r}")
