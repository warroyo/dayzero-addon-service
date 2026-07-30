#! Template for config/values.yml. `make supervisor-service` stamps the repo and version
#! from the Makefile and writes the generated file into config/, which is what kctrl
#! reads. Edit this file, not the generated one.
#@data/values-schema
---

#! Namespace the supervisor deploys this service into. Filled in by the supervisor,
#! do not edit. The AddonRepository and AddonRepositoryInstall land here (the deploy
#! rewrites their namespace), and the manager reconciles them from here.
namespace: ""

#! The published addon-repository bundle the manager fetches.
addon_repo_image: "@@REPO@@:@@VERSION@@"

#! Repository and package versions, stamped in step with the published bundle.
repository_version: "@@VERSION@@"
addon_package: "dayzero.kubernetes.vmware.com"
addon_package_version: "@@VERSION@@"
