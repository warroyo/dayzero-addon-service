# Design & API findings

Research notes backing `docs/plan.md`. Everything here was established against the
Broadcom VKS API reference and Carvel docs; each finding says how confident we are
and what is still unverified. **Read this before touching `bundle/config/`.**

## Why this exists

Only platform admins can create `AddonConfigDefinition` resources in
`vmware-system-vks-public`. Regular users can create `AddonInstall` and `AddonConfig`
in their own vSphere namespace.

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
either, because we create `Addon` + `AddonRelease` directly.

**Confidence: high.** Verbatim from the API reference.

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

**This is why arbitrary GVKs are impossible natively, and why the relay below exists.**

**Confidence: high** on GVK being static (the TypeMeta exclusion is decisive).
**Lower** on whether `name` accepts templating — the docs are silent, and
`renderForEach` generating up to 64 outputs implies *something* differentiates their
names. Treat names as static until proven otherwise.

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

kapp-controller is present in VKS guest clusters as a system/bootstrap package
(VKS ≥ 3.5, "automatically installed, does not need migration").

**Confidence: high.**

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

## Chosen design: kapp-controller `App` relay

Because GVK is structurally static (Finding 2), the ACD emits a small fixed set of
known-GVK resources into the guest cluster, the last of which is an `App` carrying
the user's arbitrary YAML inline. kapp-controller then applies whatever is inside.

All four use `referenceType: Direct`. **This matters**: the default for
`targetClusterOutput` is `ValuesRef`, which feeds a `PackageInstall` we deliberately
do not have.

| # | GVK | Scope | Purpose |
|---|-----|-------|---------|
| 1 | `v1/Namespace` | Cluster | Holds the App and its SA |
| 2 | `v1/ServiceAccount` | Namespace | Identity kapp-controller applies with |
| 3 | `rbac.authorization.k8s.io/v1/ClusterRoleBinding` | Cluster | Binds SA to a configurable ClusterRole |
| 4 | `kappctrl.k14s.io/v1alpha1/App` | Namespace | Carries payload, does the applying |

Names are static (`vks-bootstrap`).

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

---

## Honest dependency list

The pitch is **"provisioning-time seeding with no per-payload artifacts,"** not "zero
dependencies." The weaker claim will not survive review. What remains:

1. **One imgpkg bundle** — the Supervisor Service itself. The OCI dependency moves
   from *per-payload* to *once, ever*. A large reduction, not zero.
2. **kapp-controller** — in-guest, pre-existing, VMware-managed, but its presence is
   a VKS implementation detail rather than a contract. The design dodges Carvel
   *packaging* by leaning on the Carvel *runtime*.
3. **An undocumented ACD template dialect** — couples us to addon-controller
   internals that may shift across VKS versions.

What genuinely is dependency-free: no package to author/version/publish per payload;
no container images, so the `ImagesLock` is empty and air-gap relocation is trivial;
no `AddonRepository` wiring.

---

## Still unverified — resolve before authoring `bundle/config/`

1. The Go template **context root**: `.values.*`, `.inputs.<name>.*`, `.cluster.*`?
2. Whether **sprig** is registered — `toJson` is load-bearing in the plan's App template.
3. Whether `targetClusterOutput.name` accepts templating.
4. What an empty required `template` body accepts for `Namespace` / `ServiceAccount`.
5. The exact shape of `AddonRelease.spec.kubernetesVersionConstraints`.
6. What identity the addon system applies `Direct` outputs with — RBAC
   escalation-prevention means creating a `ClusterRoleBinding` to `cluster-admin`
   requires the applier to hold `cluster-admin` or the `escalate` verb. **Most likely
   first-run failure.**

See `docs/verify.md` Step 0 for the commands that answer 1–5.

---

## Sources

- [VKS API reference](https://developer.broadcom.com/xapis/vmware-vsphere-kubernetes-service/latest/api-docs.html)
- [Managing Add-ons in VKS Clusters](https://techdocs.broadcom.com/us/en/vmware-cis/vcf/vcf-service-administration-and-development/9-0/managing-vsphere-kuberenetes-service-clusters-and-workloads/managing-add-ons-in-vks-clusters.html)
- [Configure an Add-on at Installation Time](https://techdocs.broadcom.com/us/en/vmware-cis/vcf/vcf-service-administration-and-development/9-0/managing-vsphere-kuberenetes-service-clusters-and-workloads/managing-add-ons-in-vks-clusters/configure-an-add-on-at-installation-time.html)
- [kapp-controller App CR spec](https://carvel.dev/kapp-controller/docs/v0.50.x/app-spec/)
- [vsphere-tmm/Supervisor-Services](https://vsphere-tmm.github.io/Supervisor-Services/) — real service definition YAML
- [Leveraging Cilium CNI on vSphere VKS Clusters](https://medium.com/@bob-bauer/leveraging-cilium-cni-on-vsphere-vks-clusters-9070ab70c309) — real AddonInstall/AddonConfig examples
