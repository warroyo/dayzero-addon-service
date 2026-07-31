# Verification runbook

Read [`design.md`](./design.md) first for the API constraints this depends on.

## What is already proven

Two parts of the chain are validated without a full install, and should stay green.

**The render round trip, locally.** `make test` renders the ACD's Go templates and the
package's ytt and asserts a payload encoded into the values Secret comes back out as the
resources it went in as, across structured, raw-string and ConfigMap sources. It also
validates the ACD against the real CRD schema and pins the namespace and labels the
validating webhook requires.

**The guest-side mechanism, in a live cluster.** A `PackageInstall` pointing at an inline
package with this project's exact render turned per-cluster values into the expected
resources, with vendir fetching nothing from a registry, and deleting the PackageInstall
removed what it applied. So once the manager creates the addon, the guest half works.

## The one unproven step

Whether a hand-authored `AddonRepository` makes the manager materialise our ACD and sync
an inline-content package to the guest. This cannot be tested without a bundle the
Supervisor can pull, because that is what an `AddonRepository` fetches. Everything downstream
of it is verified above; this runbook exists to close it.

## Step 1: build and publish the catalog

```sh
make bundle                     # assemble build/bundle, inspect it
make test                       # render templates, validate, assert the round trip
make check                      # offerings annotation vs bundle contents
make push REPO_VERSION=1.1.0    # publish to the registry
```

`make bundle` stages the tree `kctrl package repository release` expects: every version in
`PKG_VERSIONS` under `build/bundle/packages/<pkg>/`, plus `metadata.yml` and the stamped
`pkgrepo-build.yml`. Confirm each Package's `addon-config-definition` annotation decodes
back to an ACD named for that version — `make render RENDER_VERSION=<v>` does both.

`make push` hands that to kctrl, which runs kbld to generate `.imgpkg/images.yml` and
pushes. Pull it back to confirm what actually landed in the registry:

```sh
imgpkg pull -b ghcr.io/warroyo/dayzero-addon-repo:1.1.0 -o /tmp/check && find /tmp/check -type f
```

Expect `packages/<pkg>/` with `metadata.yml` and one file per version, and an
`.imgpkg/images.yml` listing no images — the packages fetch inline, so there are no
container images to mirror for air-gap. The staging-only `pkgrepo-build.yml` should not be
in there.

## Step 2: install the AddonRepository

Either method from the README. Point `imageURL` at the catalog from step 1.

```sh
make install-manifest
kubectl apply -f build/install/addonrepository.yml
```

The resources are named for the catalog version (`dayzero-addon-repo-1-1-0`), so this
registers alongside any earlier catalog rather than replacing it. That is deliberate: an
`AddonRepository` cannot be updated once installed. Delete the superseded pair after
pins have moved.

## Step 3: did the manager materialise the addon? (the gating question)

```sh
kubectl -n vmware-system-vks-public get addon,addonrelease,acd | grep dayzero
```

All three kinds should appear, with one `AddonRelease` and one `AddonConfigDefinition` per
package version in the catalog. If they do not, read the repository install status:

```sh
kubectl -n vmware-system-vks-public get addonrepositoryinstall \
  dayzero-addon-repo-1-1-0-install -o jsonpath='{.status.usefulErrorMessage}'
```

A fetch error means the bundle is unreachable or the `imageURL` is wrong. A validation
error names the field: the most likely is a `package-offerings` annotation that does not
match the packages actually in the bundle. `make check` catches that before publishing.
If the `AddonRelease` is present but rejected, check its status against the shape in
`design.md`.

Confirm each ACD came through intact:

```sh
kubectl -n vmware-system-vks-public get acd dayzero.kubernetes.vmware.com.1.0.2 \
  -o jsonpath='{.spec.schema.openAPIV3Schema.properties}'
```

It should show `resources` and `resourcesYaml`, our schema, not a generated one. Repeat for
each version: they are separate definitions, and a frozen older version should still carry
the schema it was published with.

## Step 4: attach it to a cluster

Into the cluster's Supervisor namespace, apply:

1. [`examples/addonconfig-structured.yml`](../examples/addonconfig-structured.yml),
   renamed to `<cluster>-dayzero`, starting with a trivial payload (one ConfigMap in
   `default` is enough).
2. [`examples/addoninstall.yml`](../examples/addoninstall.yml), then label the cluster.

If the `ClusterAddon` never appears, check the `AddonInstall` status first.

## Step 5: watch reconciliation on the Supervisor

```sh
kubectl -n <cluster-ns> get clusteraddon <cluster>-dayzero -o yaml
```

Template rendering failures surface here, in the conditions. This is the primary debugging
surface for the ACD. Confirm the values Secret was produced:

```sh
kubectl -n <cluster-ns> get secret <cluster>-dayzero-data-values -o jsonpath='{.data.values\.yaml}' | base64 -d
```

It should carry `resourcesJson` and `resourcesYaml`.

## Step 6: confirm in the guest cluster

```sh
kubectl -n vmware-system-tkg get pkgi <cluster>-dayzero -o yaml   # status.usefulErrorMessage on failure
kubectl -n default get configmap <payload>
```

The `PackageInstall` should reconcile, and the payload resources should be present.

## Step 7: the payload sources and lifecycle

- Each source alone and combined: `values.resources`, `values.resourcesYaml`, and a
  `<cluster>-dayzero` ConfigMap. An `AddonConfig` with `values: {}` and no ConfigMap
  should reconcile to an empty payload, not error.
- Edit the payload: the `PackageInstall` re-reconciles and prunes resources removed from it.
- Delete the `AddonInstall`: the payload is cleaned up, or retained under
  `stopMatchingBehavior: Retain`.
- Confirm the `AddonConfig` is garbage collected with the `ClusterAddon`, which the
  `owned-for-deletion` annotation buys.

## Step 8: the real test

Create a brand-new cluster with the `AddonInstall` label already in place, using
[`examples/platform-baseline/`](../examples/platform-baseline/) as the payload. Confirm the
namespaces, service accounts, RBAC and quotas are present with no manual step, and early
enough to be useful. Seeding an existing cluster proves the mechanism; only a fresh cluster
proves the tenant never needs a kubeconfig.

## Troubleshooting

| Symptom | Look at |
|---|---|
| Addon CRs never appear | `AddonRepositoryInstall.status.usefulErrorMessage`, step 3. Fetch error or `package-offerings` mismatch |
| An edit to an AddonRepository is rejected as "in use by an AddonRepositoryInstall" | Expected. It is frozen once referenced; ship a new catalog release and delete the old pair. See the README |
| Only some package versions materialised | `package-offerings` and the bundle disagree, or a version is listed in `PKG_VERSIONS` with no file in `released/`. Run `make check` |
| ACD present but schema is `helmValues`/`helmOptions` | A helm AddonRepository was installed by mistake; this project ships an imgpkg one |
| ClusterAddon stuck, template error | `kubectl -n <cluster-ns> get clusteraddon -o yaml`, then the conditions |
| values Secret is empty or malformed | The ACD output template. Context roots are `.Values`, `.Dependencies`, `.Cluster`, `.Addon` (capital V) |
| PackageInstall fails to render | The package ytt got malformed values. Decode the values Secret (step 5) and check `resourcesJson` is valid JSON |
| Nothing applied, no error | An empty payload renders to zero resources by design; confirm the AddonConfig actually carries a payload |
