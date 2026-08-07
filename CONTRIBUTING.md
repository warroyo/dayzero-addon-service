# Contributing

How this repository is built, tested and released. For installing and using the addon, see
the [README](./README.md); for why it is designed the way it is, see
[`docs/design.md`](./docs/design.md); for verifying a change end to end on a real
Supervisor, see [`docs/verify.md`](./docs/verify.md).

The VKS addon system itself — the resource model, the manager-owned webhooks, why an
`AddonRepository` is the only way in — is not documented here. That is platform knowledge
rather than anything specific to this addon, and it lives in the `vks-addons` skill.

```sh
make bundle              # stage the catalog for kctrl (build/bundle)
make render              # inspect one Package and the ACD it carries
make install-manifest    # render the admin-apply CRs (build/install/addonrepository.yml)
make check               # assert the bundle serves PKG_VERSIONS and the manifest is release-independent
make test                # render the ACD templates and the package ytt, validate against CRD schemas
make push                # publish the catalog: snapshot tag, then move :stable onto it
make supervisor-service  # kctrl package release -> dayzero-addon.yml
make release             # check, push the catalog, build the service package
```

## Layout

```
dayzero-addon-service/
├── Makefile                       # freeze / bundle / install-manifest / check / push
├── addon/                         # source for one package version
│   ├── values.yml                 # ytt data values (name, version, k8s constraints)
│   ├── addonconfigdefinition.yml  # the ACD, encoded into the Package annotation
│   ├── package.yml                # the Package (ACD annotation + inline render)
│   ├── metadata.yml               # the PackageMetadata
│   └── render/                    # the package's ytt, shared with the tests
│       ├── values-schema.yml
│       └── render.yml
├── released/dayzero-addon-repo/   # frozen Package YAML, one file per released version
│   ├── 1.0.1.yml
│   ├── 1.0.2.yml
│   └── 1.0.3.yml
├── repo/
│   └── pkgrepo-build.yml.tpl      # PackageRepositoryBuild for `kctrl package repository release`
├── install/
│   ├── addonrepository.tpl.yml    # AddonRepository + AddonRepositoryInstall (ytt template)
│   └── values.yml                 # its data values, supplied by `make install-manifest`
├── supervisor-service/            # the same two CRs, wrapped as a Supervisor Service
│   ├── config/
│   ├── package-build.yml
│   └── package-resources.yml
├── examples/                      # tenant AddonInstall + AddonConfig
├── test/                          # renders the ACD templates and the package ytt
└── docs/
    ├── design.md                  # why the addon is shaped this way
    └── verify.md                  # end-to-end verification runbook
```

## One moving tag, three settings

`addon/` is the source for a *single* package version. The bundle is a catalog of many, so
the build keeps three things apart:

| | |
|---|---|
| `CATALOG_TAG` | The tag the registered `AddonRepository` points at, and the only one it ever points at. Constant (`stable`); every release re-points it at the newest bundle |
| `PKG_VERSIONS` | Every package version the catalog serves. The single source for the bundle contents |
| `REPO_VERSION` | The immutable snapshot tag for one publish, plus the Supervisor Service package version. Nothing registered points at it. A `v*` git tag sets it, and it is unrelated to any package version |

**The one rule to keep in mind when changing the build:** a registered `AddonRepository`
cannot be modified, so the install manifest has to be byte-identical from one release to
the next. That is why the names carry no version, `spec.version` is a constant, the
`imageURL` is a floating tag, and the offerings annotation lists no versions. A release
delivers by moving the tag, not by reissuing the manifest, and `make check` fails the
build if anything release-specific creeps back in.

The corollary is that the catalog must stay **append-only**. The tag moves under a live
registration, so a version removed from `PKG_VERSIONS` would disappear from clusters
pinned to it. `released/` keeps every published version frozen for that reason.

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
| `freeze` | Renders any version in `PKG_VERSIONS` that has no file in `released/` yet, and only those |
| `bundle` | Stages `build/bundle` for kctrl: every frozen version and the `PackageMetadata` under `packages/<refName>/`, plus a stamped `pkgrepo-build.yml` |
| `install-manifest` | Renders `install/addonrepository.tpl.yml`. Deliberately identical from release to release |
| `check` | Asserts the bundle serves exactly `PKG_VERSIONS`, and that no package version or snapshot tag leaked into the install manifest |
| `render` | Shows one Package and the ACD decoded from its annotation |
| `test`   | `go vet` and `go test` in `test/` |
| `push`   | `kctrl package repository release` twice over the same bundle: once tagged `$(REPO_VERSION)`, once tagged `$(CATALOG_TAG)`. The second is content-addressed, so it only moves the tag |

