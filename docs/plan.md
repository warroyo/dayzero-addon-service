# Build and layout

Read [`design.md`](./design.md) first for the API constraints behind everything here.
[`verify.md`](./verify.md) covers installing and testing the result.

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
│   └── 1.0.2.yml
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
```

## One moving tag, three settings

`addon/` is the source for a *single* package version. The bundle is a catalog of many, so
the build keeps three things apart:

| | |
|---|---|
| `CATALOG_TAG` | The tag the registered `AddonRepository` points at, and the only one it ever points at. Constant (`stable`); every release re-points it at the newest bundle |
| `PKG_VERSIONS` | Every package version the catalog serves. The single source for the bundle contents |
| `REPO_VERSION` | The immutable snapshot tag for one publish, plus the Supervisor Service package version. Nothing registered points at it. A `v*` git tag sets it, and it is unrelated to any package version |

A registered `AddonRepository` is frozen by the validating webhook — only
`spec.addonFilters` is mutable, and that is a helm-repository field, so an imgpkg
repository cannot be edited at all, `imageURL` and the `package-offerings` annotation
included, and not even a re-apply of an identical value is allowed. The design routes
around that rather than paying it per release: the registration is permanent, the tag
underneath it moves, and the manager re-resolves the tag by itself (measured at 569s). A
release therefore costs an admin nothing.

Shipping every version in one catalog is what makes a version bump cheap for consumers on
top of that. A consumer moving a `releaseFilter` pin between two versions touches nothing
on the Supervisor, because both `AddonRelease`s already exist.

The remaining cost is that the tag is mutable, so the *registration* no longer identifies
what it serves. Two things keep that honest: `released/` freezes each package version's
YAML so the catalog is append-only — a version already published never changes meaning —
and each publish also gets an immutable snapshot tag, so a deliberate pin is always
available. VMware's own repositories take the other route (`standard-packages` 3.6 stays
registered next to `vks-addons` 3.7), which is the right call for a catalog that ships
with the platform and the wrong one for a catalog that adds a package version a week.

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
package contents, which is the guest-side image fetch this design deliberately avoids
(see [`design.md`](./design.md)).

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

### The package-offerings annotation

The webhook rejects an `AddonRepository` without
`addons.kubernetes.vmware.com/package-offerings`, on both fetch flavours — the check is on
the kind, not on `spec.fetch`. But it is **declarative, not enforced**. Verified on a live
Supervisor: a repository whose annotation named a single package version, pointed at a
bundle carrying three, reconciled `Ready=True` with no error and materialised an
`AddonRelease` and an `AddonConfigDefinition` for all three, including the versions the
annotation omitted. A version-less declaration (`"versions": []`) behaves the same way and
passes admission.

That matters because the annotation is frozen once an `AddonRepositoryInstall` references
it — the rejection fires on the update operation, so even re-applying an identical value
is refused:

```
annotations...package-offerings: AddonRepository is in use by an
AddonRepositoryInstall, package-offerings annotation update is not allowed
```

A version list written there could therefore never be brought up to date as the catalog
grows, and would be a permanent lie. So `install/addonrepository.tpl.yml` and
`supervisor-service/config/repo.yml` both name the package and list no versions. The
earlier reading of this annotation as an exact manifest came from the builtin repository
happening to keep it accurate, not from any rejection.

`make check` no longer compares it against the bundle — there is nothing to compare.
Instead it asserts the bundle serves exactly `PKG_VERSIONS`, and that no package version
or snapshot tag leaked into the install manifest, since anything release-specific there
could never be applied over an existing registration.

CI (`.github/workflows/build-release.yml`) runs `make release` on a `v*` tag, which gates
on `check` before publishing, and attaches the rendered install manifest to a GitHub
release.

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
