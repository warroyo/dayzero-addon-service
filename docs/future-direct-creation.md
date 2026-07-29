# Direct addon creation: blocked today, possibly viable later

This describes the design this project originally aimed for, why VKS blocks it today,
and the single change that would make it viable. It is kept because the block is a
platform policy choice, not a fundamental limitation, and if it changes this design is
strictly better than the `AddonRepository` one that ships.

The findings here were established against a live Supervisor (VKS 3.7) and a live guest
cluster. Read [`design.md`](./design.md) for the design that actually ships.

## What the direct design is

A Supervisor Service that lays down the three addon resources itself:

- `AddonConfigDefinition` with our schema and a small set of output resources,
- `Addon`,
- `AddonRelease` carrying only `addonConfigDefinitionRef` and no `spec.package`.

Tenants then attach it per cluster with `AddonInstall` + `AddonConfig`, exactly as they
do in the shipped design. The payload reaches the guest through the ACD's
`templateOutputResources`, which the addon controller renders on the Supervisor and
applies into the guest directly. The last output is a kapp-controller `App` with an
`inline` fetch carrying the tenant's YAML, so the guest applies the payload from data
inside the CR.

## Why it is worth wanting

Two properties the shipped `AddonRepository` design cannot have:

1. **Package-free.** The `AddonRelease` carries no `spec.package`, so no Carvel package
   and no guest `PackageInstall` are involved.
2. **Fetch-free guest.** Because the payload rides in an `inline` App, the guest pulls
   nothing from any registry, internal or external. The addon controller renders on the
   Supervisor and pushes the result in.

The shipped design gives up both: an `AddonRepository` forces a guest package, and the
guest fetches the bundle through its package repository. The direct design is the only
route that seeds a cluster with arbitrary YAML and has the guest fetch nothing.

Everything downstream of CR creation is already proven to work (see
[What already works](#what-already-works)). Only the creation step is blocked.

## Why it does not work today

`Addon` and `AddonRelease` are owned by the VKS addon manager. Two validating webhooks
reject creation and update of them from any client that is not the manager's own service
account, and RBAC does not change that.

```
admission webhook "addon.validating.vmware.com" denied the request:
  Addon is a managed resource and CREATE is not allowed

admission webhook "addonrelease.validating.vmware.com" denied the request:
  operation on an AddonRelease is only allowed from VKS addon manager service account
```

Both were reproduced with a service account that held `create` on these kinds
(`kubectl auth can-i create addonreleases → yes`). The request still failed at
admission, not at authorization, and every mutating operation was refused: create,
update, client-side apply, and server-side apply all hit the same denial.

The webhooks are served by `tanzu-addons-controller-manager` (in the `svc-tkg-*`
namespace), fronted by `tanzu-addons-manager-webhook-service:443`. The manager runs with
`--enable-webhook-client-verification`, which is the flag behind the caller-identity
check. So the gate is on **who is calling**, and the only accepted caller is the manager
itself.

This closes the whole family of "create the CRs ourselves" approaches at once. A
Supervisor Service, a Job, a long-running controller, a raw `kubectl apply` all present
as some client that is not the manager, so all are denied identically.

## What already works

The block is narrow. Everything except the two locked creations is verified working:

- **`AddonConfigDefinition` is not locked.** A non-manager service account with RBAC
  created one successfully; it is the one kind of the three we can author directly.
- **The inline App delivers the payload with no fetch.** A kapp-controller `App` with an
  `inline` fetch, created directly in a guest, applied its ConfigMap payload with
  vendir reporting `inline: {}` and no registry contacted.
- **Deletion cleans up.** Deleting that App removed the resources it had applied, so
  addon removal would garbage-collect the payload with no owner references or finalizers
  of our own.
- **The Supervisor Service namespace is not a hard block.** A Supervisor Service package
  has its namespaced resources rewritten into the service's own namespace, but the addon
  manager reconciles resources there too (an `AddonRepositoryInstall` created in a
  service namespace reconciled), and a package can create a cluster-scoped `ClusterRole`
  or an `App` that reaches other namespaces. So placement is solvable.

The single thing that does not work is creating `Addon` and `AddonRelease`.

## The single blocker, and what would lift it

The block is `--enable-webhook-client-verification` on the addon manager, admitting only
the manager's service account for `Addon` and `AddonRelease` writes.

This design becomes viable if any of the following happens:

- VMware relaxes the webhook to admit RBAC-authorized callers (for example a documented
  exception for Supervisor Service deployer accounts, or an opt-in policy), or
- VMware provides a supported way for a non-manager principal to create a package-free
  `Addon` + `AddonRelease` pair, or
- The `AddonInstall` gains a way to reference an `AddonConfigDefinition` directly,
  removing the need for the `Addon` and `AddonRelease` entirely (today `AddonInstall`
  requires the `Addon`: "The Addon must be created to reconcile the AddonInstall", and
  has no field pointing at an ACD).

None of these is something this project can cause. They are platform changes to watch
for.

## How to re-test when revisiting

Two cheap probes settle whether the block still stands, both runnable against a live
Supervisor without building or publishing anything:

1. **Can a non-manager account create an `AddonRelease`?** Mint a token for a service
   account that holds `create` on `addonreleases.addons.kubernetes.vmware.com`, and
   attempt to create one referencing an existing ACD:

   ```sh
   kubectl -n <svc-ns> create token <sa> --duration=10m
   # then, as that token, kubectl create an AddonRelease in vmware-system-vks-public
   ```

   If it succeeds, the webhook has been relaxed and this design is back on the table.

2. **Does the inline App path still deliver payload fetch-free?** Create a kapp-controller
   `App` with an `inline` fetch in a guest namespace and confirm the payload lands with
   no registry pull. This is already true today; re-run it only to confirm a VKS upgrade
   did not change it.

If both pass, the direct design can be built: a Supervisor Service that creates the ACD
(already allowed) plus the `Addon` and `AddonRelease` (newly allowed), with the inline
App relay carrying the payload. That would restore both the package-free and fetch-free
properties the shipped design gives up.
