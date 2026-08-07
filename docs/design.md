# Design

Why this addon is shaped the way it is. Read it before touching `addon/`; the build
mechanics are in [`CONTRIBUTING.md`](../CONTRIBUTING.md).

This is the *project's* design record. The VKS addon system's own rules — the resource
model, the manager-owned webhooks, how an `AddonRepository` behaves once registered — are
platform knowledge and live in the `vks-addons` skill rather than being restated here.

## Why an AddonRepository

Because it is the only route. `Addon` and `AddonRelease` are webhook-locked to the VKS
addon manager's service account, and an `AddonConfigDefinition` on its own is inert: an
`AddonInstall` reaches an ACD only through a selected `AddonRelease`, and needs the
`Addon` to reconcile at all. An administrator *can* create `AddonRepository` and
`AddonRepositoryInstall`, and the manager then materialises all three. Everything below
follows from that one fact.

An earlier design had a Supervisor Service create the three resources itself, package-free,
with an inline `App` delivering the payload and no registry in the path. It is the better
design and it is blocked entirely by those webhooks. The skill records what it was and
what would unblock it.

## The guest fetch is accepted, not fought

Going through an `AddonRepository` means `spec.package` is set, which means the guest gets
a `PackageInstall` and pulls the bundle. No arrangement avoids it. Rather than work around
it, this project makes it cheap: the packages fetch their render files `inline`, so the
bundle carries no container images at all, its generated `ImagesLock` is empty, and
relocating it for air-gap is a single `imgpkg copy` with nothing to mirror.

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
no new resources, and older versions stay available to roll back to. The mechanics, and
the frozen-YAML rule that keeps old versions honest, are in
[`CONTRIBUTING.md`](../CONTRIBUTING.md).

## How the payload reaches the guest

The tenant payload flows as data, in three hops:

1. A tenant writes resources in `AddonConfig.spec.values` (`resources`, `resourcesYaml`,
   or a `<cluster>-dayzero` ConfigMap).
2. The ACD's output template renders those into a values `Secret`. The addon controller
   wires that Secret into the guest `PackageInstall` as its values.
3. The package's ytt reads the values and emits the resources; kapp applies them.

The Secret is the standard mechanism. One output resource does it: a `targetClusterOutput`
Secret in the guest package namespace, which is where the guest `PackageInstall` resolves
`secretRef`.

Through 1.0.2 there were two outputs, copying the cilium ACD, which pairs a
`supervisorNamespaceOutput` carrying `referenceType: ValuesRef` with a plain copy in the
guest. 1.0.3 drops the Supervisor half. Surveying the shipped ACDs on a live Supervisor
showed cilium's shape is the rare one: **85 of them declare only a `targetClusterOutput`**
and their payloads are wired correctly regardless. The redundancy was visible in the guest,
where dayzero was the only addon whose `PackageInstall` listed its values Secret twice:

```
dev1-cluster-ako       values=1 ['dev1-cluster-ako-data-values']
dev1-cluster-istio     values=1 ['dev1-cluster-istio-values']
dev1-cluster-dayzero   values=2 ['dev1-cluster-dayzero-data-values',
                                 'dev1-cluster-dayzero-data-values']
```

Both entries resolved to the same guest-local Secret, so the Supervisor copy's contents
were never read — it was a debugging convenience that cost a second copy of the output
template, kept identical to the first by hand. Dropping it also made the addon's write
access to secrets in the tenant namespace unnecessary; see the RBAC section.

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

## Why the payload is encoded, not templated in place

An ACD output template is the resource body only — no `apiVersion`, `kind` or `metadata`,
one resource per entry, and the GVK is a static literal even though `name` and `namespace`
are templatable (this project renders `{{.Cluster.name}}-dayzero-data-values`). An addon
therefore *cannot* emit arbitrary payload kinds from the ACD, which is the whole problem
this addon exists to solve. The way out is to emit one fixed kind — a Secret — and let the
package's ytt turn its contents back into arbitrary resources in the guest.

`test/render_test.go` reproduces the controller's template function map (Go `text/template`
plus sprig, plus the Helm-style `toYaml`) so template bugs surface locally instead of on a
Supervisor.

## The one exception: cluster identity tokens

Since 1.0.3 the payload is not quite opaque. Two literal tokens, `${CLUSTER_NAME}` and
`${CLUSTER_UID}`, are expanded as the payload is encoded. The need is real and narrow: a
Pinniped `JWTAuthenticator` audience must contain a UID Kubernetes assigns at cluster
creation, so it cannot be authored in git, and creating it imperatively is the thing this
addon exists to avoid.

Three choices are worth recording, because each had a plausible alternative.

