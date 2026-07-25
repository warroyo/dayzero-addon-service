# Argo CD bootstrap

The chicken-and-egg this service exists to solve.

Argo CD cannot bootstrap a cluster it does not know about, and during provisioning
nothing outside the Supervisor can reach the workload cluster to tell it. Something
has to plant the first `Application` from the inside.

This addon runs on the Supervisor as the cluster is provisioned, so it can create the
`argocd` namespace, the repository credentials, and an app-of-apps root `Application`
before anything else needs them. From that point Argo CD owns the cluster and this
addon does nothing further.

## Use it

1. Edit [`addonconfig.yml`](./addonconfig.yml): replace `my-cluster`, `my-namespace`
   and the `repoURL`, and supply real repository credentials.
2. Apply it into the cluster's Supervisor namespace.
3. Apply [`../addoninstall.yml`](../addoninstall.yml) once per Supervisor namespace,
   and label the clusters that should be seeded:

```sh
kubectl label cluster my-cluster addons.kubernetes.vmware.com/bootstrap=enabled
```

Clusters created later with that label are seeded during provisioning, with no manual
step at all. That is the case worth testing — seeding an existing cluster proves the
mechanism, but seeding a brand-new one proves the chicken-and-egg is actually solved
rather than moved.

## Credentials

The example inlines a repository password so it reads as a single self-contained
file. Do not do that for real. An `AddonConfig` is a plain object in the Supervisor
namespace and is readable by everyone with access to that namespace.

Prefer either an SSH deploy key or a token injected by whatever already manages
secrets in your Supervisor namespace, and keep the rest of the payload in this
`AddonConfig`. The three payload sources compose, so credentials can arrive by one
route and the `Application` by another.

## Checking it worked

On the Supervisor, template rendering failures show up here:

```sh
kubectl -n my-namespace get clusteraddon my-cluster-bootstrap -o yaml
```

In the workload cluster, the apply log is on the `App`:

```sh
kubectl -n vks-bootstrap get app vks-bootstrap -o yaml   # status.deploy.stdout
kubectl -n argocd get application,secret
```
