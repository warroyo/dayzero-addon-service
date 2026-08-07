# Verification runbook

Build and layout are in [`CONTRIBUTING.md`](../CONTRIBUTING.md).

## What the tests already cover

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

**The full chain, on a real Supervisor.** A published catalog registered through an
`AddonRepository` materialised the `Addon`, `AddonRelease` and ACD, and a tenant
`AddonInstall` + `AddonConfig` seeded a workload cluster. The steps below are the
procedure for re-running that after a change, not an open question.

## Step 1: build and publish the catalog

```sh
make bundle                     # assemble build/bundle, inspect it
make test                       # render templates, validate, assert the round trip
make check                      # bundle vs PKG_VERSIONS, manifest is release-independent
make push REPO_VERSION=1.1.0    # publish: snapshot tag, then move :stable onto it
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

The resources carry no version (`dayzero-addon-repo-stable`) and point at the floating
tag, so this is a one-time step: later catalogs arrive through the tag. Re-applying the
same file is a no-op. An `AddonRepository` cannot be updated once installed, so if you
need a *different* registration — a snapshot pin, say — give it its own name and its own
`targetRepositoryName` rather than editing this one.

## Step 3: did the manager materialise the addon? (the gating question)

```sh
kubectl -n vmware-system-vks-public get addon,addonrelease,acd | grep dayzero
```

All three kinds should appear, with one `AddonRelease` and one `AddonConfigDefinition` per
package version in the catalog. If they do not, read the repository install status:

```sh
kubectl -n vmware-system-vks-public get addonrepositoryinstall \
  dayzero-addon-repo-stable-install -o jsonpath='{.status.usefulErrorMessage}'
```

A fetch error means the bundle is unreachable or the `imageURL` is wrong — for a private
registry, that the Supervisor cannot pull it. The `package-offerings` annotation is not a
suspect: it is declarative and never checked against the bundle. If the `AddonRelease` is
present but rejected, read its status conditions.

After a release, confirm the moved tag landed rather than assuming it: the new
`AddonRelease` appears without any CR changing, roughly ten minutes after the push.

```sh
kubectl -n vmware-system-vks-public get packagerepository dayzero-addon-repo-stable \
  -o jsonpath='{.status.fetch.stdout}' | grep -o 'sha256:[a-f0-9]*'
kubectl -n vmware-system-vks-public get addonrelease | grep dayzero
```

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
surface for the ACD.

The values Secret itself is delivered into the guest, not the Supervisor namespace, so
decode it there — see step 6. Before 1.0.3 a second copy was also written alongside the
`ClusterAddon`; if you are checking an older version, that is what
`kubectl -n <cluster-ns> get secret <cluster>-dayzero-data-values` returns.

## Step 6: confirm in the guest cluster

```sh
kubectl -n vmware-system-tkg get pkgi <cluster>-dayzero -o yaml   # status.usefulErrorMessage on failure
kubectl -n vmware-system-tkg get secret <cluster>-dayzero-data-values \
  -o jsonpath='{.data.values\.yaml}' | base64 -d                  # should carry resourcesJson + resourcesYaml
