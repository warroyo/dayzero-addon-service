# bootstrap-addon-service

A vSphere Supervisor Service that installs a single VKS addon whose only job is to
apply operator-supplied Kubernetes YAML into a workload cluster at provisioning time.

No Carvel package. No Helm chart. No container images. No package repository.

> **Status: design complete, nothing built yet.** Start with [`docs/plan.md`](docs/plan.md).
> Implementation is gated on Step 0 in [`docs/verify.md`](docs/verify.md).

## What problem this solves

Only platform admins can create `AddonConfigDefinition` resources in
`vmware-system-vks-public`. Regular users can create `AddonInstall` and `AddonConfig`
in their own vSphere namespace.

So a **one-time admin install** of this service permanently delegates "seed my
workload cluster with arbitrary YAML" to tenants — with no workload-cluster
kubeconfig, no registry, no chart, and no repo.

Falling out of that: it kills the GitOps chicken-and-egg. The addon runs from the
Supervisor *before* the guest cluster is reachable by anything external, so it can
plant the Argo CD root `Application` and repo credentials that let Argo take over
from there.

Addons are built for this job, but essentially every shipped addon drags in images,
charts, or repositories. This one is pure data — which is legal because
`AddonRelease.spec.package` is optional, and an `AddonRelease` carrying only an
`addonConfigDefinitionRef` is a first-class package-free addon.

## How it works

Because `AddonConfigDefinition` output resources have a structurally static GVK, the
addon can't emit arbitrary kinds directly. Instead it emits four fixed-GVK resources
into the guest cluster, the last being a kapp-controller `App` carrying the user's
YAML inline. kapp-controller — already present in every VKS cluster — applies
whatever is inside, with pruning, GC and status for free.

```
Supervisor                          Workload cluster
──────────                          ────────────────
Addon                    ┌────────► Namespace
AddonRelease (no pkg)    │          ServiceAccount
AddonConfigDefinition ───┤          ClusterRoleBinding
                         └────────► App (kapp-controller)
AddonInstall  ┐                       └── your YAML, applied
AddonConfig   ┘ per tenant
```

Payload comes from either inline `AddonConfig.spec.values.resources` or a ConfigMap in
the cluster's Supervisor namespace. Changing it is a data edit — no rebuild, no
version bump, no re-upload.

## Honest dependency list

The pitch is *provisioning-time seeding with no per-payload artifacts*, not "zero
dependencies." What remains:

1. **One imgpkg bundle** — for the Supervisor Service itself, which vCenter requires.
   The OCI dependency moves from per-payload to once, ever.
2. **kapp-controller in the guest** — pre-existing and VMware-managed, but an
   implementation detail rather than a contract.
3. **An undocumented ACD template dialect.**

## Scope boundary

This is a **seeding** mechanism, not an installer. The "no images" property holds
only while payloads are pure config — `Application`s, `Secret`s, `Namespace`s, CRs.
Real workloads mean images, registries and air-gap concerns, and belong in a Carvel
package instead.

## Docs

| | |
|---|---|
| [`docs/design.md`](docs/design.md) | API findings, why the design is shaped this way, alternatives rejected |
| [`docs/plan.md`](docs/plan.md)     | Repo layout, files to author, build/release, draft YAML |
| [`docs/verify.md`](docs/verify.md) | Step 0 dialect discovery + end-to-end verification runbook |
