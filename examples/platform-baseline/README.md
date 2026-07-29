# Platform baseline

The day-zero state a cluster needs before anyone uses it: namespaces, service
accounts, RBAC, quotas and a default-deny NetworkPolicy.

None of it is exotic, and that is the point. These are the resources that otherwise
arrive by someone being handed a workload-cluster kubeconfig and running `kubectl`,
which is exactly the handover this addon removes.

[`addonconfig.yml`](./addonconfig.yml) seeds:

| | |
|---|---|
| `Namespace` | one per team, labelled |
| `ServiceAccount` + `RoleBinding` | an identity for CI to deploy with, scoped to `edit` in its own namespace |
| `RoleBinding` to an SSO group | human access granted to a group rather than to individuals |
| `ClusterRoleBinding` | cluster-wide `view` for the platform team |
| `ResourceQuota`, `LimitRange` | consumption guardrails |
| `NetworkPolicy` | deny-by-default ingress |

## Use it

1. Edit [`addonconfig.yml`](./addonconfig.yml): replace `my-cluster` and
   `my-namespace`, and set the SSO group names for your environment. Confirm the
   exact subject strings. On a Supervisor they look like `sso:<group>@<domain>`, and
   a wrong string binds silently to nobody.
2. Apply it into the cluster's Supervisor namespace.
3. Apply [`../addoninstall.yml`](../addoninstall.yml) once per Supervisor namespace,
   then label the clusters to seed:

```sh
kubectl label cluster my-cluster addons.kubernetes.vmware.com/dayzero=enabled
```

Clusters created later with that label are seeded during provisioning, with no manual
step. Seeding an existing cluster proves the mechanism works; seeding a brand-new one
is what proves the tenant never needs a kubeconfig.

## Scaling this up

The payload is data, so the usual approach is a per-team or per-environment file and
whatever already templates your Supervisor namespace content. Changing the baseline is
an edit to the `AddonConfig`, with no rebuild, version bump or re-upload, and
kapp-controller prunes resources you remove from it.

Use `values.resources` (structured, as here) when the payload is authored alongside
the `AddonConfig`; use a `<cluster-name>-dayzero` ConfigMap when it is large or
managed separately. The two compose.

## Credentials

Image pull secrets and similar belong here too, but do not inline them. An
`AddonConfig` is a plain object in the Supervisor namespace, readable by anyone with
access to it. Source secrets from whatever already manages them in that namespace and
keep the rest of the baseline here. The payload sources compose, so the two can arrive
by different routes.

## Checking it worked

On the Supervisor, template rendering failures show up here:

```sh
kubectl -n my-namespace get clusteraddon my-cluster-dayzero -o yaml
```

In the workload cluster, the apply log is on the guest `PackageInstall`:

```sh
kubectl -n vmware-system-tkg get pkgi my-cluster-dayzero -o yaml   # status.usefulErrorMessage
kubectl get ns team-a
kubectl -n team-a get sa,rolebinding,resourcequota
```