The ACD travels in the Package's `addon-config-definition` annotation as gzip+base64,
which is how the shipped cilium addon delivers its definition. `freeze` does the encoding,
so the ACD stays a readable file.

### The two kctrl release flows

The repo runs both of kctrl's authoring flows, on two different things:

- **The catalog** is a package repository. `make push` runs `kctrl package repository
  release --chdir build/bundle -v <ver> -t <ver>` against a `pkgrepo-build.yml` and a
  `packages/<refName>/` tree, which is the layout that command expects. kctrl runs `kbld -f
  packages --imgpkg-lock-output .imgpkg/images.yml` and then `imgpkg push`, so the
  ImagesLock is generated from the packages rather than hand-written. Our packages fetch
  inline and reference no images, so it comes out empty — there is nothing to mirror for
  air-gap. `-t` is needed because kctrl otherwise tags `build-<TIMESTAMP>`, and the tag is
  what the `AddonRepository`'s `imageURL` points at. kctrl also drops a Carvel
  `PackageRepository` CR in the staging dir; VKS does not use it, and it is not part of
  the pushed bundle.
- **The Supervisor Service wrapper** is a single package, not a repository: what gets
  uploaded to vCenter is a package *reference*, a `Package` plus its `PackageMetadata`. So
  it runs `kctrl package release` from `supervisor-service/` against the standard
  `package-build.yml` / `package-resources.yml` pair, and `make supervisor-service`
  concatenates the two files kctrl writes to `carvel-artifacts/packages/<refName>/` into
  `dayzero-addon.yml`.

The `Package` files themselves are rendered by ytt rather than by `kctrl package release
--repo-output`, because each one has to carry a hand-authored ACD in an annotation and
fetch its render files inline. kctrl's package flow would build an imgpkg bundle for the
package contents, which is a second guest-side image fetch this design deliberately
avoids.

### Why released package YAML is frozen

Because the ACD lives *inside* each Package, a Package is not a pointer to a definition —
it is the definition. Re-rendering `1.0.1` with today's `addon/` templates would produce a
file still called `1.0.1` that means something else, and consumers pinned to it would get
the change silently on the next catalog release. So `released/` holds the rendered output
per version, committed, and `bundle` copies rather than re-renders. `freeze` uses `gzip -n`
so the encoding does not carry an mtime and a re-render is reproducible.

`test/released_test.go` guards the frozen files: filename, `metadata.name`, `spec.version`
and the name of the ACD decoded from the annotation all have to agree. `.github/workflows/test.yml`
additionally fails if `make bundle` generated a file that is not committed.

### The package-offerings annotation lists no versions

Both `install/addonrepository.tpl.yml` and `supervisor-service/config/repo.yml` emit the
annotation naming the package with `"versions": []`. It is required — the resource is
rejected without it — but it is declarative: the manager materialises what the bundle
contains and never checks it against the annotation. Since the annotation is frozen once
installed, a version list written there could never be brought up to date as the catalog
grows. Do not "fix" it by enumerating versions.

### Releasing

Push a `v*` tag. `v1.1.0` sets `REPO_VERSION=1.1.0`, which names the snapshot tag and the
Supervisor Service package version; it says nothing about package versions, which come
from `PKG_VERSIONS`.

CI (`.github/workflows/build-release.yml`) runs `make release`, gating on `check`, then
publishes the bundle to both tags, builds the Supervisor Service package, and attaches
`dayzero-addonrepository.yml` and `dayzero-addon.yml` to a GitHub release. Moving
`:stable` is the step that delivers: registered repositories follow it within about ten
minutes, so the attached artifacts are unchanged from the previous release and nobody has
to re-apply them.

To ship a new package version: change `addon/`, append the version to `PKG_VERSIONS`, run
`make bundle`, commit the new file under `released/`, and push a `v*` tag.

`supervisor-service/config/values.yml` is generated from `values.yml.tpl`. Edit the
template, not the generated file.

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
