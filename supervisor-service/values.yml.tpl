#! Template for config/values.yml. `make supervisor-service` stamps the repo, the floating
#! tag and both object names from the Makefile and writes the generated file into config/,
#! which is what kctrl reads. Edit this file, not the generated one.
#@data/values-schema
---

#! Namespace the supervisor deploys this service into. Filled in by the supervisor,
#! do not edit. The AddonRepository and AddonRepositoryInstall land here (the deploy
#! rewrites their namespace), and the manager reconciles them from here.
namespace: ""

#! The catalog bundle the manager fetches, on the floating tag. Nothing here changes when
#! the catalog gains a package version -- the tag is re-pointed and the manager
#! re-resolves it -- so a service instance keeps delivering new package versions without
#! being upgraded.
addon_repo_image: "@@REPO@@:@@CATALOG_TAG@@"

#! Both object names. Version-less: the pair is registered once and kept, and an upgrade
#! of this service re-applies it unchanged (a no-op to kapp). Anything that varied per
#! release would make an upgrade attempt an update the webhook rejects.
repository_name: "@@REPO_NAME@@"

#! The package this catalog serves. Named in the offerings annotation; the versions are
#! deliberately not listed, see config/repo.yml.
addon_package: "@@PKG_REF@@"
