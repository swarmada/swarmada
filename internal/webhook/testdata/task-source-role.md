# The task-source Role, as RFC-0004 prints it

This file pins the `Role` and `RoleBinding` that RFC-0004 prints under *Identity and
Authorization*, verbatim. The envtest in `fleetaction_child_spec_envtest_test.go` binds a
ServiceAccount to this Role, so it needs no chapter source.
`TestTaskSourceRole_PinnedCopyIsTheRoleAsPrinted` holds this copy to the assembled
specification, `rfcs/dist/RFC-0004-task-source-surface.md`. When the specification changes
the block, copy the new block here.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: warehouse-a
  name: swarmada:task-source
rules:
  - apiGroups: ["swarmada.io"]
    resources: ["fleetactions", "fleettasks"]
    verbs: ["create", "get", "list", "watch", "patch"]
  # patch is required by the server-side apply upsert path (N2.4); without it a source
  # that follows this Role receives 403 on the path this chapter prescribes.
  # No rule grants fleetactions/status or fleettasks/status (RA-1) and none grants estop-clear.
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: warehouse-a
  name: wms-east-to-task-source
subjects:
  - kind: ServiceAccount
    name: wms-east
    namespace: warehouse-a
roleRef:
  kind: Role
  name: swarmada:task-source
  apiGroup: rbac.authorization.k8s.io
```
