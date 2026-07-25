# Implementation plan

Read [`design.md`](./design.md) first — it holds the API findings that constrain
everything below, and the reasoning behind the chosen approach.

**Status: nothing is built yet.** Step 0 gates the rest.

## Execution order

1. **Step 0 — confirm the template dialect** against a live Supervisor
   (see [`verify.md`](./verify.md)). Record findings in `design.md`.
2. Author `bundle/config/*` against the confirmed dialect.
3. Makefile + CI.
4. `examples/` + README.
5. Work the verification runbook end to end.

Step 0 is cheap and de-risks the whole build. The draft YAML below is written
against *assumed* template syntax and **will need correcting** once a real shipped
ACD has been dumped.

---

## Repository layout

```
bootstrap-addon-service/
├── README.md                          # the delegation pitch, install, quickstart
├── Makefile                           # render / lint / lock / bundle / release
├── .github/workflows/
│   ├── ci.yaml                        # PR: ytt render + YAML validation
│   └── release.yaml                   # tag v*: push bundle, cut service YAML
├── bundle/                            # imgpkg bundle root
│   ├── .imgpkg/images.yml             # ImagesLock — EMPTY, no images
│   └── config/
│       ├── values-schema.yaml         # ytt data values for the *service*
│       ├── addon.yaml
│       ├── addonconfigdefinition.yaml # the core piece
│       └── addonrelease.yaml          # no spec.package
├── examples/
│   ├── addoninstall.yaml
│   ├── addonconfig-inline.yaml
│   ├── addonconfig-configmap.yaml
│   └── argocd-bootstrap/
│       ├── README.md
│       ├── bootstrap-configmap.yaml
│       └── addonconfig.yaml
└── docs/
    ├── design.md
    ├── plan.md
    └── verify.md
```

Because the addon ships no container images, `.imgpkg/images.yml` is an empty
`ImagesLock`. Air-gap relocation is just the bundle itself — no image mirroring.
Worth stating in the README; it is a real benefit of the package-free approach.

---

## Files to author

### `bundle/config/addon.yaml`

```yaml
apiVersion: addons.kubernetes.vmware.com/v1alpha1
kind: Addon
metadata:
  name: bootstrap
  namespace: vmware-system-vks-public
spec:
  displayName: Cluster Bootstrap YAML
  description: >-
    Applies operator-supplied Kubernetes YAML into a VKS cluster at provisioning
    time, before GitOps tooling is aware of the cluster. Carries no Carvel package
    and no container images.
```

### `bundle/config/addonrelease.yaml` — the load-bearing one

```yaml
apiVersion: addons.kubernetes.vmware.com/v1alpha1
kind: AddonRelease
metadata:
  name: bootstrap.1.0.0
  namespace: vmware-system-vks-public
spec:
  addonRef: {name: bootstrap}
  version: 1.0.0
  addonConfigDefinitionRef:
    name: bootstrap.kubernetes.vmware.com.1.0.0
    namespace: vmware-system-vks-public
  # no spec.package        — the entire point (see design.md, Finding 1)
  # no dependsOn           — must land as early as the system allows
  kubernetesVersionConstraints: ">=1.31.0"   # confirm shape in Step 0
```

Naming mirrors the shipped convention (`ako.kubernetes.vmware.com.1.13.4+vmware.1-vks.1`).

### `bundle/config/addonconfigdefinition.yaml` — the core

Schema accepts **both** input paths — inline and ConfigMap:

```yaml
apiVersion: addons.kubernetes.vmware.com/v1alpha1
kind: AddonConfigDefinition
metadata:
  name: bootstrap.kubernetes.vmware.com.1.0.0
  namespace: vmware-system-vks-public
spec:
  schema:
    openAPIV3Schema:
      type: object
      properties:
        resources:
          type: string
          description: Inline multi-document Kubernetes YAML applied to the cluster.
        configMapRef:
          type: object
          description: ConfigMap in the cluster's Supervisor namespace; every key is applied.
          properties:
            name: {type: string}
        clusterRoleName: {type: string,  default: cluster-admin}
        syncPeriod:      {type: string,  default: 10m}
        noopDelete:      {type: boolean, default: false}

  templateInputResources:
    - inputName: bootstrapConfigMap
      apiVersion: v1
      kind: ConfigMap
      name: '{{ .values.configMapRef.name }}'   # input name IS documented as templatable
      scope: Namespace
      constraints:
        - operator: optional                     # inline-only usage must still render

  templateOutputResources:
    # 1 — Namespace (cluster-scoped). Body is effectively empty; confirm in Step 0.
    - targetClusterOutput:
        apiVersion: v1
        kind: Namespace
        name: vks-bootstrap
        scope: Cluster
        referenceType: Direct
      template: "{}"

    # 2 — ServiceAccount
    - targetClusterOutput:
        apiVersion: v1
        kind: ServiceAccount
        name: vks-bootstrap
        namespace: vks-bootstrap
        referenceType: Direct
      template: "{}"

    # 3 — ClusterRoleBinding. Body is top-level roleRef + subjects, NOT spec.
    - targetClusterOutput:
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRoleBinding
        name: vks-bootstrap
        scope: Cluster
        referenceType: Direct
      template: |
        roleRef:
          apiGroup: rbac.authorization.k8s.io
          kind: ClusterRole
          name: {{ .values.clusterRoleName }}
        subjects:
        - kind: ServiceAccount
          name: vks-bootstrap
          namespace: vks-bootstrap

    # 4 — the App that actually applies the payload
    - targetClusterOutput:
        apiVersion: kappctrl.k14s.io/v1alpha1
        kind: App
        name: vks-bootstrap
        namespace: vks-bootstrap
        referenceType: Direct
      template: |
        spec:
          serviceAccountName: vks-bootstrap
          syncPeriod: {{ .values.syncPeriod }}
          noopDelete: {{ .values.noopDelete }}
          fetch:
          - inline:
              paths:
                {{- range $k, $v := .inputs.bootstrapConfigMap.data }}
                {{ $k | toJson }}: {{ $v | toJson }}
                {{- end }}
                {{- if .values.resources }}
                "inline-resources.yml": {{ .values.resources | toJson }}
                {{- end }}
          template:
          - ytt: {ignoreUnknownComments: true}
          deploy:
          - kapp: {}
```

