# Design & API reference

How the VKS addon API constrains this project, and why `config/` is shaped the way it
is. Established against the Broadcom VKS API reference, the Carvel docs and a live
Supervisor (VKS 3.7). Read this before touching `config/`.

## Why this exists

`AddonConfigDefinition`, `Addon` and `AddonRelease` cannot be created by hand.
`kubectl auth can-i create … --all-namespaces` returns `no` even for
`sso:Administrator@gpu.local`, and a direct apply into `vmware-system-vks-public`,
where the shipped catalog lives, is rejected with `Forbidden`. They come from exactly
two places: a Supervisor Service, or the controller reconciling an `AddonRepository`.
An administrator can create `AddonRepository` and `AddonRepositoryInstall`; tenants can
create `AddonInstall` and `AddonConfig` in their own vSphere namespace.

Installing this Supervisor Service is therefore one of only two routes that exist, and
the only one that keeps the addon package-free. See
[Alternatives rejected](#alternatives-rejected).

Which namespace the three CRs land in is a separate question, and the answer is not the
public catalog. `vmware-system-vks-public` is closed to a Supervisor Service's deployer
account too. This service creates them in the namespace the Supervisor deploys it into,
which its deployer account owns. Tenants reach them from their own namespace through
`AddonInstall.spec.addonRef.namespace`. Unset, that field falls back to the public
catalog.

So a one-time admin install permanently delegates "seed my workload cluster with
arbitrary YAML" to tenants, with:

- no workload-cluster kubeconfig
- no OCI registry
- no Helm chart
- no package repository

Addons are built for exactly this job, but essentially every shipped addon drags in
images, charts, or repositories. This one is pure data.

What tenants actually seed is the day-zero state of a cluster: namespaces, service
accounts, RBAC bindings to SSO groups, resource quotas, default network policies,
pull secrets. Ordinary configuration that otherwise arrives by handing someone a
workload-cluster kubeconfig.

Timing matters as much as the mechanism. The addon runs from the Supervisor during
provisioning, before the cluster is reachable by anything external, so the payload is
in place before anyone or anything else touches the cluster.

### Security posture

Arbitrary YAML applied with a `cluster-admin` binding is not an escalation under this
model. The tenant already owns that workload cluster; this gives them no more than a
kubeconfig would, minus the kubeconfig. The residual exposure is a vSphere namespace
containing users who should not have workload-cluster admin, which is why
`clusterRoleName` stays configurable.

### Scope boundary: seeding, not installing

The "no images" property holds only while payloads are pure config: `Application`s,
`Secret`s, `Namespace`s, CRs. Real workloads mean images, registries and air-gap
concerns, and belong in a Carvel package instead. This is a seeding mechanism.

---

## Package-free addons are legal

Without this the approach would not work at all.

> "Specifies Carvel package ref name and version; **unset means no package deployed**"

> "An AddonRelease should have package and/or definitionRef set."

An `AddonRelease` carrying only `addonConfigDefinitionRef` is a first-class,
package-free addon. No `AddonRepository` or `AddonRepositoryInstall` wiring is needed
either, because the Supervisor Service lays down `Addon` and `AddonRelease` directly.
Three shipped releases are built this way (`carvel-repo-1.0.0`,
`depot.kube-system.svc-1.0.0` and `helm-repo-1.0.0`), each carrying only `addonRef`,
`addonConfigDefinitionRef` and `version`.

Both refs carry an explicit namespace in `config/addonrelease.yml`.
`addonConfigDefinitionRef.namespace` is required by the CRD;
`addonRef.namespace` is optional and defaults to "the public namespace defined by the
addon manager", which is not where this addon lives.

## Output templates are the resource body only

This constrains the design more than anything else in the API.

> "template defines the Kubernetes resource template for output manifest generation.
> **This field only needs the resource specification (excluding TypeMeta and
> ObjectMeta) as the GVK, Name, and Namespace are already handled**..."

Consequences:

- The template must not contain `apiVersion`, `kind`, or `metadata`.
- One resource per output entry. Multi-document YAML is out.
- `apiVersion` and `kind` on `targetClusterOutput` are static literals. The template
  structurally cannot override them, because it excludes TypeMeta. This is the reason
  the [App relay](#the-app-relay) exists.
- Body shape varies by kind: `ClusterRoleBinding` takes a top-level `roleRef` and
  `subjects`, `ConfigMap` takes `data`, `Namespace` and `ServiceAccount` are
  effectively empty.

`name` and `namespace` on an output *are* templatable. The shipped cilium definition
uses `'{{.Cluster.name}}-cilium-data-values'`, external-secrets uses
`'{{.Cluster.name}}-{{.Addon.name}}-values'`, and under `renderForEach` the name comes
from `'{{.ForEach.CurrentDependency…}}'`. Only the GVK is fixed.

`referenceType` is ignored unless the kind is `Secret`. Verbatim from the CRD: "if the
kind is not a secret, then the referenceType will be ignored." Every shipped
non-`Secret` target output leaves it at the `ValuesRef` default and is still applied
directly into the guest; `vks-static`'s `PriorityClass` and `depot`'s `Service` both
exist in live workload clusters.

## kapp-controller needs no OCI artifact

kapp-controller does not require an imgpkg bundle. `imgpkgBundle` is one of six
`spec.fetch` types (`inline`, `image`, `imgpkgBundle`, `http`, `git`, `helmChart`), and
`inline` (v0.31.0 and later) carries content in the CR itself:

```yaml
- inline:
    paths:
      dir/file.ext: file-content
    pathsFrom:
    - configMapRef: {name: cfgmap-name, directoryPath: dir}
    - secretRef:    {name: secret-name, directoryPath: dir}
```

`PackageInstall` to `Package` to `fetch.imgpkgBundle` is the packaging layer; `App` is
the primitive beneath it. Emitting a bare `App` gets kapp-controller's
apply/prune/GC/status engine with no package to author and no registry to reach.

Two constraints on the `App` this addon emits:

- `spec.template` is required. "Template must have one or more directives." The
  available directives are `ytt`, `kbld`, `helmTemplate`, `cue` and `sops`.
- `spec.serviceAccountName` is documented as optional, but the App needs some
  credential: either a SA in its own namespace or `spec.cluster` with a kubeconfig
  secret.

kapp-controller is present in every VKS cluster. `apps.kappctrl.k14s.io` is registered
and a `kapp-controller` deployment runs in `tkg-system`, documented as a
system/bootstrap package (VKS 3.5 and later, "automatically installed, does not need
migration").

> The one imgpkg bundle in this project is for the Supervisor Service itself, which
> vCenter requires. It is unrelated to, and never consumed by, the guest cluster.

## Supervisor Service definition format

A Supervisor Service YAML uploaded to vCenter is a two-document Carvel manifest. The
shape below matches a shipped service, `sre-supervisor-role`, whose only job is
installing RBAC YAML. That is the closest prior art to this project:

```yaml
apiVersion: data.packaging.carvel.dev/v1alpha1
kind: PackageMetadata
metadata: {name: sre-supervisor-role.fling.vsphere.vmware.com}
spec:
  displayName: Supervisor Troubleshooting Access
  providerName: Broadcom Inc.
  shortDescription: ...
  longDescription: ...
  maintainers: [{name: ...}]
  categories: [CI/CD, GitOps]
  iconSVGBase64: none
---
apiVersion: data.packaging.carvel.dev/v1alpha1
kind: Package
metadata: {name: sre-supervisor-role.fling.vsphere.vmware.com.1.0.1}
spec:
  refName: sre-supervisor-role.fling.vsphere.vmware.com
  version: 1.0.1
  releasedAt: "2026-02-24T16:05:21Z"
  template:
    spec:
      fetch:    [{imgpkgBundle: {image: <registry>/<path>@sha256:...}}]
      template: [{ytt: {paths: [./config]}}, {kbld: {paths: ['-', .imgpkg/images.yml]}}]
      deploy:   [{kapp: {}}]
  valuesSchema:
    openAPIv3:
      properties: {...}
```

Uploaded via **Supervisor Management → Services → Add New Service → Upload**.

vCenter also documents a `CUSTOM_YAML` service-definition format ("inline YAML
document that follows a plain Kubernetes YAML format") as an alternative to
`CARVEL_APPS_YAML`, but no public example exists. Not pursued.

## Naming and placement conventions

- `AddonConfigDefinition` names look like `ako.kubernetes.vmware.com.1.13.4+vmware.1-vks.1`
- The shipped addon resources live in `vmware-system-vks-public`. This addon's live in
  the service's own namespace, and tenants reach them through
  `AddonInstall.spec.addonRef.namespace`
- `AddonConfig` must be named `<cluster-name>-<addon-name>` and created in the
  cluster's Supervisor namespace
- `AddonConfig` requires the annotation
  `clusteraddon.addons.kubernetes.vmware.com/owned-for-deletion: "true"`. Without it
  the `AddonConfig` survives `ClusterAddon` deletion

A shipped `AddonInstall`, for reference:

```yaml
apiVersion: addons.kubernetes.vmware.com/v1alpha1
kind: AddonInstall
metadata: {name: vks-cilium01-cilium, namespace: test-ns}
spec:
  addonRef: {name: cilium}
  clusters:
  - selector: {matchLabels: {cluster.x-k8s.io/cluster-name: vks-cilium01}}
    constraints:
      expression: "cluster.cniRefName() == 'cilium' && version_in_range(cluster.spec.topology.version, '>=1.35.0')"
  stopMatchingBehavior: Retain
```

---

## The template dialect

The dialect is undocumented; what follows is read off shipped definitions. It is Go
`text/template` plus sprig, not CEL. The CRD's own field description claims CEL
(`${values.arguments.paused}`), but that doc string is stale and the shipped
definitions are authoritative.

Context roots:

| Root | Contents |
|---|---|
| `.Values.<field>` | This definition's schema, populated from `AddonConfig.spec.values`. Capital V |
| `.Dependencies.<inputName>` | A resolved `templateInputResource`, as the whole object |
| `.Cluster.name` | The consuming cluster |
| `.Addon.name` | The addon |
| `.ForEach.CurrentDependency` | Current item, inside a `renderForEach` output |

Functions in use across shipped definitions: `list`, `append`, `contains`, `index`,
`toYaml`, `indent`, `fail`, `b64enc`, `hasKey`, `default`, `quote`, `ne`, `and`, plus
Go 1.18's `break`. That is sprig, with the caveat that `toYaml` is not a sprig
function. It is a Helm addition, and the controller registers it the same way (the
shipped external-secrets definition depends on it). VMware also registers at least one
custom function, `getRegistryAuth`, used by the shipped helm-repo definition. `toJson`
is therefore safe to rely on. `test/render_test.go` reproduces this function map so
template bugs surface locally.

There is no helm field on an `AddonConfigDefinition`. `spec` has exactly four keys:
`addonInstallPermission`, `schema`, `templateInputResources`,
`templateOutputResources`. The Helm resemblance is only the sprig function set inside
`template`. There is no chart engine, no multi-document output, no `_helpers.tpl` and
no `.Files`, so it does nothing to relax the static-GVK constraint above.

`AddonRelease.spec.kubernetesVersionConstraints` is a plain semver range string. Live
values: `>=1.35.0 <1.36.0`, `=1.35.5+vmware.1-vkr.1`, `>=1.32.0`.

## RBAC and where `addonInstallPermission` applies

Emitting a `ClusterRoleBinding` to `cluster-admin` in the guest is not blocked by
escalation-prevention. The shipped `vks-static-v1.34.8---vmware.1-vkr.1-def` emits
exactly that shape: a `rbac.authorization.k8s.io/v1/ClusterRoleBinding` target output
binding `vks:apiserver-kubelet-client` to `system:kubelet-api-admin`.

`spec.addonInstallPermission.accessPolicies` grants RBAC in the Supervisor namespace,
for reading `templateInputResources` and writing a `supervisorNamespaceOutput`, not in
the guest cluster. `vks-static` declares none at all yet writes cluster-scoped guest
resources. This definition declares only `get`/`list`/`watch` on `configmaps`, for the
optional payload ConfigMap.

## How packaged addons get their definition

The shipped addons arrive through a route this project does not use. A Carvel
`Package` carries the entire definition as a gzip+base64 annotation,
`addons.kubernetes.vmware.com/addon-config-definition`, alongside
`addon.kubernetes.vmware.com/addon-name` and
`addons.kubernetes.vmware.com/upgrade-strategy`. Decoding cilium's yields its
definition verbatim. The controller materialises `Addon`, `AddonRelease` and
`AddonConfigDefinition` from it.

A `Package`-derived `AddonRelease` gets `spec.package` set, forcing a real Carvel
package and a guest-side `PackageInstall`, and it means publishing and maintaining an
addon repository on top of the Supervisor Service. See
[Alternatives rejected](#alternatives-rejected).

---

## The App relay

Because GVK is structurally static, the definition emits a small fixed set of
known-GVK resources into the guest cluster. The last of them is an `App` carrying the
tenant's arbitrary YAML inline, and kapp-controller applies whatever is inside.

| # | GVK | Scope | Purpose |
|---|-----|-------|---------|
| 1 | `v1/Namespace` | Cluster | Holds the App and its SA |
| 2 | `v1/ServiceAccount` | Namespace | Identity kapp-controller applies with |
| 3 | `rbac.authorization.k8s.io/v1/ClusterRoleBinding` | Cluster | Binds SA to a configurable ClusterRole |
| 4 | `kappctrl.k14s.io/v1alpha1/App` | Namespace | Carries payload, does the applying |

None set `referenceType`, since it is ignored for every kind but `Secret`.

All four are named `vks-bootstrap`. Names could be templated, but there is nothing
per-cluster to encode, because each cluster gets its own copy inside its own cluster.
Keeping them literal also avoids interleaving ytt substitution with the Go templates
the addon controller evaluates later.

Payload values are JSON-encoded into the `App`'s inline paths rather than indented in.
A JSON string is a valid YAML scalar, so arbitrary multi-line YAML embeds with no
indentation arithmetic and no sensitivity to odd whitespace in the payload.

The relay buys any GVK, any resource count, pruning and GC on change, deletion on
removal, retry with backoff, and a real status object to debug against.

---

## Alternatives rejected

**Fixed set of output kinds.** Declare outputs for exactly the kinds worth caring about
(Namespace, Secret, Application, ConfigMap). Fully native, no kapp-controller, no
extra in-cluster object. Rejected because any unanticipated kind requires a platform
admin to version the definition and reinstall the service, which puts the admin back in
the loop and destroys the delegation this exists for.

**`renderForEach` over label-selected ConfigMaps.** Iterates inputs with a `multiple`
constraint, but still cannot vary output GVK. Dead end for generic YAML.

**CAPI `ClusterResourceSet`.** Would have meant zero guest-cluster footprint and no
kapp-controller dependency. Out of scope for this project.

**VKS pre-created objects.** VKS picks up certain objects pre-created in the vSphere
namespace during provisioning (`kapp-controller-package` is the documented example).
Scoped to specific CRs the TKR controller recognises, and it does not accept arbitrary
YAML, so it is not a substitute.

**Ship the three CRs as plain YAML, no bundle.** The shipped `carvel-repo`, `depot`
and `helm-repo` triples have no package and no `ownerReferences` to any Carvel
`Package`, so they look like plain CRs, and the Supervisor already runs an Argo CD
instance that manages every `AddonConfig` in the tenant namespaces. Delivering the
addon the same way would take the dependency count to literal zero. It is not possible:
those three kinds come only from a Supervisor Service or an `AddonRepository`
reconcile, in any namespace. Which namespace they land in does not change that.

**`AddonRepository` pointing at a Helm repository.** `AddonRepository.spec.fetch`
accepts `helmRepository: {url}` as well as `imgpkgBundle`. A live example points at
`https://charts.external-secrets.io`, and the controller auto-generates the `Addon`,
`AddonRelease` (with `dependsOn: helm-controller`) and `AddonConfigDefinition` from
the chart with no authoring at all. "Install any Helm chart as a VKS addon" is a solved
problem, but it needs a chart, which reintroduces exactly the per-payload artifact this
project exists to eliminate.

**`AddonRepository` pointing at an imgpkg bundle.** The other route an administrator is
permitted to drive. Rejected, and not held in reserve, for two independent reasons:

1. A `Package`-derived `AddonRelease` carries `spec.package`, which forces a Carvel
   package and a guest-side `PackageInstall`. The package-free property is the whole
   premise.
2. It adds a second external artifact: a published addon repository that has to be
   built, hosted, versioned and relocated for air-gap, alongside the Supervisor
   Service. The Supervisor Service is meant to be the only external artifact this
   project requires. That constraint is part of the design rather than a convenience.

The same constraint is why the addon CRs go in the service's own namespace. The
deployer account owns it already, so there is no extra RBAC to grant, no
escalation-prevention corner, and no second delivery mechanism.

---

## Dependencies

The claim is provisioning-time seeding with no per-payload artifacts, not zero
dependencies. What remains:

1. One imgpkg bundle, the Supervisor Service itself. The OCI dependency moves from
   per-payload to once, ever. That is a large reduction rather than zero, and no route
   avoids it, because the "plain CRs" alternative is closed by RBAC.
2. kapp-controller, in-guest, pre-existing and VMware-managed. Its presence is a VKS
   implementation detail rather than a contract. The design dodges Carvel *packaging*
   by leaning on the Carvel *runtime*.
3. An undocumented template dialect, mapped above and covered by tests, but still
   addon-controller internals that may shift across VKS versions. The tests encode an
   understanding of it, not a contract.

What genuinely is dependency-free: no package to author, version and publish per
payload; no container images, so the `ImagesLock` is empty and air-gap relocation is
trivial; no `AddonRepository` wiring.

---

## Sources

- [VKS API reference](https://developer.broadcom.com/xapis/vmware-vsphere-kubernetes-service/latest/api-docs.html)
- [Managing Add-ons in VKS Clusters](https://techdocs.broadcom.com/us/en/vmware-cis/vcf/vcf-service-administration-and-development/9-0/managing-vsphere-kuberenetes-service-clusters-and-workloads/managing-add-ons-in-vks-clusters.html)
- [Configure an Add-on at Installation Time](https://techdocs.broadcom.com/us/en/vmware-cis/vcf/vcf-service-administration-and-development/9-0/managing-vsphere-kuberenetes-service-clusters-and-workloads/managing-add-ons-in-vks-clusters/configure-an-add-on-at-installation-time.html)
- [kapp-controller App CR spec](https://carvel.dev/kapp-controller/docs/v0.50.x/app-spec/)
- [vsphere-tmm/Supervisor-Services](https://vsphere-tmm.github.io/Supervisor-Services/), real service definition YAML
- [Leveraging Cilium CNI on vSphere VKS Clusters](https://medium.com/@bob-bauer/leveraging-cilium-cni-on-vsphere-vks-clusters-9070ab70c309), real AddonInstall and AddonConfig examples
