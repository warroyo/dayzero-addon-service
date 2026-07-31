#! Template for config/values.yml. `make supervisor-service` stamps the repo, the catalog
#! version and the package list from the Makefile and writes the generated file into
#! config/, which is what kctrl reads. Edit this file, not the generated one.
#@data/values-schema
---

#! Namespace the supervisor deploys this service into. Filled in by the supervisor,
#! do not edit. The AddonRepository and AddonRepositoryInstall land here (the deploy
#! rewrites their namespace), and the manager reconciles them from here.
namespace: ""

#! The published catalog bundle the manager fetches.
addon_repo_image: "@@REPO@@:@@REPO_VERSION@@"

#! The catalog release. Becomes spec.version, the repositoryVersion in the offerings
#! annotation, and the suffix on both object names. Not a package version.
repository_version: "@@REPO_VERSION@@"

#! The package versions the catalog serves, comma separated. The offerings annotation
#! has to match the bundle exactly, so this is stamped from the Makefile's PKG_VERSIONS
#! rather than typed. Overriding it without changing the bundle will fail the webhook.
addon_package: "dayzero.kubernetes.vmware.com"
addon_package_versions: "@@PKG_VERSIONS@@"
