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

## Two versions

`addon/` is the source for a *single* package version. The bundle is a catalog of many, so
the build keeps two versions apart:

| | |
|---|---|
| `REPO_VERSION` | The catalog release: the imgpkg tag, the `AddonRepository`'s `spec.version` and `repositoryVersion`, the suffix on both object names, and the Supervisor Service package version. A `v*` git tag sets it, and it is unrelated to any package version |
| `PKG_VERSIONS` | Every package version the catalog serves. The single source for both the bundle contents and the `package-offerings` annotation |

Shipping every version in one catalog is what makes a version bump cheap for consumers. A
registered `AddonRepository` is frozen by the validating webhook — only `spec.addonFilters`
is mutable, and that is a helm-repository field, so an imgpkg repository cannot be edited
at all, `imageURL` included. A consumer moving a `releaseFilter` pin between two versions
of the same catalog touches nothing on the Supervisor, because both `AddonRelease`s already
exist. Moving to a *new catalog* still costs a new repo+install pair, which is what VMware
does too (`standard-packages` 3.6 stays registered next to `vks-addons` 3.7). The win is
that it happens per catalog release rather than per package version.

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
| `install-manifest` | Renders `install/addonrepository.tpl.yml`, generating `package-offerings` from `PKG_VERSIONS` |
| `check` | Asserts the offerings annotation and the bundle contents are the same set |
| `render` | Shows one Package and the ACD decoded from its annotation |
| `test`   | `go vet` and `go test` in `test/` |
| `push`   | `kctrl package repository release`, tagged `$(REPO_VERSION)` |

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
`addons.kubernetes.vmware.com/package-offerings`, and treats it as a complete, exact
manifest — the builtin `vks-addons` repository lists all 22 of its packages with no version
mismatches. It is also frozen once installed, so a wrong one cannot be corrected in place;
it has to be a new repo+install pair. It is therefore generated from `PKG_VERSIONS` in both
`install/addonrepository.tpl.yml` and `supervisor-service/config/repo.yml`, never
hand-maintained, and `make check` compares it against the assembled bundle.

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
