#!/usr/bin/env python3
# Copyright 2026 The Swarmada Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# ─────────────────────────────────────────────────────────────────────────────
# Renders deploy/swarmada and fails unless its authorization surface EQUALS the one in
# config/, rule for rule. Wired into `make helm-verify-sync` beside the CRD and webhook
# sync checks.
#
# WHY THIS EXISTS. The chart's roles were hand-mirrored from config/rbac with a comment
# as the only thing holding the two together, and they drifted in the direction that
# fails silently: a chart that grants LESS than config/ is a controller that gets 403s
# at runtime on a Helm install and on no other. A chart that grants MORE is
# over-privileged on every install. So this is an equality check, not a superset check.
#
# WHAT IS COMPARED, with default values:
#   - the manager ClusterRole       vs config/rbac/role.yaml
#   - the manager ClusterRoleBinding vs config/rbac/manager_role_binding.yaml
#   - every hand-authored built-in ClusterRole in config/rbac/kustomization.yaml, by name
#   - every webhook in config/webhook/manifests.yaml, by name: failurePolicy, rules,
#     sideEffects, service path, admissionReviewVersions, matchPolicy, timeoutSeconds,
#     namespace/object selectors
#   - the controller-manager Deployment's update strategy: Recreate in both, so an upgrade
#     never runs a second manager process beside the leader (a single replica is the only
#     supported safety configuration in RFC-0001 #safety-robot-stop-table)
#
# WHAT IS ALLOWED TO DIFFER is only what the chart templates by construction: the
# release-derived names (manager role, binding, ServiceAccount, webhook Service and
# configuration names) and the namespace. The chart is rendered under a release name and
# namespace that differ from config/'s, so a substitution the check did not anticipate
# shows up as a difference instead of hiding behind a coincidental match.
#
# It needs the helm binary and renders the chart; it never compares templates as text.
# Without helm it FAILS rather than skipping: a gate that exits 0 without running is
# indistinguishable from one that passed.
# ─────────────────────────────────────────────────────────────────────────────
import os
import shutil
import subprocess
import sys

import yaml

CHART = os.environ.get("HELM_CHART", "deploy/swarmada")
RBAC_DIR = "config/rbac"
WEBHOOKS = "config/webhook/manifests.yaml"

# Deliberately not "swarmada" / "system": see WHAT IS ALLOWED TO DIFFER above.
RELEASE = "verify-rel"
NAMESPACE = "verify-ns"

# The kustomize defaults config/ is written against (config/default/kustomization.yaml
# prefixes names with swarmada- and places the control plane in its namespace; role.yaml
# and the binding carry the prefixed names already).
CONFIG_MANAGER_ROLE = "swarmada-manager-role"


def load_all(path):
    with open(path) as f:
        return [d for d in yaml.safe_load_all(f) if d]


