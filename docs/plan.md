# Build and layout

Read [`design.md`](./design.md) first — it holds the API findings that constrain
everything here. [`verify.md`](./verify.md) covers installing and testing the result.

This document described a plan before anything was built. It now describes what
exists. The draft YAML it used to carry was written against *assumed* template syntax
and was wrong in almost every particular; the real definition is
[`config/addonconfigdefinition.yml`](../config/addonconfigdefinition.yml), and the
corrections are recorded as Findings 2 and 6 in `design.md`.

## Layout

```
bootstrap-addon-service/
├── Makefile                       # render / test / release
├── package-build.yml              # kctrl PackageBuild
├── package-resources.yml          # Package + PackageMetadata + PackageInstall
├── config/                        # ytt-rendered, imgpkg-bundled
│   ├── values.yml                 # ytt data-values schema, surfaced by vCenter at install
│   ├── addon.yml
│   ├── addonrelease.yml           # no spec.package
│   └── addonconfigdefinition.yml  # the core piece
├── test/                          # renders the definition's Go templates
├── examples/
└── docs/
```

Modelled on [`warroyo/argocd-attach-service`](https://github.com/warroyo/argocd-attach-service),
a working Supervisor Service, minus everything to do with a controller: this service
ships no image, no Dockerfile and no Go binary. `kbld` therefore resolves nothing and
the images lock comes out empty, which makes air-gap relocation just the bundle
itself.

## Two template languages, deliberately kept apart

`config/` is processed by **ytt** at build time (`#@` directives), and the strings
inside `templateOutputResources[].template` are processed by the **addon controller's
Go templates** at reconcile time (`{{ }}`). They do not collide, but interleaving them
is unreadable.

So ytt is used only for values that vary per *release* — addon name, version,
namespace — and the guest-cluster resource names are hardcoded to `vks-bootstrap`.
Anything that varies per *cluster* goes through the definition's own schema and is
read at runtime as `.Values.<field>`.

## Build

| Target | Does |
|---|---|
| `render` | `ytt -f config` |
| `test`   | `go vet` + `go test` in `test/` — renders each output template and asserts the result |
| `release`| `kctrl package release`, then concatenates the generated `metadata.yml` and `package.yml` into `bootstrap-addon.yml` |

`bootstrap-addon.yml` is the artifact uploaded through **Workload Management →
Services → Add Service**. `package-build.yml` sets the bundle destination
(`ghcr.io/warroyo/bootstrap-addon-service`); override it there for a different
registry.

CI mirrors this: `.github/workflows/test.yml` runs render + tests on every PR, and
`.github/workflows/build-release.yml` runs `make release` on a `v*` tag and attaches
the artifact to a GitHub release.

## Why `test/` exists

Not for coverage. There is no way to iterate against a real Supervisor — an
`AddonConfigDefinition` cannot be created by hand, even by the vSphere administrator
(see `verify.md`), so the only loop that exercises the real controller is build →
upload → install.

`test/render_test.go` closes part of that gap by reproducing the controller's template
environment: sprig, plus the Helm-style `toYaml` the controller registers but sprig
does not provide. It asserts that every output stays body-only (no `apiVersion`,
`kind` or `metadata`, per Finding 2), that the `App` renders to valid YAML with a
non-empty inline paths map for every combination of payload sources including none,
and that awkward payloads — colons in ConfigMap keys, comments and blank lines in raw
YAML — survive the JSON encoding intact.

`test/schema_test.go` validates the rendered resources against the real CRD schemas in
`test/schemas/`. Note that `kubectl apply --dry-run=client --validate=true` is **not**
a substitute: it accepts invalid enum values and invented fields alike. Server-side
dry run hits the same RBAC wall as a real apply.

The schemas are stricter than the API server on purpose. A CRD prunes unknown fields
rather than rejecting them, so a typo'd field name yields a silently wrong definition;
`additionalProperties: false` is injected wherever the CRD does not explicitly
preserve unknown fields, turning that into a test failure. Regenerate them against a
Supervisor with:

```sh
for c in addonconfigdefinitions addons addonreleases; do
  kubectl get crd $c.addons.kubernetes.vmware.com -o json | jq '
    def clean:
      if type == "object" then
        (if has("properties") and (has("x-kubernetes-preserve-unknown-fields") | not)
         then . + {additionalProperties: false} else . end)
        | with_entries(select(.key | startswith("x-kubernetes-") | not))
        | map_values(clean)
      elif type == "array" then map(clean)
      else . end;
    .spec.versions[0].schema.openAPIV3Schema | clean
  ' > test/schemas/$c.json
done
```

What none of it can tell you is whether the addon controller accepts and acts on the
definition. That is `verify.md` step 2.
