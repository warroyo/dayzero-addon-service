# dayzero-addon-service

A VKS addon whose only job is to apply operator-supplied Kubernetes YAML into a workload
cluster at provisioning time: namespaces, service accounts, RBAC, resource quotas and
other day-zero configuration. Tenants attach it per cluster and supply the payload as
data, with no workload-cluster kubeconfig and no artifact authored per payload.

> Status: built, not yet verified against a live Supervisor end to end. The render logic
> is covered by tests (`make test`), and the guest-side mechanism is validated directly
> in a cluster (see [`docs/verify.md`](docs/verify.md)). The one unproven step is the
> `AddonRepository` round trip, which needs a published bundle.

## How it works

The addon ships as a Carvel package repository. An administrator points an
`AddonRepository` at it, and the VKS addon manager materialises the `Addon`,
`AddonRelease` and `AddonConfigDefinition` in `vmware-system-vks-public`. Tenants then
attach the addon with an `AddonInstall` and supply YAML through an `AddonConfig` per
cluster.

```
Registry                         Supervisor                      Workload cluster
────────                         ──────────                      ────────────────
addon-repo bundle  ──fetch──►    AddonRepository
  Package                        AddonRepositoryInstall
  + ACD (annotation)                    │ manager
                                        ▼
                                 Addon, AddonRelease, ACD        (in vks-public)
                                        │
  tenant: AddonInstall ─────────────────┤
          AddonConfig ─── values ──────►│ addon controller
                                        └──► values Secret ──►   PackageInstall
                                                                   └─ ytt renders your
                                                                      YAML, kapp applies
```

The payload flows as data: what a tenant writes in `AddonConfig.spec.values` is rendered
into a values Secret, which feeds the guest package, whose ytt turns it back into the
resources and applies them. Changing the payload is an edit to the `AddonConfig`, with no
rebuild, version bump or re-upload.

### Why an AddonRepository and not a direct install

`Addon` and `AddonRelease` are owned by the VKS addon manager: validating webhooks reject
creation from any client that is not the manager's own service account, and RBAC does not
change that. The only supported way to get them created is to hand the manager an
`AddonRepository`. A fuller account, and the design this project originally aimed for, is
in [`docs/future-direct-creation.md`](docs/future-direct-creation.md).

One consequence: the guest pulls the addon-repository bundle. That fetch is unavoidable
through the addon system, so this addon does not try to avoid it. The bundle is
config-only (empty ImagesLock), so relocation for air-gap is just the bundle itself.

## Install

Two methods. Both point at the same published bundle; pick the one that fits how the
Supervisor is administered.

### Method 1: apply the AddonRepository directly

For admins with Supervisor `kubectl` access. Edit
[`install/addonrepository.yml`](install/addonrepository.yml) so `imageURL` points at your
published bundle, then:

```sh
kubectl apply -f install/addonrepository.yml
```

### Method 2: the Supervisor Service

For environments where services are installed through the vCenter catalog. Build the
service package in [`supervisor-service/`](supervisor-service/) and upload it through
**Workload Management → Services → Add Service**. It carries the same two CRs; the deploy
places them in its own namespace, where the manager reconciles them.

### Confirm it registered

```sh
kubectl -n vmware-system-vks-public get addon,addonrelease,acd | grep dayzero
```

## Usage

Two objects per cluster, both in the cluster's own Supervisor namespace.

First, attach the addon, once per namespace. See
[`examples/addoninstall.yml`](examples/addoninstall.yml). Then label the clusters to seed:

```sh
kubectl label cluster my-cluster addons.kubernetes.vmware.com/dayzero=enabled
```

Second, supply the payload: an `AddonConfig` named `<cluster-name>-dayzero`, carrying
the annotation `clusteraddon.addons.kubernetes.vmware.com/owned-for-deletion: "true"`.
Both the name and the annotation are load-bearing. The name is how the addon system pairs
config to cluster, and without the annotation the `AddonConfig` outlives the
`ClusterAddon`.

Two payload sources, which compose:

| Source | Field | Use when |
|---|---|---|
| Structured | `values.resources` | Payload is authored with the AddonConfig. Diffable. ([example](examples/addonconfig-structured.yml)) |
| Raw string | `values.resourcesYaml` | Pasting manifests you already have. ([example](examples/addonconfig-inline.yml)) |

A `<cluster-name>-dayzero` ConfigMap in the same namespace also folds in, for payloads
managed by a separate pipeline ([example](examples/addonconfig-configmap.yml)).

### Security posture

The payload is applied by the addon system's own privileged identity, so treat write
access to a cluster's Supervisor namespace as equivalent to workload-cluster admin: a
tenant who can write the `AddonConfig` can apply arbitrary resources to their cluster.
That is the same authority a workload-cluster kubeconfig would grant, which is the point,
but it means the Supervisor namespace should not contain users who should not have it.

An `AddonConfig` is readable by anyone with access to the Supervisor namespace. Do not
inline credentials into one.

## Scope boundary

This is a seeding mechanism, not an installer. It is for pure configuration:
`Namespace`s, `ServiceAccount`s, RBAC, quotas, `Secret`s, CRs. Real workloads mean
container images and belong in their own addon or chart.

## Development

```sh
make bundle    # assemble the addon-repository bundle (build/bundle)
make render    # inspect the Package and the ACD it carries
make test      # render the ACD templates and the package ytt, validate against CRD schemas
make push      # publish the bundle to the registry
```

`make test` renders the AddonConfigDefinition's Go templates the way the addon controller
does and the package's ytt the way the guest does, then checks the full round trip: a
payload encoded into the values Secret comes back out as the original resources. It also
validates the ACD against the real CRD schema and pins the labels and namespace the
validating webhook requires. See [`docs/plan.md`](docs/plan.md).

### Releasing

Push a `v*` tag. GitHub Actions runs `make push` to publish the bundle and attaches the
install manifest to a GitHub release.

## Docs

| | |
|---|---|
| [`docs/design.md`](docs/design.md) | The API constraints, why the design is shaped this way, alternatives rejected |
| [`docs/verify.md`](docs/verify.md) | End-to-end verification runbook |
| [`docs/plan.md`](docs/plan.md) | Repo layout and build mechanics |
| [`docs/future-direct-creation.md`](docs/future-direct-creation.md) | The package-free, fetch-free design this aimed for, why it is blocked today, and when it might work |
