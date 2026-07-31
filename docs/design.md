# Design & API reference

How the VKS addon API constrains this project and why the code is shaped the way it is.
Established against the Broadcom VKS API reference, the Carvel docs, and a live Supervisor
and guest cluster (VKS 3.7). Read this before touching `addon/`.

For how the addon kinds fit together in general, with a diagram, see
[`architecture.md`](./architecture.md). For the package-free, fetch-free design this
project originally aimed for, and why it is blocked today, see
[`future-direct-creation.md`](./future-direct-creation.md).

## Why an AddonRepository

`Addon` and `AddonRelease` are owned by the VKS addon manager. Two validating webhooks,
`addon.validating.vmware.com` and `addonrelease.validating.vmware.com`, reject every
mutating operation on those kinds from any client that is not the manager's own service
account. A service account with `create` RBAC on them is still denied at admission, and
create, update, and both client-side and server-side apply all fail identically. The
manager runs with `--enable-webhook-client-verification`, which is the caller-identity
check behind this.

The `AddonConfigDefinition` is not locked, but it is inert on its own: an `AddonInstall`
reaches an ACD only through a selected `AddonRelease`, and requires the `Addon` to
reconcile at all. So authoring the ACD directly gets us nothing usable.

The one route that makes the manager create all three is an `AddonRepository`. An
administrator can create `AddonRepository` and `AddonRepositoryInstall` (verified), the
manager reconciles them, and it materialises the `Addon`, `AddonRelease` and ACD in
`vmware-system-vks-public`. That is the design.

## The guest fetch is unavoidable

An `AddonRepository` produces an `AddonRelease` with `spec.package` set, which makes the
addon controller create a guest `PackageInstall`, and the guest acquires the package
through a package repository that fetches a bundle. Every VKS addon works this way. So the
guest pulls the addon-repository bundle, and no arrangement avoids it. This project does
not try to; instead the bundle is config-only, so relocation for air-gap is trivial.

The guest fetch does not reach an arbitrary external registry directly. Guest image pulls
route through an in-cluster proxy (`mgmt-image-proxy.kube-system.svc`, backed by the
depot) that the Supervisor feeds. This is the VKS isolation model.

## What ships: a package repository with the ACD in an annotation

The bundle (`make bundle`) is a Carvel package repository: a `PackageMetadata` and one
`Package` per released version. Each `Package` carries our hand-written
`AddonConfigDefinition` in its `addons.kubernetes.vmware.com/addon-config-definition`
annotation as gzip+base64. This is exactly how the shipped cilium addon delivers its
definition, and it is why our own ACD (schema, validation, output resources) survives the
`AddonRepository` route intact. `make freeze` renders the ACD and encodes it into the
annotation, so the ACD is edited as a readable file.

Each `Package` also declares `kubernetesVersionSelection.constraints`, which lands on its
generated `AddonRelease`.

It is a catalog, not a shipping container for the newest version, because an
`AddonRepository` is immutable once installed (see below). Carrying every released version
at once means the manager materialises an `AddonRelease` for each, so a consumer changing
which version a cluster runs only moves a `releaseFilter` pin — no Supervisor-admin access,
no new resources, and older versions stay available to roll back to. The mechanics are in
[`plan.md`](./plan.md); the frozen-YAML rule that keeps old versions honest is there too.

## How the payload reaches the guest

The tenant payload flows as data, in three hops:

1. A tenant writes resources in `AddonConfig.spec.values` (`resources`, `resourcesYaml`,
   or a `<cluster>-dayzero` ConfigMap).
2. The ACD's output template renders those into a values `Secret`. The addon controller
   wires that Secret into the guest `PackageInstall` as its values.
3. The package's ytt reads the values and emits the resources; kapp applies them.

The Secret is the standard mechanism: the shipped cilium ACD emits a
`<cluster>-cilium-data-values` Secret the same way, with `referenceType: ValuesRef` on
the Supervisor side and a plain copy in the guest package namespace. This project mirrors
that shape exactly, since the ACD-to-PackageInstall wiring is addon-controller internals
that can only be fully exercised on a real install.

### Encoding the payload

Arbitrary YAML cannot be passed through ytt data values naively, because ytt interprets
`#@`. So the ACD encodes the payload and the package decodes it:

- `resources` (a structured list) becomes a JSON string, `resourcesJson`, via
  `toJson | toJson`. The package runs `json.decode` and emits each item.
- `resourcesYaml` (a raw multi-document string) and any ConfigMap data are passed as a
  single string, JSON-encoded so newlines and colons survive. The package splits on the
  document separator and `yaml.decode`s each.

`test/render_test.go` renders this whole chain and asserts a payload comes back out as
the resources it went in as. The guest-side half was also validated directly: a
`PackageInstall` pointing at an inline package with this exact render turned per-cluster
values into the expected resources.

## Output templates are the resource body only

The single most constraining API rule, and the reason the payload is encoded rather than
templated in place:

> "template defines the Kubernetes resource template for output manifest generation. This
> field only needs the resource specification (excluding TypeMeta and ObjectMeta) as the
> GVK, Name, and Namespace are already handled."

So an output template must not contain `apiVersion`, `kind`, or `metadata`, and it is one
resource per output entry. `name` and `namespace` are templatable (this project uses
`{{.Cluster.name}}-dayzero-data-values`), but the GVK is a static literal. That is why
the addon cannot emit arbitrary payload kinds directly, and why the package renders them
instead.

