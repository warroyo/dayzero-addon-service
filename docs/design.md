# Design & API findings

Research notes backing `docs/plan.md`. Originally established against the Broadcom VKS
API reference and Carvel docs, then **verified against a live Supervisor** (VKS 3.7).
Findings that the live environment corrected are marked as such.
**Read this before touching `config/`.**

## Why this exists

`AddonConfigDefinition`, `Addon` and `AddonRelease` live in `vmware-system-vks-public`
and **cannot be created by hand at all** — `kubectl auth can-i create … --all-namespaces`
returns `no` even for `sso:Administrator@gpu.local`, and a direct apply is rejected
with `Forbidden`. They are controller-owned. What an administrator *can* create in
that namespace is `AddonRepository` and `AddonRepositoryInstall`; tenants can create
`AddonInstall` and `AddonConfig` in their own vSphere namespace.

That is stronger than originally assumed (the first draft of this document said
"only platform admins can create AddonConfigDefinition"). It means installing this
Supervisor Service is not merely the convenient route — it is one of only two routes
that exist, and the only one that preserves the package-free property. See
[Alternatives rejected](#alternatives-rejected).

So a **one-time admin install** of this Supervisor Service permanently delegates
"seed my workload cluster with arbitrary YAML" to tenants, with:

- no workload-cluster kubeconfig
- no OCI registry
- no Helm chart
- no package repository

Addons are built for exactly this job, but essentially every shipped addon drags in
images, charts, or repositories. This one is pure data.

Resolving the Argo CD chicken-and-egg falls out of that: the addon runs from the
Supervisor at provisioning time, before the guest cluster is reachable by anything
external, so it can plant the root `Application` and repo credentials that let Argo
take over.

### Security posture

Arbitrary YAML applied with a `cluster-admin` binding is **not** an escalation under
this model. The tenant already owns that workload cluster; this gives them no more
than a kubeconfig would, minus the kubeconfig. The only residual exposure is a
vSphere namespace containing users who should not have workload-cluster admin —
hence `clusterRoleName` stays configurable.

### Scope boundary: seeding, not installing

The "no images" property holds only while payloads are pure config — `Application`s,
`Secret`s, `Namespace`s, CRs. Real workloads mean images, registries and air-gap
concerns, and belong in a Carvel package instead. This is a seeding mechanism.

---

## Finding 1 — `AddonRelease.spec.package` is optional

This is what makes the whole approach legal rather than a hack.

> "Specifies Carvel package ref name and version; **unset means no package deployed**"

> "An AddonRelease should have package and/or definitionRef set."

An `AddonRelease` carrying only `addonConfigDefinitionRef` is a first-class,
package-free addon. No `AddonRepository` / `AddonRepositoryInstall` wiring is needed
either, because the Supervisor Service lays down `Addon` + `AddonRelease` directly.

**Verified.** Verbatim from the API reference, and confirmed on a live Supervisor by
three shipped package-free releases: `carvel-repo-1.0.0`, `depot.kube-system.svc-1.0.0`
and `helm-repo-1.0.0`, each carrying only `addonRef`, `addonConfigDefinitionRef` and
`version`.

## Finding 2 — `TemplateOutputResource.template` is the resource *body only*

The single most design-constraining fact.

> "template defines the Kubernetes resource template for output manifest generation.
> **This field only needs the resource specification (excluding TypeMeta and
> ObjectMeta) as the GVK, Name, and Namespace are already handled**..."

Consequences:

- The template must **not** contain `apiVersion`, `kind`, or `metadata`.
- **One resource per output entry.** Multi-document YAML is out.
- `apiVersion` / `kind` on `targetClusterOutput` are **static literals** — the
  template structurally cannot override them, because it excludes TypeMeta.
- Body shape varies by kind: `ClusterRoleBinding` → top-level `roleRef` + `subjects`;
  `ConfigMap` → `data`; `Namespace` / `ServiceAccount` → effectively empty.

**Verified.** GVK is static — decisive, and the reason the relay below exists.

Two sub-findings were **wrong in the first draft** and are corrected by the live API:

- **`name` and `namespace` on an output *are* templatable.** The shipped cilium
  definition uses `'{{.Cluster.name}}-cilium-data-values'`, and external-secrets uses
  `'{{.Cluster.name}}-{{.Addon.name}}-values'`. Under `renderForEach` the name comes
  from `'{{.ForEach.CurrentDependency…}}'`. Only the GVK is fixed.
- **`referenceType` is ignored unless the kind is `Secret`.** Verbatim from the CRD:
  "if the kind is not a secret, then the referenceType will be ignored." Every shipped
  non-`Secret` target output leaves it at `ValuesRef` and is still applied directly
  into the guest — confirmed by `vks-static`'s `PriorityClass` and `depot`'s `Service`
  both existing in a live workload cluster. The first draft's insistence on
  `referenceType: Direct` everywhere was harmless but meaningless.

## Finding 3 — kapp-controller needs no OCI artifact

A reasonable objection to the relay is that kapp-controller requires an imgpkg
bundle. It does not. `imgpkgBundle` is one of six `spec.fetch` types (`inline`,
`image`, `imgpkgBundle`, `http`, `git`, `helmChart`), and `inline` (≥ v0.31.0)
carries content in the CR itself:

```yaml
- inline:
    paths:
      dir/file.ext: file-content
    pathsFrom:
    - configMapRef: {name: cfgmap-name, directoryPath: dir}
    - secretRef:    {name: secret-name, directoryPath: dir}
```

`PackageInstall` → `Package` → `fetch.imgpkgBundle` is the *packaging* layer; `App`
is the primitive beneath it. Emitting a bare `App` gets kapp-controller's
apply/prune/GC/status engine with no package to author and no registry to reach.

Two constraints on the `App` we emit:

- **`spec.template` is required** — "Template must have one or more directives."
  Available directives: `ytt`, `kbld`, `helmTemplate`, `cue`, `sops`.
- **`spec.serviceAccountName` is documented optional**, but the App needs *some*
  credential: either a SA in its own namespace or `spec.cluster` with a kubeconfig
  secret.

**Verified in a live guest cluster.** `apps.kappctrl.k14s.io` is registered and a
`kapp-controller` deployment runs in `tkg-system`. Documented as a system/bootstrap
package (VKS ≥ 3.5, "automatically installed, does not need migration").

> Note: the one imgpkg bundle in this project is for the **Supervisor Service
> itself**, which vCenter requires. It is unrelated to, and never consumed by, the
> guest cluster.

## Finding 4 — Supervisor Service definition format

A Supervisor Service YAML uploaded to vCenter is a two-document Carvel manifest.
Confirmed against a real shipped service (`sre-supervisor-role`, whose only job is
installing RBAC YAML — the closest prior art to this project):

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
`CARVEL_APPS_YAML`, but no public example was found. Not pursued.

## Finding 5 — naming conventions observed on live Supervisors

- `AddonConfigDefinition` names look like `ako.kubernetes.vmware.com.1.13.4+vmware.1-vks.1`
- Addon resources live in `vmware-system-vks-public`
- `AddonConfig` must be named `<cluster-name>-<addon-name>` and created in the
  **cluster's** Supervisor namespace
- `AddonConfig` requires the annotation
  `clusteraddon.addons.kubernetes.vmware.com/owned-for-deletion: "true"` — without it
  the `AddonConfig` survives `ClusterAddon` deletion

Real observed `AddonInstall`:

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

## Finding 6 — the template dialect (verified)

The dialect is undocumented, so it was read off shipped definitions. It is **Go
`text/template` plus sprig**, not CEL — note that the CRD's own field description
claims CEL (`${values.arguments.paused}`). The doc string is stale; trust the shipped
definitions.

Context roots:

| Root | Contents |
|---|---|
| `.Values.<field>` | This definition's schema, populated from `AddonConfig.spec.values`. Capital V |
| `.Dependencies.<inputName>` | A resolved `templateInputResource`, as the whole object |
| `.Cluster.name` | The consuming cluster |
| `.Addon.name` | The addon |
| `.ForEach.CurrentDependency` | Current item, inside a `renderForEach` output |

Functions observed in shipped definitions: `list`, `append`, `contains`, `index`,
`toYaml`, `indent`, `fail`, `b64enc`, `hasKey`, `default`, `quote`, `ne`, `and`, plus
Go 1.18's `break`. That is sprig — with the caveat that **`toYaml` is not a sprig
function**; it is a Helm addition, and the controller clearly registers it the same
way (the shipped external-secrets definition depends on it). VMware also registers at
least one custom function, `getRegistryAuth`, used by the shipped helm-repo
definition. `toJson` is therefore safe to rely on.

`test/render_test.go` reproduces this function map so template bugs surface locally.

**There is no helm field on an `AddonConfigDefinition`.** `spec` has exactly four
keys: `addonInstallPermission`, `schema`, `templateInputResources`,
`templateOutputResources`. The Helm resemblance is only the sprig function set inside
`template` — there is no chart engine, no multi-document output, no `_helpers.tpl` and
no `.Files`, so it does nothing to relax the static-GVK constraint in Finding 2.

`AddonRelease.spec.kubernetesVersionConstraints` is a plain semver range string. Live
values: `>=1.35.0 <1.36.0`, `=1.35.5+vmware.1-vkr.1`, `>=1.32.0`.

## Finding 7 — RBAC, and where `addonInstallPermission` actually applies

The first draft flagged "creating a `ClusterRoleBinding` to `cluster-admin` in the
guest" as the **most likely first-run failure**, on the theory that Kubernetes
escalation-prevention would block the applier. **It is not a risk.** The shipped
`vks-static-v1.34.8---vmware.1-vkr.1-def` emits exactly that shape — a
`rbac.authorization.k8s.io/v1/ClusterRoleBinding` target output binding
`vks:apiserver-kubelet-client` to `system:kubelet-api-admin`. Prior art exists in the
product.

Separately, `spec.addonInstallPermission.accessPolicies` grants RBAC in the
**Supervisor** namespace — for reading `templateInputResources` and writing a
`supervisorNamespaceOutput` — not in the guest cluster. `vks-static` declares none at
all yet writes cluster-scoped guest resources. This definition declares only
`get`/`list`/`watch` on `configmaps`, for the optional payload ConfigMap.

## Finding 8 — how a packaged addon's definition actually gets created

Since no human can create an `AddonConfigDefinition`, it is worth recording how the
shipped ones arrive. A Carvel `Package` carries **the entire definition as a
gzip+base64 annotation**, `addons.kubernetes.vmware.com/addon-config-definition`,
alongside `addon.kubernetes.vmware.com/addon-name` and
`addons.kubernetes.vmware.com/upgrade-strategy`. Decoding cilium's yields its
definition verbatim. The controller materialises `Addon` + `AddonRelease` +
`AddonConfigDefinition` from it.

Recorded because it explains how the shipped addons work and because it is the obvious
thing a reviewer will propose. It is **not** a route this project takes: a
`Package`-derived `AddonRelease` gets `spec.package` set (confirmed on cilium's),
forcing a real Carvel package and a guest-side `PackageInstall`, and it would mean
publishing and maintaining an addon repository *in addition to* the Supervisor
Service. See [Alternatives rejected](#alternatives-rejected).

---

## Chosen design: kapp-controller `App` relay

Because GVK is structurally static (Finding 2), the ACD emits a small fixed set of
known-GVK resources into the guest cluster, the last of which is an `App` carrying
the user's arbitrary YAML inline. kapp-controller then applies whatever is inside.

None of them set `referenceType`. An earlier draft insisted on `Direct` everywhere,
believing the `ValuesRef` default would feed a `PackageInstall` we do not have. That
was wrong: the field is ignored for any kind other than `Secret` (Finding 2), and
every shipped non-`Secret` target output is applied directly regardless.

| # | GVK | Scope | Purpose |
|---|-----|-------|---------|
| 1 | `v1/Namespace` | Cluster | Holds the App and its SA |
| 2 | `v1/ServiceAccount` | Namespace | Identity kapp-controller applies with |
| 3 | `rbac.authorization.k8s.io/v1/ClusterRoleBinding` | Cluster | Binds SA to a configurable ClusterRole |
| 4 | `kappctrl.k14s.io/v1alpha1/App` | Namespace | Carries payload, does the applying |

All four are named `vks-bootstrap`. Names *could* be templated (Finding 2), but there
is nothing per-cluster to encode: each cluster gets its own copy of these resources
inside its own cluster. Keeping them literal also avoids interleaving ytt substitution
with the Go templates the addon controller evaluates later.

What this buys: any GVK, any resource count, pruning and GC on change, deletion on
removal, retry/backoff, and a real status object to debug against.

---

## Alternatives rejected

**Fixed set of output kinds.** Declare outputs for exactly the kinds we care about
(Namespace, Secret, Application, ConfigMap). Fully native, no kapp-controller, no
extra in-cluster object. Rejected because any unanticipated kind requires a platform
admin to version the ACD and reinstall the service — putting the admin back in the
loop and **destroying the delegation value proposition**. Rejected on the premise,
not on taste.

**`renderForEach` over label-selected ConfigMaps.** Iterates inputs with a `multiple`
constraint, but still cannot vary output GVK. Dead end for generic YAML.

**CAPI `ClusterResourceSet`.** Would have meant zero guest-cluster footprint and no
kapp-controller dependency. Ruled out by the user.

**VKS pre-created objects.** VKS picks up certain objects pre-created in the vSphere
namespace during provisioning (`kapp-controller-package` is the documented example).
Confirmed by the user to be scoped to specific CRs the TKR controller recognises; it
does not accept arbitrary YAML, so it is not a substitute.

**Ship the three CRs as plain YAML, no bundle.** The shipped `carvel-repo`, `depot`
and `helm-repo` triples have no package *and no `ownerReferences` to any Carvel
`Package`* — they look like plain CRs, and the Supervisor already runs an Argo CD
instance that manages every `AddonConfig` in the tenant namespaces. Delivering the
addon the same way would take the dependency count to literal zero. **Rejected: it is
not possible.** As established at the top of this document, nobody can create those
three kinds by hand, administrator included. Only the VKS controllers can. This was
the most attractive-looking alternative and it is simply closed.

**`AddonRepository` pointing at a Helm repository.** `AddonRepository.spec.fetch`
accepts `helmRepository: {url}` as well as `imgpkgBundle`. A live example points at
`https://charts.external-secrets.io`, and the controller auto-generates the `Addon`,
`AddonRelease` (with `dependsOn: helm-controller`) and `AddonConfigDefinition` from
the chart with no authoring at all. Genuinely useful — "install any Helm chart as a
VKS addon" is a solved problem. **Rejected here:** it needs a chart, which
reintroduces exactly the per-payload artifact this project exists to eliminate.

**`AddonRepository` pointing at an imgpkg bundle.** The other route an administrator
is permitted to drive (see Finding 8). **Rejected, and not held in reserve.** Two
independent reasons:

1. A `Package`-derived `AddonRelease` carries `spec.package` (confirmed on cilium's),
   which forces a Carvel package and a guest-side `PackageInstall`. The package-free
   property is the whole premise.
2. It adds a second external artifact — a published addon repository that has to be
   built, hosted, versioned and relocated for air-gap, *alongside* the Supervisor
   Service. **The Supervisor Service is meant to be the only external artifact this
   project requires.** That constraint is the design, not a convenience.

If the Supervisor Service's deployer account cannot write to `vmware-system-vks-public`,
the answer is to ship the required RBAC inside the service (see `verify.md` step 2),
not to add a delivery mechanism.

---

## Honest dependency list

The pitch is **"provisioning-time seeding with no per-payload artifacts,"** not "zero
dependencies." The weaker claim will not survive review. What remains:

1. **One imgpkg bundle** — the Supervisor Service itself. The OCI dependency moves
   from *per-payload* to *once, ever*. A large reduction, not zero. There is no
   route that avoids it: the "plain CRs" alternative is closed by RBAC.
2. **kapp-controller** — in-guest, pre-existing, VMware-managed, and now confirmed
   present, but its presence is a VKS implementation detail rather than a contract.
   The design dodges Carvel *packaging* by leaning on the Carvel *runtime*.
3. **An undocumented template dialect** — now mapped (Finding 5) and covered by
   tests, but still addon-controller internals that may shift across VKS versions.
   The tests encode our understanding of it, not a contract.

What genuinely is dependency-free: no package to author/version/publish per payload;
no container images, so the `ImagesLock` is empty and air-gap relocation is trivial;
no `AddonRepository` wiring.

---

## Sources

- [VKS API reference](https://developer.broadcom.com/xapis/vmware-vsphere-kubernetes-service/latest/api-docs.html)
- [Managing Add-ons in VKS Clusters](https://techdocs.broadcom.com/us/en/vmware-cis/vcf/vcf-service-administration-and-development/9-0/managing-vsphere-kuberenetes-service-clusters-and-workloads/managing-add-ons-in-vks-clusters.html)
- [Configure an Add-on at Installation Time](https://techdocs.broadcom.com/us/en/vmware-cis/vcf/vcf-service-administration-and-development/9-0/managing-vsphere-kuberenetes-service-clusters-and-workloads/managing-add-ons-in-vks-clusters/configure-an-add-on-at-installation-time.html)
- [kapp-controller App CR spec](https://carvel.dev/kapp-controller/docs/v0.50.x/app-spec/)
- [vsphere-tmm/Supervisor-Services](https://vsphere-tmm.github.io/Supervisor-Services/) — real service definition YAML
- [Leveraging Cilium CNI on vSphere VKS Clusters](https://medium.com/@bob-bauer/leveraging-cilium-cni-on-vsphere-vks-clusters-9070ab70c309) — real AddonInstall/AddonConfig examples
