# Frozen package YAML

One file per released package version of the `dayzero-addon-repo` catalog. `make bundle`
copies these into the bundle and renders only a version that has no file here yet.

**Do not re-render or edit these.** Each Package carries its `AddonConfigDefinition`
inside itself, so editing a released file changes what that version means for consumers
already pinned to it. A change to the addon is a new package version.

## Adding a version

1. Change `addon/`.
2. Append the new version to `PKG_VERSIONS` in the `Makefile`.
3. `make bundle` — the new version is rendered here, the rest are copied.
4. Commit the new file with the rest of the change.
