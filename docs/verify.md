# Verification runbook

Everything here needs a live Supervisor and a VKS cluster. Step 0 is a prerequisite
for writing any code; steps 1–8 validate the finished service.

---

## Step 0 — confirm the template dialect (do this first)

`docs/design.md` lists five unverified assumptions about the `AddonConfigDefinition`
template dialect. All of them are answerable by reading a shipped ACD, because
AKO / Cilium / Telegraf are working examples of the exact API we're targeting.

```sh
# what's available
kubectl -n vmware-system-vks-public get addonconfigdefinition

# dump a real one — pick ako, cilium or telegraf
kubectl -n vmware-system-vks-public get addonconfigdefinition <name> -o yaml

# the CRD itself, for authoritative field descriptions
kubectl get crd addonconfigdefinitions.addons.kubernetes.vmware.com -o yaml
kubectl explain addonconfigdefinitions.spec.templateOutputResources --recursive
```

Record answers to these in `design.md`:

| # | Question | Why it matters |
|---|----------|----------------|
| 1 | Template context root — `.values.*`, `.inputs.<name>.*`, `.cluster.*`? | Every template in the plan assumes a shape |
| 2 | Is sprig registered (is `toJson` available)? | Load-bearing for embedding payload YAML without indentation math |
| 3 | Does `targetClusterOutput.name` accept templating? | Plan assumes static names |
| 4 | What does an empty required `template` accept for `Namespace`/`ServiceAccount`? | Plan guesses `"{}"` |
| 5 | Exact shape of `AddonRelease.spec.kubernetesVersionConstraints` | Plan guesses a semver range string |

Also worth checking: what identity applies `Direct` outputs, since creating a
`ClusterRoleBinding` to `cluster-admin` requires that identity to hold `cluster-admin`
or the `escalate` verb.

```sh
kubectl -n vmware-system-vks get sa,clusterrolebinding | grep -i addon
```

**If assumptions 1 or 2 are wrong, correct `bundle/config/addonconfigdefinition.yaml`
before going further.** Fallbacks for a missing `toJson` are in `docs/plan.md`.

---

## Step 1 — build locally

```sh
make render                    # inspect the rendered addon YAML
make lint
make lock bundle release       # → dist/bootstrap-addon-service-1.0.0.yaml
```

Confirm the emitted service YAML is two documents (`PackageMetadata` + `Package`),
that `fetch.imgpkgBundle.image` carries a resolved `@sha256:` digest, and that
`bundle/.imgpkg/images.yml` is an empty `ImagesLock` (no images is expected).

## Step 2 — install the Supervisor Service

vCenter → **Supervisor Management → Services → Add New Service → Upload**, select
`dist/bootstrap-addon-service-1.0.0.yaml`, then install it on the Supervisor.

## Step 3 — confirm the definition landed

```sh
kubectl -n vmware-system-vks-public get addon,addonrelease,addonconfigdefinition | grep bootstrap
```

All three should be present. If the `AddonRelease` is rejected, the most likely cause
is the missing `spec.package` — check the admission error against design.md Finding 1.

## Step 4 — attach it to a cluster

Apply, from `examples/`:

1. the bootstrap ConfigMap into the cluster's Supervisor namespace
2. the `AddonConfig` (named `<cluster>-bootstrap`, with the
   `owned-for-deletion` annotation)
3. the `AddonInstall`

## Step 5 — watch reconciliation on the Supervisor

```sh
kubectl -n <cluster-ns> get clusteraddon
kubectl -n <cluster-ns> get clusteraddon <name> -o yaml
```

Check status conditions and which `AddonRelease` is active. **Template rendering
failures surface here** — this is the primary debugging surface for the ACD.

## Step 6 — confirm in the guest cluster

```sh
kubectl get ns vks-bootstrap
kubectl -n vks-bootstrap get app vks-bootstrap -o yaml
```

Read `status.friendlyDescription` for a one-line verdict and `status.deploy.stdout`
for the full kapp apply log. Then confirm the actual payload resources exist:

```sh
kubectl get ns argocd
kubectl -n argocd get application,secret
```

## Step 7 — lifecycle

- Edit the bootstrap ConfigMap → confirm kapp-controller reconciles the change and
  **prunes** resources removed from the payload.
- Delete the `AddonInstall` → confirm cleanup, or retention if
  `stopMatchingBehavior: Retain`.
- Confirm the `AddonConfig` is garbage-collected with the `ClusterAddon` (this is what
  the `owned-for-deletion` annotation buys).

## Step 8 — the real test

Create a **brand-new** cluster with the `AddonInstall` label already in place.
Confirm Argo CD's root `Application` is present without any manual step, and that it
landed early enough to be useful.

This is the one that proves the chicken-and-egg is actually solved rather than just
moved.

---

## Troubleshooting

| Symptom | Look at |
|---|---|
| `AddonRelease` rejected at admission | Missing `spec.package` — design.md Finding 1 |
| ClusterAddon stuck, template error | `kubectl -n <cluster-ns> get clusteraddon -o yaml` → conditions |
| Nothing appears in guest cluster | Is `referenceType: Direct` set on all four outputs? Default is `ValuesRef` |
| `ClusterRoleBinding` forbidden | RBAC escalation-prevention — design.md unverified item 6 |
| App exists but payload not applied | `kubectl -n vks-bootstrap get app vks-bootstrap -o yaml` → `status.deploy.stdout` |
| ytt errors on user payload | Confirm `ignoreUnknownComments: true` is set on the App's template stage |