**Note the templates carry no `apiVersion`/`kind`/`metadata`** — per Finding 2, the
`template` field is the resource body only.

Two deliberate choices in output 4:

- **`toJson` on every payload value.** A JSON-quoted string is a valid YAML scalar,
  so embedding arbitrary multi-line YAML needs zero indentation arithmetic — far more
  robust than `nindent` against payloads with odd whitespace. This is the sprig
  dependency flagged in Step 0. If sprig is absent: fall back to a single fixed path
  key with manual indentation, or push the payload into a guest ConfigMap and use
  `inline.pathsFrom.configMapRef`.
- **`ytt` with `ignoreUnknownComments: true`.** `spec.template` is mandatory on an
  App, and this is the passthrough idiom that won't choke on `#` comments in user YAML.

Also needs `addonInstallPermission.accessPolicies` broad enough for the applier to
create a `ClusterRoleBinding` to `cluster-admin`. See design.md unverified item 6 —
this is the likeliest first-run failure.

### `bundle/config/values-schema.yaml`

ytt data values for the **service**, not the addon: `namespace` (default
`vmware-system-vks-public`), `addonName`, `version`, `kubernetesVersionConstraints`.
Keep it small — the service has almost nothing to configure, which is the point.

---

## Build and release

| Target | Does |
|---|---|
| `render`  | `ytt -f bundle/config` |
| `lint`    | render piped through YAML validation |
| `lock`    | `ytt -f bundle/config \| kbld -f - --imgpkg-lock-output bundle/.imgpkg/images.yml` (yields an empty lock) |
| `bundle`  | `imgpkg push -b $(REGISTRY)/$(NAME):$(VERSION) -f bundle --lock-output dist/bundle.lock.yml` |
| `release` | Renders `PackageMetadata` + `Package` with the resolved `@sha256:` digest into `dist/bootstrap-addon-service-$(VERSION).yaml` |

`REGISTRY` defaults to `ghcr.io/warroyo/bootstrap-addon-service` and is overridable —
document that for air-gapped users. The emitted service YAML must match the
two-document shape in design.md Finding 4.

- `.github/workflows/ci.yaml` — on PR: install ytt/kbld, `make lint`, fail on render errors.
- `.github/workflows/release.yaml` — on `v*` tag: `make lock bundle release`, push to
  GHCR using `GITHUB_TOKEN` with `packages: write`, attach `dist/*.yaml` to a release.

---

## Worked example — `examples/argocd-bootstrap/`

The example that justifies the project. A ConfigMap in the cluster's Supervisor
namespace holding the `argocd` Namespace, a repo-credentials `Secret`, and the Argo CD
app-of-apps root `Application`. Then:

```yaml
apiVersion: addons.kubernetes.vmware.com/v1alpha1
kind: AddonConfig
metadata:
  name: <cluster-name>-bootstrap        # required <cluster>-<addon> convention
  namespace: <cluster-namespace>
  annotations:
    clusteraddon.addons.kubernetes.vmware.com/owned-for-deletion: "true"
spec:
  clusterName: <cluster-name>
  values:
    configMapRef: {name: argocd-bootstrap}
```

That annotation is not optional in practice — without it the `AddonConfig` is not
garbage-collected when the `ClusterAddon` goes away.

Plus an `AddonInstall` selecting clusters by label, mirroring the observed Cilium
example in design.md Finding 5, so new clusters carrying the label are bootstrapped
at creation.

---

## Open risks

- **Template dialect unverified** — Step 0 gates everything.
- **RBAC escalation** on the `cluster-admin` `ClusterRoleBinding` — likeliest
  first-run failure.
- **Soft dependency on in-guest kapp-controller** — present in VKS ≥ 3.5, but an
  implementation detail rather than a contract. If a future release drops it, the
  relay breaks.
- **Ordering** — no `dependsOn`, so the addon installs as early as the system
  permits. Confirm that genuinely precedes Argo in every path during verification
  step 8.
- **Registry default** is GHCR, inferred rather than stated. Trivially overridable
  via `REGISTRY`, but flagging the assumption.
