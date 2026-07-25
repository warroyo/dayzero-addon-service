# Platform baseline

The day-zero state a cluster needs before anyone uses it: namespaces, service
accounts, RBAC, quotas and a default-deny NetworkPolicy.

None of it is exotic. That is the point — it is the set of resources that otherwise
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
   exact subject strings — on a Supervisor they look like `sso:<group>@<domain>`, and
   a wrong string binds silently to nobody.
2. Apply it into the cluster's Supervisor namespace.
3. Apply [`../addoninstall.yml`](../addoninstall.yml) once per Supervisor namespace,
   then label the clusters to seed:

```sh
kubectl label cluster my-cluster addons.kubernetes.vmware.com/bootstrap=enabled
```

Clusters created later with that label are seeded during provisioning, with no manual
step. Seeding an existing cluster proves the mechanism works; seeding a brand-new one
is what proves the tenant never needs a kubeconfig.

## Scaling this up

The payload is data, so the usual approach is a per-team or per-environment file and
whatever already templates your Supervisor namespace content. Changing the baseline is
an edit to the `AddonConfig` — no rebuild, no version bump, no re-upload — and
kapp-controller prunes resources you remove from it.

Use `values.resources` (structured, as here) when the payload is authored alongside
the `AddonConfig`; use a `<cluster-name>-bootstrap` ConfigMap when it is large or
managed separately. The two compose.

## Credentials

Image pull secrets and similar belong here too, but do not inline them. An
`AddonConfig` is a plain object in the Supervisor namespace, readable by anyone with
access to it. Source secrets from whatever already manages them in that namespace and
keep the rest of the baseline here — the payload sources compose, so the two can
arrive by different routes.

## Checking it worked

On the Supervisor, template rendering failures show up here:

```sh
kubectl -n my-namespace get clusteraddon my-cluster-bootstrap -o yaml
```

In the workload cluster, the apply log is on the `App`:

```sh
kubectl -n vks-bootstrap get app vks-bootstrap -o yaml   # status.deploy.stdout
kubectl get ns team-a
kubectl -n team-a get sa,rolebinding,resourcequota
```