**Substitution happens in the ACD template, not in the guest render.** The dialect has no
`tpl` function, so a payload string cannot be re-templated — it can only be rewritten as
text before it is encoded. Doing it there rather than in the package's ytt means no new
data value crosses into the guest, so `addon/render/` is untouched and 1.0.3's inlined
render files are byte-identical to 1.0.2's. A token-free payload therefore renders
identically to the previous version by construction rather than by test.

**The `Cluster` object is a `templateInputResource`, not a context root.** `.Cluster`
carries only `name` and `namespace`, which is why all 26 shipped ACDs that need cluster
state declare the CR as a dependency instead. It is declared **required**, matching every
one of them: an unresolvable `Cluster` should stop the `ClusterAddon` rather than expand
`${CLUSTER_UID}` to empty and ship a subtly wrong payload.

**The guard is an exact-string test and nothing cleverer.** If a payload asks for a UID
that did not resolve, the template calls `fail` and the error names the cause in the
`ClusterAddon` conditions. It deliberately does not try to catch near-miss spellings like
`${CLUSTERUID}`: the ACD ships frozen inside its package version, so a heuristic that
produced a false positive would reproduce on every cluster and could only be fixed by
publishing another version. A precise check that misses typos is worth more than a fuzzy
one that cannot be corrected.

Known limits, all accepted: expansion covers tenant-supplied ConfigMap content as well as
operator-authored values; substitution into `resourcesJson` writes into a JSON string
literal, so a future token whose value could contain `"` or `\` would corrupt it (an RFC
1123 label and a UUID cannot); and there is no way to emit a literal `${CLUSTER_NAME}`.

Alternatives rejected: a typed field-path injection primitive, which would be structurally
safer but is a permanent path-interpreter for one field on one resource; a different
identity source such as the guest's `kube-system` namespace UID, which VCFA does not accept
as a relying party; and a guest-side ytt overlay carried in the payload, which cannot work
because `render.yml` `yaml.decode`s payload documents and overlay annotations are comments.

## What this ACD asks for in RBAC

`spec.addonInstallPermission.accessPolicies` declares `get`/`list`/`watch` on configmaps —
the optional payload ConfigMap — and nothing else. Those rights are in the cluster's
Supervisor namespace, not the guest.

Through 1.0.2 it also granted full access to secrets, following cilium. That existed only
to write the Supervisor-side copy of the values Secret; with that output gone in 1.0.3 the
addon writes nothing into the tenant namespace, and the grant went with it. All 85 shipped
ACDs that declare only a `targetClusterOutput` likewise grant no secret write.

The `clusterCR` input added in 1.0.3 has no matching rule, which looks like an oversight
and is not. Checked against a live Supervisor: of the 26 shipped ACDs that declare a
`Cluster` `templateInputResource`, **none** grants a `cluster.x-k8s.io` rule — cilium
grants secrets only and still dereferences `.Dependencies.ClusterCR.spec.*` on a healthy
cluster. `accessPolicies` does not gate input resolution; the controller resolves inputs
with its own identity. Granting rights the addon does not need would be the worse default.

The payload lands in the guest under the addon system's own `PackageInstall` identity,
which this project does not control and cannot scope down. That is the one knob the
rejected App-relay design had and this one gives up — see below.

## Delivery: one permanent registration on a moving tag

The platform's naming, placement and immutability rules are in the skill. Two of them
decide how this project ships, so they are worth stating as design choices rather than
constraints:

- **The registration never changes.** A registered `AddonRepository` cannot be modified at
  all, so the install manifest is written to be byte-identical across releases: no version
  in the names, a constant `spec.version`, a floating `:stable` `imageURL`, and an
  offerings annotation that names the package without listing versions. Releases deliver by
  moving the tag, which the manager re-resolves on its own.
- **The catalog is append-only.** Because the tag moves under a live registration, removing
  a package version would remove it from clusters pinned to it. `released/` freezes every
  published version for exactly this reason.

The alternative — a new repo+install pair per catalog, which is how VKS ships its own
repositories — was rejected for this project: it makes every package version an
administrator's problem, and the whole point here is that adding one is not.

`make test` pins the label and namespace requirements the CRD schemas do not cover, so a
change that would be rejected at admission fails locally instead.

## Alternatives rejected

**Direct creation by a Supervisor Service.** Have the service create the `Addon`,
`AddonRelease` and ACD itself, package-free, with an inline App delivering the payload to
the guest with no fetch. This is the ideal, and it is blocked only by the manager-only
webhooks — a platform policy that could change. The skill records the full design and the
probe that tells you whether the block still stands.

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