## The template dialect

The output templates are Go `text/template` plus sprig, evaluated per cluster by the addon
controller (not CEL, despite a stale CRD field description). Context roots are `.Values`
(the AddonConfig values, capital V), `.Dependencies.<inputName>` (a resolved
`templateInputResource`), `.Cluster` and `.Addon`. `toJson` is sprig; `toYaml` is a Helm
addition the controller also registers. `test/render_test.go` reproduces this function map
so template bugs surface locally.

## RBAC and where `addonInstallPermission` applies

`spec.addonInstallPermission.accessPolicies` grants rights in the Supervisor namespace, for
reading `templateInputResources` and writing the output Secret, not in the guest. This
definition declares `get`/`list`/`watch` on configmaps (the optional payload ConfigMap)
and full access to secrets (the output values Secret), matching the shipped cilium ACD.

The payload itself is applied in the guest by the addon system's `PackageInstall` identity,
which is privileged enough to install cluster addons. This project does not control that
identity, so the payload applies with the addon system's rights. This is the one place the
native design gives up a knob the App-relay alternative had (a configurable ClusterRole);
see [Alternatives rejected](#alternatives-rejected).

## Naming and placement conventions

- `Addon`, `AddonRelease` and `AddonConfigDefinition` all live in
  `vmware-system-vks-public`. The manager places them there.
- Each must carry `addon.kubernetes.vmware.com/addon-name` (the addon name); the shipped
  addons also carry `addon.kubernetes.vmware.com/addon-namespace`. The validating webhook
  rejects the resources without the name label. The CRD schemas do not cover this, so
  `make test` pins it.
- An `AddonRepository` must carry the `addons.kubernetes.vmware.com/package-offerings`
  annotation (a JSON listing of the packages it offers), or the webhook rejects it. It is
  read as an exact manifest of the bundle, so it is generated from the version list rather
  than hand-maintained.
- An `AddonRepository` is frozen once an `AddonRepositoryInstall` references it.
  `addonrepositories.validating.vmware.com` permits only `spec.addonFilters` to change, and
  that is a `helmRepository` field, so an imgpkg repository is immutable outright. Verified:
  a server-side dry run changing only `spec.fetch.imgpkgBundle.imageURL` was rejected with
  `AddonRepository is in use by an AddonRepositoryInstall`. Deletion is still allowed. A new
  catalog release therefore gets its own name and its own `targetRepositoryName` (which
  names the backing Carvel `PackageRepository`, so two catalogs must not share one) and the
  superseded pair is deleted once pins have moved. The shipped repositories work the same
  way: `standard-packages` 3.6 stays registered next to `vks-addons-3.7.0`.
- `AddonConfig` must be named `<cluster-name>-<addon-name>` and created in the cluster's
  Supervisor namespace, with the `owned-for-deletion` annotation so it is garbage
  collected with the `ClusterAddon`.

## Alternatives rejected

**Direct creation by a Supervisor Service.** Have the service create the `Addon`,
`AddonRelease` and ACD itself, package-free, with an inline App delivering the payload to
the guest with no fetch. This is the ideal, and it is blocked only by the manager-only
webhooks. Documented in full in
[`future-direct-creation.md`](./future-direct-creation.md), kept because the block is a
platform policy that could change.

**A helm AddonRepository.** `AddonRepository.spec.fetch` also accepts a `helmRepository`,
and a helm-derived addon is package-free on the `AddonRelease`. Rejected because the
manager generates the ACD from the chart with a fixed `helmValues`/`helmOptions` schema,
so our own definition and tenant API are lost, and it drags in helm-controller as a
required addon. The imgpkg route keeps our ACD and runs on kapp-controller alone.

**The App-relay with a stub package.** Keep the ACD emitting a Namespace, ServiceAccount,
ClusterRoleBinding and an inline App carrying the payload, with the forced package as a
no-op stub. Keeps a configurable `clusterRoleName`. Rejected as not minimal: it leaves two
guest-side mechanisms (a do-nothing PackageInstall next to the App) where the native
package does the whole job with one. The `clusterRoleName` knob is the cost of that
simplification.

**ytt in the payload.** Let tenants supply ytt rather than plain YAML. Rejected: the
`AddonConfig` is already per-cluster, so per-cluster variation needs no templating. ytt
would only add value for patching a base the addon ships, which is a different product
than a generic seeder.

## Sources

- [VKS API reference](https://developer.broadcom.com/xapis/vmware-vsphere-kubernetes-service/latest/api-docs.html)
- [Managing Add-ons in VKS Clusters](https://techdocs.broadcom.com/us/en/vmware-cis/vcf/vcf-service-administration-and-development/9-0/managing-vsphere-kuberenetes-service-clusters-and-workloads/managing-add-ons-in-vks-clusters.html)
- [kapp-controller App CR spec](https://carvel.dev/kapp-controller/docs/v0.50.x/app-spec/)
- [Leveraging Cilium CNI on vSphere VKS Clusters](https://medium.com/@bob-bauer/leveraging-cilium-cni-on-vsphere-vks-clusters-9070ab70c309), real AddonInstall and AddonConfig examples
