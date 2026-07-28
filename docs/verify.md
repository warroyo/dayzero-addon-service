# Verification runbook

Read [`design.md`](./design.md) first for the API constraints this depends on.

## There is no fast loop

`Addon`, `AddonRelease` and `AddonConfigDefinition` come only from a Supervisor Service
or from an `AddonRepository` reconcile. No administrator can apply them, in any
namespace:

```console
$ kubectl auth can-i create addonconfigdefinitions.addons.kubernetes.vmware.com --all-namespaces
no
$ kubectl apply -f addon.yml
Error from server (Forbidden): addons.addons.kubernetes.vmware.com is forbidden:
User "sso:Administrator@gpu.local" cannot create resource "addons" in API group
"addons.kubernetes.vmware.com" in the namespace "vmware-system-vks-public"
```

The only way onto a Supervisor is to build the service and install it, which makes
step 2 below the gating check rather than a late risk.

### What can be checked without one

`make test` covers more than it first appears. It validates the rendered resources
against the real CRD schemas, extracted from a live Supervisor into `test/schemas/`.
That is the closest available substitute for a server-side dry run, and it is stricter
than the API server: unknown fields are rejected rather than silently pruned, so a
misspelled field name fails the build instead of producing a quietly wrong definition.
It also renders the templates using the same function map the addon controller exposes
(sprig plus the Helm-style `toYaml`), across every combination of payload sources, and
checks the cross-references between the three resources, which are assembled from ytt
values in separate files and would otherwise only break at install time.

Neither dry-run mode is worth reaching for:

| Approach | Result |
|---|---|
| `kubectl apply --dry-run=server` | `Forbidden`, the same RBAC wall |
| `kubectl apply --dry-run=client --validate=true` | Validates nothing. Accepts both `scope: NotARealScope` and entirely invented fields |

`TestSchemaValidationHasTeeth` guards against the second row by corrupting the rendered
definition three ways and failing if validation lets any through.

What none of this can tell you is whether the addon controller *accepts and acts on*
the definition, or whether the service can be installed at all. That needs steps 1 and
2.

---

## Step 1: build and install

```sh
make render                    # inspect the addon CRs
make test                      # render the Go templates, assert they hold up
VERSION=0.1.0 make release     # -> bootstrap-addon.yml
```

Confirm `bootstrap-addon.yml` is two documents (`PackageMetadata` and `Package`) and
that `fetch.imgpkgBundle.image` carries a resolved `@sha256:` digest.

vCenter → **Workload Management → Services → Add Service** → upload it → install.

## Step 2: did the definition land? (the gating question)

```sh
kubectl get addon,addonrelease,acd -A | grep bootstrap
```

All three should be present, in the service's own namespace
(`svc-bootstrap-addon-<id>`). That namespace is not a choice: the deploy rewrites every
namespaced resource in the package into it. Note it down, because every tenant
`AddonInstall` has to name it in `spec.addonRef.namespace`.

If they are absent, read the reconcile error:

```sh
kubectl -n vmware-system-supervisor-services get pkgi \
  svc-bootstrap-addon.fling.vsphere.vmware.com \
  -o jsonpath='{.status.usefulErrorMessage}'
```

**The open question is whether `Addon` is accepted outside
`vmware-system-vks-public`.** `AddonConfigDefinition` and `AddonRelease` are, which is
established. For `Addon`, the one rejection so far read:

```
- metadata.namespace: Unsupported value: "svc-bootstrap-addon-svr31":
  supported values: "vmware-system-vks-public"
- metadata.labels[addon.kubernetes.vmware.com/addon-name]: must be set to match "bootstrap"
```

The label is now set, so this install separates the two. If the namespace complaint is
gone, the label was the whole problem. If it comes back on its own, `Addon` is confined
to the public catalog, and since a Supervisor Service cannot write there, this delivery
route is closed and the design needs rethinking rather than patching.

A `Forbidden` instead of an admission denial would mean the deployer service account
lacks rights in its own namespace, which would be surprising.

If the `AddonRelease` is rejected at admission instead, check the error against
*Package-free addons are legal* in `design.md`. Three shipped releases are
package-free, so this would be surprising.

## Step 3: attach it to a cluster

