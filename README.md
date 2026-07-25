# bootstrap-addon-service

A vSphere Supervisor Service that installs a single VKS addon whose only job is to
apply operator-supplied Kubernetes YAML into a workload cluster at provisioning time.

No Carvel package. No Helm chart. No container images. No package repository.

> **Status: built, not yet verified against a live Supervisor.** The template logic is
> covered by tests (`make test`); the install path has not been exercised end to end.
> See [Verifying](#verifying).

## What problem this solves

`AddonConfigDefinition`, `Addon` and `AddonRelease` live in `vmware-system-vks-public`
and cannot be created by hand — not even by the vSphere administrator. They are
controller-owned. Tenants, meanwhile, can freely create `AddonInstall` and
`AddonConfig` in their own vSphere namespace.

So a **one-time admin install** of this service permanently delegates "seed my
workload cluster with arbitrary YAML" to tenants, with no workload-cluster
kubeconfig, no registry, no chart, and no repo.

In practice that means the day-zero state a cluster needs before anyone uses it —
namespaces, service accounts, RBAC bindings to SSO groups, resource quotas, default
network policies, pull secrets. Resources that otherwise arrive by handing someone a
workload-cluster kubeconfig and having them run `kubectl`. See
[`examples/platform-baseline/`](examples/platform-baseline/).

Because the addon runs from the Supervisor during provisioning, the payload is in
place before anything else reaches the cluster, which also makes it a reasonable place
to plant whatever a later tool needs to take over.

Addons are built for this job, but essentially every shipped addon drags in images,
charts, or repositories. This one is pure data — which is legal because
`AddonRelease.spec.package` is optional, and an `AddonRelease` carrying only an
`addonConfigDefinitionRef` is a first-class package-free addon. Three shipped VKS
addons (`carvel-repo`, `depot`, `helm-repo`) are built exactly that way.

## How it works

An `AddonConfigDefinition` output resource has a structurally static GVK, so the addon
cannot emit arbitrary kinds directly. Instead it emits four fixed-GVK resources into
the guest cluster, the last being a kapp-controller `App` carrying the payload inline.
kapp-controller — already present in every VKS cluster — applies whatever is inside,
with pruning, GC, retry and status for free.

```
Supervisor                          Workload cluster
──────────                          ────────────────
Addon                    ┌────────► Namespace           vks-bootstrap
AddonRelease (no pkg)    │          ServiceAccount      vks-bootstrap
AddonConfigDefinition ───┤          ClusterRoleBinding  vks-bootstrap
                         └────────► App (kapp-controller)
AddonInstall  ┐                       └── your YAML, applied
AddonConfig   ┘ per tenant
```

Changing the payload is a data edit — no rebuild, no version bump, no re-upload.

## Install

1. Download `bootstrap-addon.yml` from the [latest release](https://github.com/warroyo/bootstrap-addon-service/releases).
2. In vCenter, go to **Workload Management → Services → Add Service** and upload it.
3. Install it on the Supervisor.

Confirm the addon registered:

```sh
kubectl -n vmware-system-vks-public get addon,addonrelease,acd | grep bootstrap
```

### Air-gapped

The addon ships no container images, so the images lock is empty and relocation is
just the bundle itself — there is nothing to mirror:

```sh
imgpkg copy -b <bundle-ref-from-bootstrap-addon.yml> --to-repo your-registry.example.com/bootstrap-addon
```

Then replace the registry prefix in `bootstrap-addon.yml` (the digest is unchanged)
and follow the steps above.

## Usage

Two objects per cluster, both in the cluster's own Supervisor namespace.

**1. Attach the addon** — once per namespace, see
[`examples/addoninstall.yml`](examples/addoninstall.yml):

```sh
kubectl label cluster my-cluster addons.kubernetes.vmware.com/bootstrap=enabled
```

**2. Supply the payload** — an `AddonConfig` named `<cluster-name>-bootstrap`, carrying
the annotation `clusteraddon.addons.kubernetes.vmware.com/owned-for-deletion: "true"`.
Both the name and the annotation are load-bearing: the name is how the addon system
pairs config to cluster, and without the annotation the `AddonConfig` outlives the
`ClusterAddon`.

Three payload sources, which compose:

| Source | Field | Use when |
|---|---|---|
| Structured | `values.resources` | Payload is authored with the AddonConfig. Diffable. ([example](examples/addonconfig-structured.yml)) |
| Raw string | `values.resourcesYaml` | Pasting manifests you already have. ([example](examples/addonconfig-inline.yml)) |
| ConfigMap | `<cluster-name>-bootstrap` in the same namespace | Payload is large or managed by a separate pipeline. ([example](examples/addonconfig-configmap.yml)) |

Other values:

| Value | Default | Description |
|---|---|---|
| `clusterRoleName` | `cluster-admin` | ClusterRole the payload is applied with |
| `syncPeriod` | `10m` | How often kapp-controller re-reconciles the payload |
| `noopDelete` | `false` | When true, removing the addon leaves the payload in place |

### Security posture

Arbitrary YAML applied with a `cluster-admin` binding is not an escalation here. The
tenant already owns that workload cluster; this gives them no more than a kubeconfig
would, minus the kubeconfig. The residual exposure is a Supervisor namespace
containing users who should *not* have workload-cluster admin — which is why
`clusterRoleName` is configurable.

Note that an `AddonConfig` is readable by anyone with access to the Supervisor
namespace. Do not inline credentials into one.

## Scope boundary

This is a **seeding** mechanism, not an installer. The "no images" property holds only
while payloads are pure config — `Application`s, `Secret`s, `Namespace`s, CRs. Real
workloads mean images, registries and air-gap concerns, and belong in a Carvel package
instead.

## Development

```sh
make render   # ytt over config/
make test     # render the ACD's Go templates the way the addon controller will
```

`make test` exists because there is no fast loop against a real Supervisor: an
`AddonConfigDefinition` cannot be created by hand, so the only way to exercise the
real controller is to build, upload and install the service. In its place the tests:

- validate the rendered resources against the real CRD schemas in `test/schemas/`,
  more strictly than the API server does — unknown fields are rejected rather than
  silently pruned, so a typo fails the build instead of producing a quietly wrong
  definition;
- render every output template with the controller's own function map (sprig plus the
  Helm-style `toYaml`) across all payload-source combinations, asserting the results
  parse and stay body-only;
- check the three resources actually reference each other.

`kubectl apply --dry-run=client` is not a substitute — it validates nothing at all,
accepting invalid enum values and invented fields. Server-side dry run hits the same
RBAC wall as a real apply.

### Releasing

Push a `v*` tag; GitHub Actions runs `kctrl package release`, assembles
`bootstrap-addon.yml` and attaches it to a GitHub release.

```sh
git tag v1.0.0 && git push origin v1.0.0
```

Locally: `VERSION=1.0.0 make release`.

## Verifying

Untested end to end. The open question is whether the Supervisor Service's generated
deployer service account can write to `vmware-system-vks-public` — its RBAC is neither
listable nor impersonatable, so only an install answers it. If it cannot, the fix is to
ship the required RBAC inside the service; the Supervisor Service is deliberately the
only external artifact this project requires, and that does not change. Details in
[`docs/verify.md`](docs/verify.md).

## Docs

| | |
|---|---|
| [`docs/design.md`](docs/design.md) | API findings verified against a live Supervisor, why the design is shaped this way, alternatives rejected |
| [`docs/plan.md`](docs/plan.md)     | Repo layout and build/release mechanics |
| [`docs/verify.md`](docs/verify.md) | End-to-end verification runbook |
