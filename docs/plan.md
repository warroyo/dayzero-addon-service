# Build and layout

Read [`design.md`](./design.md) first for the API constraints behind everything here.
[`verify.md`](./verify.md) covers installing and testing the result.

## Layout

```
bootstrap-addon-service/
├── Makefile                       # bundle / render / test / push
├── addon/                         # source for the addon-repository bundle
│   ├── values.yml                 # ytt data values (name, version, k8s constraints)
│   ├── addonconfigdefinition.yml  # the ACD, encoded into the Package annotation
│   ├── package.yml                # the Package (ACD annotation + inline render)
│   ├── metadata.yml               # the PackageMetadata
│   └── render/                    # the package's ytt, shared with the tests
│       ├── values-schema.yml
│       └── render.yml
├── install/
│   └── addonrepository.yml        # AddonRepository + AddonRepositoryInstall (admin apply)
├── supervisor-service/            # the same two CRs, wrapped as a Supervisor Service
│   ├── config/
│   ├── package-build.yml
│   └── package-resources.yml
├── examples/                      # tenant AddonInstall + AddonConfig
├── test/                          # renders the ACD templates and the package ytt
└── docs/
```

## Two template languages, kept apart

`addon/` mixes two template systems on purpose, and they never overlap:

- **ytt at build time** (`#@` directives) fills in per-release values: addon name,
  version, and the encoded ACD. It renders `addon/` into the bundle.
- **Go templates at reconcile time** (`{{ }}`) are the ACD's output templates, evaluated
  per cluster by the addon controller. They stay literal through ytt and through the
  gzip+base64 encoding, and the controller evaluates them later.
- **ytt again in the guest**: the package's own render (`addon/render/`) is ytt, run by
  the guest kapp-controller over the values Secret.

## Build

| Target | Does |
|---|---|
| `bundle` | Renders the ACD, encodes it into the Package annotation, injects the render files, and assembles the package-repository bundle under `build/bundle` |
| `render` | Shows the Package and the ACD decoded from its annotation |
| `test`   | `go vet` and `go test` in `test/` |
| `push`   | `imgpkg push` the bundle to `ADDON_REPO` |

The ACD travels in the Package's `addon-config-definition` annotation as gzip+base64,
which is how the shipped cilium addon delivers its definition. `make bundle` does the
encoding, so the ACD stays a readable file. The bundle's `.imgpkg/images.yml` is an empty
ImagesLock: the package content is config only, so there are no container images to
mirror for air-gap.

CI (`.github/workflows/build-release.yml`) runs `make push` on a `v*` tag and attaches the
install manifest to a GitHub release.

## Why `test/` exists

The addon kinds cannot be created by hand (the manager owns `Addon` and `AddonRelease`),
so the only loop that exercises the real controller is build, publish, install. The tests
close as much of that gap as possible without a Supervisor:

`test/render_test.go` renders the ACD's Go templates with the controller's function map
(sprig plus the Helm-style `toYaml`), then runs the resulting values through the package's
own ytt (`addon/render/`). It asserts the full round trip: a payload encoded into the
values Secret comes back out as the resources it went in as, for each payload source and
all combined, and that an empty payload renders to nothing rather than erroring. Because
the render files are shared, the test runs the exact ytt the guest runs.

`test/schema_test.go` validates the rendered ACD against the real CRD schema in
`test/schemas/`, pins the namespace and labels the validating webhook requires (which the
CRD schema does not cover), and checks the Package's annotation round-trips through
gzip+base64 back to the ACD.

The schemas are stricter than the API server on purpose. A CRD prunes unknown fields
rather than rejecting them, so a typo'd field yields a silently wrong definition;
`additionalProperties: false` is injected wherever the CRD does not preserve unknown
fields, turning that into a test failure. Regenerate them against a Supervisor with:

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

What none of it can tell you is whether the `AddonRepository` round trip works on a
Supervisor. That is `verify.md`.
