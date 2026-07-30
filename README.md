# dayzero-addon-service

[![Test](https://github.com/warroyo/dayzero-addon-service/actions/workflows/test.yml/badge.svg)](https://github.com/warroyo/dayzero-addon-service/actions/workflows/test.yml)
[![Build and Release](https://github.com/warroyo/dayzero-addon-service/actions/workflows/build-release.yml/badge.svg)](https://github.com/warroyo/dayzero-addon-service/actions/workflows/build-release.yml)
[![Latest release](https://img.shields.io/github/v/release/warroyo/dayzero-addon-service)](https://github.com/warroyo/dayzero-addon-service/releases/latest)

A VKS addon that applies operator-supplied Kubernetes YAML into a workload cluster at
provisioning time: namespaces, service accounts, RBAC, resource quotas, and other
day-zero configuration. Tenants attach it per cluster and pass the YAML in as data, so
there is no workload-cluster kubeconfig to hand out and nothing to rebuild per payload.

## How it works

The addon ships as a Carvel package repository. An administrator points an
`AddonRepository` at it, and the VKS addon manager creates the `Addon`,
`AddonRelease` and `AddonConfigDefinition` in `vmware-system-vks-public`. Tenants then
attach the addon with an `AddonInstall` and supply YAML through an `AddonConfig` per
cluster.

```mermaid
flowchart LR
  subgraph registry["Registry"]
    BUNDLE["addon-repo bundle<br/>Package + ACD (annotation)"]
  end

  subgraph supervisor["Supervisor"]
    REPO["AddonRepository<br/>AddonRepositoryInstall"]
    MAT["Addon, AddonRelease, ACD<br/><i>in vmware-system-vks-public</i>"]
    AINST["tenant: AddonInstall"]
    ACFG["AddonConfig<br/>spec.values"]
    SECRET["values Secret"]
  end

  subgraph guest["Workload cluster"]
    PI["PackageInstall"]
    RENDER["ytt renders your YAML,<br/>kapp applies"]
  end

  BUNDLE -->|fetch| REPO
  REPO -->|manager| MAT
  AINST --> ACFG
  MAT -.->|addon controller| ACFG
  ACFG -->|values| SECRET
  SECRET --> PI
  PI --> RENDER

  classDef locked fill:#fde2e2,stroke:#c0392b,color:#111
  classDef tenantc fill:#e3f2fd,stroke:#1565c0,color:#111
  classDef guestc fill:#e8f5e9,stroke:#2e7d32,color:#111
  class MAT locked
  class AINST,ACFG tenantc
  class PI,RENDER guestc
```

Whatever a tenant writes in `AddonConfig.spec.values` is rendered into a values Secret.
The guest package reads that Secret, ytt turns the values back into the original
resources, and kapp applies them. To change the payload you edit the `AddonConfig`; there
is no rebuild, version bump or re-upload.

## Install

There are two ways to install, and they end up in the same place: the same two CRs
pointing at the same published bundle. Pick whichever matches how your Supervisor is
administered. Each
[release](https://github.com/warroyo/dayzero-addon-service/releases/latest) ships the
artifact for both, already stamped with that release's bundle version.

### Method 1: the Supervisor Service

For environments where services are installed through the vCenter catalog. Download
`dayzero-addon.yml` from the [latest
release](https://github.com/warroyo/dayzero-addon-service/releases/latest):

```sh
gh release download --repo warroyo/dayzero-addon-service --pattern dayzero-addon.yml
```

Then upload it through **Workload Management → Services → Add Service** and create a
service instance. The deploy places the two CRs in the service's own namespace, where the
manager reconciles them. There is nothing to edit: the bundle image and versions are
already the package's values defaults, and you can override them from the service config
form.

### Method 2: apply the AddonRepository directly

For admins with Supervisor `kubectl` access. Download `dayzero-addonrepository.yml` from
the same release and apply it:

```sh
gh release download --repo warroyo/dayzero-addon-service --pattern dayzero-addonrepository.yml
kubectl apply -f dayzero-addonrepository.yml
```

[`install/addonrepository.yml`](install/addonrepository.yml) is the same file in-tree, for
pointing `imageURL` at a bundle you published yourself.

### Confirm it registered

```sh
kubectl -n vmware-system-vks-public get addon,addonrelease,acd | grep dayzero
```

## Usage

Each cluster needs two objects, both in the cluster's own Supervisor namespace.

First, attach the addon (once per namespace). See
[`examples/addoninstall.yml`](examples/addoninstall.yml). Then label the clusters to seed:

```sh
kubectl label cluster my-cluster addons.kubernetes.vmware.com/dayzero=enabled
```

Second, supply the payload: an `AddonConfig` named `<cluster-name>-dayzero`, carrying
the annotation `clusteraddon.addons.kubernetes.vmware.com/owned-for-deletion: "true"`.
Both the name and the annotation matter. The name is how the addon system pairs a config
with a cluster, and without the annotation the `AddonConfig` sticks around after the
`ClusterAddon` is gone.

There are two ways to supply the payload, and you can use both at once:

| Source | Field | Use when |
|---|---|---|
| Structured | `values.resources` | You are writing the payload alongside the AddonConfig and want clean diffs ([example](examples/addonconfig-structured.yml)) |
| Raw string | `values.resourcesYaml` | You are pasting in manifests you already have ([example](examples/addonconfig-inline.yml)) |

A `<cluster-name>-dayzero` ConfigMap in the same namespace gets merged in too, which helps
when a separate pipeline manages the payload
([example](examples/addonconfig-configmap.yml)).

### Security posture

The payload is applied by the addon system's own privileged identity, so treat write
access to a cluster's Supervisor namespace as equivalent to workload-cluster admin: a
tenant who can write the `AddonConfig` can apply arbitrary resources to their cluster.
That is the same authority a workload-cluster kubeconfig would grant, so it is not a new
hole, but it does mean you should not give write access to that namespace to anyone you
would not trust with the cluster itself.

An `AddonConfig` is readable by anyone with access to the Supervisor namespace. Do not
inline credentials into one.

## Scope boundary

This seeds configuration; it is not an installer. Use it for `Namespace`s,
`ServiceAccount`s, RBAC, quotas, `Secret`s and CRs. Anything that runs container images
should be its own addon or Helm chart.

## Development

```sh
make bundle              # assemble the addon-repository bundle (build/bundle)
make render              # inspect the Package and the ACD it carries
make test                # render the ACD templates and the package ytt, validate against CRD schemas
make push                # publish the bundle to the registry
make supervisor-service  # build the Supervisor Service package -> dayzero-addon.yml
make release             # both: push the bundle and build the service package
```

`make test` renders the AddonConfigDefinition's Go templates the way the addon controller
does and the package's ytt the way the guest does, then checks that a payload encoded into
the values Secret comes back out as the original resources. It also validates the ACD
against the real CRD schema and pins the labels and namespace the validating webhook
requires. See [`docs/plan.md`](docs/plan.md).

### Releasing

Push a `v*` tag. GitHub Actions runs `make release`, which publishes the addon-repository
bundle and builds the Supervisor Service package, then attaches both `dayzero-addon.yml`
and `dayzero-addonrepository.yml` to a GitHub release.

`supervisor-service/config/values.yml` is generated from
[`values.yml.tpl`](supervisor-service/values.yml.tpl) with the release version stamped in,
so the shipped service can only point at the bundle version it was built with. Edit the
template, not the generated file.

## Docs

| | |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | How the VKS addon kinds fit together, with a diagram. Generic to any addon |
| [`docs/design.md`](docs/design.md) | The API constraints, why the design ended up this way, and what was rejected |
| [`docs/verify.md`](docs/verify.md) | End-to-end verification runbook |
| [`docs/plan.md`](docs/plan.md) | Repo layout and build mechanics |
| [`docs/future-direct-creation.md`](docs/future-direct-creation.md) | The package-free, fetch-free design this aimed for, why it is blocked today, and when it might work |