Everything from here is admin-creatable, so iteration is cheap again.

Apply, into the cluster's Supervisor namespace:

1. [`examples/addonconfig-structured.yml`](../examples/addonconfig-structured.yml),
   renamed to `<cluster>-bootstrap`, starting with a trivial payload. One `ConfigMap`
   in `default` is enough to prove the path.
2. [`examples/addoninstall.yml`](../examples/addoninstall.yml), with
   `spec.addonRef.namespace` set to the namespace from step 2, then label the cluster.

This is also where cross-namespace resolution gets proven. No shipped addon lives
outside `vmware-system-vks-public`, so the controller honouring `addonRef.namespace` is
supported by the API shape but unexercised. If the `ClusterAddon` never appears, check
the `AddonInstall`'s status and confirm the namespace matches before looking anywhere
else.

## Step 4: watch reconciliation on the Supervisor

```sh
kubectl -n <cluster-ns> get clusteraddon
kubectl -n <cluster-ns> get clusteraddon <cluster>-bootstrap -o yaml
```

Template rendering failures surface here, in the conditions. This is the primary
debugging surface for the definition.

## Step 5: confirm in the guest cluster

```sh
kubectl get ns vks-bootstrap
kubectl -n vks-bootstrap get app vks-bootstrap -o yaml
```

`status.friendlyDescription` gives a one-line verdict; `status.deploy.stdout` has the
full kapp apply log. Then confirm the payload itself landed:

```sh
kubectl -n default get configmap <payload>
```

## Step 6: the three payload sources

Each independently, then combined, mirroring what `make test` covers locally:
`values.resources`, `values.resourcesYaml`, and a `<cluster>-bootstrap` ConfigMap.
The ConfigMap input is optional, so confirm an `AddonConfig` with `values: {}` and no
ConfigMap still reconciles rather than blocking.

## Step 7: lifecycle

- Edit the payload. kapp-controller reconciles and prunes resources removed from it.
- Set `noopDelete: true`. Removing the addon then leaves the payload behind.
- Delete the `AddonInstall`, which cleans up, or retains under
  `stopMatchingBehavior: Retain`.
- Confirm the `AddonConfig` is garbage-collected with the `ClusterAddon`. This is what
  the `owned-for-deletion` annotation buys, and it is easy to get wrong.

## Step 8: the real test

Create a brand-new cluster with the `AddonInstall` label already in place, using
[`examples/platform-baseline/`](../examples/platform-baseline/) as the payload.
Confirm the namespaces, service accounts, RBAC and quotas are present with no manual
step, and that they landed early enough to be useful, before anyone would have had to
be handed a kubeconfig.

Seeding an existing cluster proves the mechanism works. Only this proves the tenant
never needs workload-cluster credentials at all.

---

## Troubleshooting

| Symptom | Look at |
|---|---|
| Addon CRs never appear after install | `.status.usefulErrorMessage` on the `pkgi`, step 2 |
| Addon CRs denied by admission | `addon.validating.vmware.com` enforces the labels, and that an `AddonRelease` shares a namespace with its `Addon` and its definition. See design.md, *Naming and placement conventions* |
| Addon CRs land in the wrong namespace | They do not. The deploy rewrites the namespace of every resource in the package to the service's own |
| `AddonRelease` rejected at admission | Missing `spec.package`. See design.md, *Package-free addons are legal* |
| ClusterAddon stuck, template error | `kubectl -n <cluster-ns> get clusteraddon -o yaml`, then read the conditions |
| Template renders but references wrong data | Context roots are `.Values`, `.Dependencies`, `.Cluster`, `.Addon`. See design.md, *The template dialect*. Lowercase `.values` silently yields nothing |
| Nothing appears in guest cluster | Not `referenceType`, which is ignored for non-Secrets. Check the ClusterAddon conditions instead |
| App exists but payload not applied | `kubectl -n vks-bootstrap get app vks-bootstrap -o yaml`, then `status.deploy.stdout` |
| App fails with no inline paths | Every payload source was empty. The placeholder path should prevent this; see `TestNoPayloadStillRenders` |
| ytt errors on user payload | `ignoreUnknownComments: true` is set, but ytt still parses `#@` as directives. Payloads containing `#@` need escaping |