kubectl -n default get configmap <payload>
```

The `PackageInstall` should reconcile, and the payload resources should be present.

Its `spec.values` should list the Secret exactly **once**. Two entries naming the same
Secret means a `supervisorNamespaceOutput` has come back:

```sh
kubectl -n vmware-system-tkg get pkgi <cluster>-dayzero -o jsonpath='{.spec.values}'
```

## Step 7: the payload sources and lifecycle

- Each source alone and combined: `values.resources`, `values.resourcesYaml`, and a
  `<cluster>-dayzero` ConfigMap. An `AddonConfig` with `values: {}` and no ConfigMap
  should reconcile to an empty payload, not error.
- Edit the payload: the `PackageInstall` re-reconciles and prunes resources removed from it.
- Delete the `AddonInstall`: the payload is cleaned up, or retained under
  `stopMatchingBehavior: Retain`.
- Confirm the `AddonConfig` is garbage collected with the `ClusterAddon`, which the
  `owned-for-deletion` annotation buys.

## Step 7a: cluster identity tokens (1.0.3 and later)

Pin `releaseFilter.ref.name` to the version under test, then work through all four cases —
the last two are the ones that fail quietly if the ACD template is wrong.

1. **Inline.** A payload with `${CLUSTER_NAME}-${CLUSTER_UID}` in `values.resourcesYaml`
   (see [`examples/addonconfig-jwtauthenticator.yml`](../examples/addonconfig-jwtauthenticator.yml)).
   Confirm the object lands in the guest with both expanded, cross-checked against the
   cluster's real UID:
   ```sh
   kubectl -n <cluster-ns> get cluster <cluster> -o jsonpath='{.metadata.uid}'
   ```
2. **Structured.** The same tokens inside `values.resources`, which travels through the
   JSON encoding rather than the raw string.
3. **From the ConfigMap.** Put a token in a `<cluster>-dayzero` ConfigMap value. This is
   the case that breaks if substitution runs before the ConfigMap is concatenated, and it
   cannot be caught by any inline payload.
4. **No ConfigMap at all, no tokens.** The plain case, which is what every payload written
   before 1.0.3 looks like. It exercises the second `templateInputResource` being present
   while the optional one is absent — the shape that a bare `.Dependencies.dayzeroConfigMap`
   guard would fail on.

Then the refusal path: a payload using `${CLUSTER_UID}` on a cluster whose `Cluster` object
cannot be resolved must fail to render rather than expand the token to empty. The error
appears in the `ClusterAddon` conditions and names the cause:

```sh
kubectl -n <cluster-ns> get clusteraddon <cluster>-dayzero -o yaml
```

Regression, worth doing once per release: apply a token-free payload and confirm the guest
output is unchanged from the previous version. This should hold by construction — the
inlined render files are byte-identical across 1.0.2 and 1.0.3 — but confirm it rather than
assume it:

```sh
python3 - <<'EOF'
import yaml
f=lambda p: yaml.safe_load(open(p))['spec']['template']['spec']['fetch'][0]['inline']['paths']
print(f('released/dayzero-addon-repo/1.0.2.yml') == f('released/dayzero-addon-repo/1.0.3.yml'))
EOF
```

## Step 8: the real test

Create a brand-new cluster with the `AddonInstall` label already in place, using
[`examples/platform-baseline/`](../examples/platform-baseline/) as the payload. Confirm the
namespaces, service accounts, RBAC and quotas are present with no manual step, and early
enough to be useful. Seeding an existing cluster proves the mechanism; only a fresh cluster
proves the tenant never needs a kubeconfig.

## Troubleshooting

| Symptom | Look at |
|---|---|
| Addon CRs never appear | `AddonRepositoryInstall.status.usefulErrorMessage`, step 3. Almost always a fetch error: unreachable bundle or a wrong `imageURL` |
| An edit to an AddonRepository is rejected as "in use by an AddonRepositoryInstall" | Expected, and nothing here should be editing one. New package versions arrive by moving the tag, not by changing the CR. See the README |
| A new package version never shows up | The tag did not move, or the manager has not re-resolved yet (allow ~10 min). Compare the digest in the `PackageRepository`'s `status.fetch.stdout` against what `:stable` points at |
| Only some package versions materialised | A version is listed in `PKG_VERSIONS` with no file in `released/`, or the bundle was pushed before `make bundle` re-staged. Run `make check` |
| ACD present but schema is `helmValues`/`helmOptions` | A helm AddonRepository was installed by mistake; this project ships an imgpkg one |
| ClusterAddon stuck, template error | `kubectl -n <cluster-ns> get clusteraddon -o yaml`, then the conditions |
| values Secret is empty or malformed | The ACD output template. Context roots are `.Values`, `.Dependencies`, `.Cluster`, `.Addon` (capital V) |
| PackageInstall fails to render | The package ytt got malformed values. Decode the values Secret in the guest (step 6) and check `resourcesJson` is valid JSON |
| Nothing applied, no error | An empty payload renders to zero resources by design; confirm the AddonConfig actually carries a payload |
| ClusterAddon condition says "payload uses ${CLUSTER_UID} but the cluster UID did not resolve" | The `clusterCR` input did not resolve. Working as intended — it refuses rather than shipping an empty UID. Check the `Cluster` object exists in the Supervisor namespace and that the addon is 1.0.3 or later |
| A `${CLUSTER_...}` token reached the guest unexpanded | Either the addon is older than 1.0.3, or the spelling is off. Only `${CLUSTER_NAME}` and `${CLUSTER_UID}` expand; near-miss forms are passed through deliberately |