def render(extra_args=()):
    if shutil.which("helm") is None:
        sys.exit(
            "ERROR: helm not found; this check renders the chart and cannot run without it "
            "(brew install helm)."
        )
    out = subprocess.run(
        ["helm", "template", RELEASE, CHART, "--namespace", NAMESPACE, *extra_args],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    return [d for d in yaml.safe_load_all(out) if d]


def rule_tuples(rules):
    """Expand RBAC rules to (group, resource, resourceName, verb) — order and grouping free."""
    out = set()
    for r in rules or []:
        names = r.get("resourceNames") or ["*"]
        for v in r.get("verbs", []):
            for g in r.get("apiGroups", []):
                for res in r.get("resources", []):
                    for n in names:
                        out.add((g, res, n, v))
            for u in r.get("nonResourceURLs", []):
                out.add(("<url>", u, "*", v))
    return out


def fmt(t):
    g, res, n, v = t
    name = "" if n == "*" else f" [{n}]"
    return f"{g or 'core'}/{res}{name}: {v}"


def compare_rules(label, cfg_rules, chart_rules, problems):
    a, b = rule_tuples(cfg_rules), rule_tuples(chart_rules)
    for t in sorted(a - b):
        problems.append(
            f"{label}: config/ grants, chart does NOT (403 on a Helm install): {fmt(t)}"
        )
    for t in sorted(b - a):
        problems.append(
            f"{label}: chart grants, config/ does NOT (over-privileged on a Helm install): {fmt(t)}"
        )


def kustomized_rbac_files():
    k = yaml.safe_load(open(os.path.join(RBAC_DIR, "kustomization.yaml")))
    return [os.path.join(RBAC_DIR, f) for f in k.get("resources", [])]


def check_rbac(chart_docs, problems):
    chart_roles = {d["metadata"]["name"]: d for d in chart_docs if d["kind"] == "ClusterRole"}
    chart_bindings = [d for d in chart_docs if d["kind"] == "ClusterRoleBinding"]
    sa = [d for d in chart_docs if d["kind"] == "ServiceAccount"]
    fullname_manager_role = f"{RELEASE}-swarmada-manager-role"

    cfg_docs = [d for f in kustomized_rbac_files() for d in load_all(f)]
    seen_chart = set()
    for d in cfg_docs:
        name = d["metadata"]["name"]
        if d["kind"] == "ClusterRole":
            if name == CONFIG_MANAGER_ROLE:
                chart_name = fullname_manager_role
            else:
                chart_name = name  # built-in roles are fixed-name cluster singletons
            c = chart_roles.get(chart_name)
            if c is None:
                problems.append(
                    f"ClusterRole {name}: declared in config/rbac, not rendered by the chart"
                )
                continue
            seen_chart.add(chart_name)
            compare_rules(f"ClusterRole {name}", d.get("rules"), c.get("rules"), problems)
            if d.get("aggregationRule") != c.get("aggregationRule"):
                problems.append(f"ClusterRole {name}: aggregationRule differs")
            for k, v in (d["metadata"].get("labels") or {}).items():
                got = (c["metadata"].get("labels") or {}).get(k)
                if got != v:
                    problems.append(
                        f"ClusterRole {name}: label {k}={v!r} in config/, {got!r} in chart"
                    )
        elif d["kind"] == "ClusterRoleBinding":
            if d["roleRef"]["name"] != CONFIG_MANAGER_ROLE:
                problems.append(
                    f"ClusterRoleBinding {name}: unexpected roleRef {d['roleRef']['name']}"
                )
                continue
            hits = [b for b in chart_bindings if b["roleRef"]["name"] == fullname_manager_role]
            if len(hits) != 1:
                problems.append(
                    f"ClusterRoleBinding {name}: chart renders {len(hits)} manager bindings, want 1"
                )
                continue
            want_sa = sa[0]["metadata"]["name"] if len(sa) == 1 else None
            subj = hits[0].get("subjects") or []
            want_n = len(d.get("subjects") or [])
            if len(subj) != want_n:
                problems.append(f"CRB {name}: {want_n} subjects in config/, {len(subj)} in chart")
            for s in subj:
                if (
                    s.get("kind") != "ServiceAccount"
                    or s.get("name") != want_sa
                    or s.get("namespace") != NAMESPACE
                ):
                    problems.append(
                        f"ClusterRoleBinding {name}: chart subject {s} is not the rendered manager "
                        f"ServiceAccount {NAMESPACE}/{want_sa}"
                    )
        else:
            problems.append(
                f"{d['kind']} {name} in config/rbac is not compared by this check; extend it"
            )

    for name in sorted(set(chart_roles) - seen_chart):
        problems.append(
            f"ClusterRole {name}: rendered by the chart, declared nowhere in config/rbac"
        )


WEBHOOK_FIELDS = (
    "failurePolicy",
    "sideEffects",
    "admissionReviewVersions",
    "matchPolicy",
    "timeoutSeconds",
    "namespaceSelector",
    "objectSelector",
    "reinvocationPolicy",
)


def norm_rules(rules):
    return sorted(
        (
            tuple(sorted(r.get("apiGroups", []))),
            tuple(sorted(r.get("apiVersions", []))),
            tuple(sorted(r.get("operations", []))),
            tuple(sorted(r.get("resources", []))),
            r.get("scope", "*"),
        )
        for r in rules or []
    )


def check_webhooks(chart_docs, problems):
    kinds = ("MutatingWebhookConfiguration", "ValidatingWebhookConfiguration")
    cfg = {
        (d["kind"], w["name"]): w
        for d in load_all(WEBHOOKS)
        if d["kind"] in kinds
        for w in d["webhooks"]
    }
    chart = {
        (d["kind"], w["name"]): w
        for d in chart_docs
        if d["kind"] in kinds
        for w in d.get("webhooks") or []
    }
    for key in sorted(set(cfg) | set(chart)):
        kind, name = key
        a, b = cfg.get(key), chart.get(key)
        if a is None:
            problems.append(f"{kind} {name}: rendered by the chart, absent from {WEBHOOKS}")
            continue
        if b is None:
            problems.append(
                f"{kind} {name}: in {WEBHOOKS}, NOT rendered by the chart (unenforced on Helm)"
            )
            continue
        for f in WEBHOOK_FIELDS:
            # Kubernetes defaults these server-side; compare what each side would store.
            default = {
                "matchPolicy": "Equivalent",
                "timeoutSeconds": 10,
                "namespaceSelector": {},
                "objectSelector": {},
                "reinvocationPolicy": "Never" if kind.startswith("Mutating") else None,
                "failurePolicy": "Fail",
            }.get(f)
            va, vb = a.get(f, default), b.get(f, default)
            if va != vb:
                problems.append(
                    f"{kind} {name}: {f} is {va!r} in config/, {vb!r} in the rendered chart"
                )
        if norm_rules(a.get("rules")) != norm_rules(b.get("rules")):
            problems.append(f"{kind} {name}: rules differ")
        pa = a["clientConfig"]["service"].get("path")
        pb = (b["clientConfig"].get("service") or {}).get("path")
        if pa != pb:
            problems.append(f"{kind} {name}: service path {pa!r} in config/, {pb!r} in chart")
        svc = b["clientConfig"].get("service") or {}
        if svc.get("namespace") != NAMESPACE:
            problems.append(
                f"{kind} {name}: chart points at a Service outside the release namespace: {svc}"
            )


MANAGER_DEPLOYMENT = "config/manager/manager.yaml"


def check_manager_strategy(chart_docs, problems):
    """The manager Deployment is replaced, never rolled: Recreate stops the old process before
    the new one starts. The Kubernetes default, RollingUpdate with maxSurge 25%, rounds up to a
    second manager next to the leader on every upgrade."""
    cfg = [d for d in load_all(MANAGER_DEPLOYMENT) if d.get("kind") == "Deployment"]
    chart = [
        d
        for d in chart_docs
        if d.get("kind") == "Deployment" and d["metadata"]["name"].endswith("-controller-manager")
    ]
    for where, docs in ((MANAGER_DEPLOYMENT, cfg), ("the rendered chart", chart)):
        if len(docs) != 1:
            problems.append(f"{where}: {len(docs)} controller-manager Deployments, expected 1")
            continue
        strategy = (docs[0].get("spec") or {}).get("strategy") or {}
        if strategy.get("type") != "Recreate":
            problems.append(
                f"{where}: the controller-manager Deployment's strategy is {strategy!r}, not "
                "{'type': 'Recreate'}: an upgrade would run a second manager beside the leader"
            )


def main():
    chart_docs = render()
    problems = []
    check_rbac(chart_docs, problems)
    check_webhooks(chart_docs, problems)
    check_manager_strategy(chart_docs, problems)
    if problems:
        print(f"ERROR: {CHART} differs from config/ ({len(problems)} difference(s)):")
        for p in problems:
            print(f"  - {p}")
        print("Establish which copy is right from the code that needs it, then fix the wrong copy.")
        return 1
    print(
        f"chart RBAC and webhook settings equal config/, and both replace the manager with "
        f"Recreate (rendered as {RELEASE} in {NAMESPACE})"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
