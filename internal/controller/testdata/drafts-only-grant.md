# The drafts-only grant, as RFC-0001 prints it

This file pins the two YAML blocks of the drafts-only grant from RFC-0001
`#security-authorization-swarmada-rbac-roles` ("RBAC cannot hold an automation client to a
narrower grant than its verb"), verbatim, placeholders included. The envtest in
`fleettask_drafts_only_grant_envtest_test.go` applies them from here, so it needs no chapter
source. `TestDraftsOnlyGrant_PinnedCopyIsTheGrantAsPrinted` holds this copy to the assembled
specification, `rfcs/dist/RFC-0001-core-spec.md`. When the specification changes either
block, copy the new block here.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: <namespace>
  name: swarmada:task-source-drafts-only
rules:
  - apiGroups: ["swarmada.io"]
    resources: ["fleettasks"]
    verbs: ["create", "get", "list", "watch", "patch"]
  # Read only: a FleetAction has no draft state, so any write to one dispatches.
  - apiGroups: ["swarmada.io"]
    resources: ["fleetactions"]
    verbs: ["get", "list", "watch"]
```

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: task-source-creates-drafts-only
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["swarmada.io"]
        apiVersions: ["*"]
        operations: ["CREATE", "UPDATE"]
        resources: ["fleettasks"]
  matchConditions:
    - name: requested-by-the-task-source
      expression: request.userInfo.username == 'system:serviceaccount:<namespace>:<task-source>'
  validations:
    - expression: has(object.spec.draft) && object.spec.draft == true
      message: this task source may only create FleetTasks with spec.draft true
      reason: Forbidden
```
