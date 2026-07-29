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

## Step 1: build and publish the bundle

```sh
make bundle                 # assemble build/bundle, inspect it
make test                   # render templates, validate, assert the round trip
make push VERSION=1.0.0     # publish to the registry
```

Confirm the bundle is a package repository: `build/bundle/packages/<pkg>/{metadata.yml,1.0.0.yml}`
plus a `.imgpkg/images.yml` with an empty `images` list (no container images to mirror).
Confirm the Package's `addon-config-definition` annotation decodes back to the ACD.

## Step 2: install the AddonRepository

Either method from the README. Point `imageURL` at the bundle from step 1.

```sh
kubectl apply -f install/addonrepository.yml
```

## Step 3: did the manager materialise the addon? (the gating question)

```sh
kubectl -n vmware-system-vks-public get addon,addonrelease,acd | grep bootstrap
```

All three should appear. If they do not, read the repository install status:

```sh
kubectl -n vmware-system-vks-public get addonrepositoryinstall \
  bootstrap-addon-repo-install -o jsonpath='{.status.usefulErrorMessage}'
```

A fetch error means the bundle is unreachable or the `imageURL` is wrong. A validation
error names the field: the most likely is a `package-offerings` annotation that does not
match the packages actually in the bundle. If the `AddonRelease` is present but rejected,
check its status against the shape in `design.md`.

Confirm the ACD came through intact:

```sh
kubectl -n vmware-system-vks-public get acd bootstrap.kubernetes.vmware.com.1.0.0 \
  -o jsonpath='{.spec.schema.openAPIV3Schema.properties}'
```

It should show `resources` and `resourcesYaml`, our schema, not a generated one.

## Step 4: attach it to a cluster

Into the cluster's Supervisor namespace, apply:

1. [`examples/addonconfig-structured.yml`](../examples/addonconfig-structured.yml),
   renamed to `<cluster>-bootstrap`, starting with a trivial payload (one ConfigMap in
   `default` is enough).
2. [`examples/addoninstall.yml`](../examples/addoninstall.yml), then label the cluster.

If the `ClusterAddon` never appears, check the `AddonInstall` status first.

## Step 5: watch reconciliation on the Supervisor

```sh
kubectl -n <cluster-ns> get clusteraddon <cluster>-bootstrap -o yaml
```

Template rendering failures surface here, in the conditions. This is the primary debugging
surface for the ACD. Confirm the values Secret was produced:

```sh
kubectl -n <cluster-ns> get secret <cluster>-bootstrap-data-values -o jsonpath='{.data.values\.yaml}' | base64 -d
```

It should carry `resourcesJson` and `resourcesYaml`.

## Step 6: confirm in the guest cluster

```sh
kubectl -n vmware-system-tkg get pkgi <cluster>-bootstrap -o yaml   # status.usefulErrorMessage on failure
kubectl -n default get configmap <payload>
```

The `PackageInstall` should reconcile, and the payload resources should be present.

## Step 7: the payload sources and lifecycle

- Each source alone and combined: `values.resources`, `values.resourcesYaml`, and a
  `<cluster>-bootstrap` ConfigMap. An `AddonConfig` with `values: {}` and no ConfigMap
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
| ACD present but schema is `helmValues`/`helmOptions` | A helm AddonRepository was installed by mistake; this project ships an imgpkg one |
| ClusterAddon stuck, template error | `kubectl -n <cluster-ns> get clusteraddon -o yaml`, then the conditions |
| values Secret is empty or malformed | The ACD output template. Context roots are `.Values`, `.Dependencies`, `.Cluster`, `.Addon` (capital V) |
| PackageInstall fails to render | The package ytt got malformed values. Decode the values Secret (step 5) and check `resourcesJson` is valid JSON |
| Nothing applied, no error | An empty payload renders to zero resources by design; confirm the AddonConfig actually carries a payload |
