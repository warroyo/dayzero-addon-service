ADDON_NAME    := dayzero
PKG_REF       := $(ADDON_NAME).kubernetes.vmware.com
ADDON_REPO    := ghcr.io/warroyo/dayzero-addon-repo

# The tag the registered AddonRepository points at, and the only one it ever points at.
# Every release re-points this tag at the newest bundle; the manager re-resolves it on its
# own (~10 min, measured) and materialises whatever package versions it now finds. That is
# what keeps a new package version from costing a new repo+install pair.
CATALOG_TAG   := stable

# An immutable snapshot tag for the same bundle, plus the Supervisor Service package
# version. Nothing registered points here: it exists so a given publish can be identified,
# re-fetched or pinned by hand later. Not a package version. A `v*` git tag sets it.
REPO_VERSION  ?= 1.1.0

# Every package version this catalog serves, oldest first. The catalog carries all of
# them at once, so a consumer moving a releaseFilter pin between two of them needs no
# Supervisor access, and older versions stay available to roll back to.
PKG_VERSIONS  := 1.0.1 1.0.2

# Released package YAML, frozen at the version it was published as. See released/README.md.
RELEASED      := released/dayzero-addon-repo

BUNDLE        := build/bundle
PKG_DIR       := $(BUNDLE)/packages/$(PKG_REF)
INSTALL_YML   := build/install/addonrepository.yml
ACD_ANNO      := addons.kubernetes.vmware.com/addon-config-definition

# The registered pair is permanent, so its name carries no version. Everything in the CR
# has to stay byte-stable as the catalog grows: the webhook rejects any update to an
# in-use AddonRepository, so a rendered manifest that changed between releases could not
# be re-applied. Hence the floating tag above and the version-less offerings annotation.
REPO_NAME     := dayzero-addon-repo-$(CATALOG_TAG)

# The Supervisor Service wrapper: the same two AddonRepository CRs, packaged for upload
# through the vCenter Services catalogue.
SVC_PKG       := dayzero-addon.fling.vsphere.vmware.com
SVC_ARTIFACT  := dayzero-addon.yml

# Which version `make render` shows.
RENDER_VERSION ?= $(lastword $(PKG_VERSIONS))

.PHONY: render freeze bundle install-manifest check push supervisor-service release test clean pkg-versions

# Show one package the way it will be materialised: the Package itself and the ACD
# decoded back out of its annotation.
render: bundle
	@echo "--- Package ($(PKG_DIR)/$(RENDER_VERSION).yml) ---"
	@cat $(PKG_DIR)/$(RENDER_VERSION).yml
	@echo "--- AddonConfigDefinition (decoded from the annotation) ---"
	@sed -n 's|^ *$(ACD_ANNO): ||p' $(PKG_DIR)/$(RENDER_VERSION).yml | base64 -d | gunzip

# Render any listed package version that has no frozen file yet, and only those. A
# released version is never re-rendered: its ACD lives inside it, so re-rendering would
# redefine a version consumers are already pinned to. gzip -n keeps the encoding
# reproducible by leaving the mtime out of the header.
freeze:
	@mkdir -p $(RELEASED) build
	@for v in $(PKG_VERSIONS); do \
		if [ ! -f $(RELEASED)/$$v.yml ]; then \
			echo "freezing $(PKG_REF).$$v from addon/"; \
			ytt -f addon/addonconfigdefinition.yml -f addon/values.yml --data-value addon_version=$$v \
				| gzip -n -c | base64 -w0 > build/acd-$$v.b64; \
			ytt -f addon/package.yml -f addon/values.yml --data-value addon_version=$$v \
				--data-value-file acd_encoded=build/acd-$$v.b64 \
				--data-value-file render_values_schema=addon/render/values-schema.yml \
				--data-value-file render=addon/render/render.yml > $(RELEASED)/$$v.yml; \
		fi; \
	done

# Stage the catalog for `kctrl package repository release`: the packages/<refName>/ tree
# kctrl expects, one Package per listed version plus the PackageMetadata, alongside the
# pkgrepo-build.yml naming the registry. kctrl adds .imgpkg/images.yml at push time.
bundle: freeze
	@rm -rf $(BUNDLE)
	@mkdir -p $(PKG_DIR)
	@sed -e 's|@@REPO@@|$(ADDON_REPO)|g' repo/pkgrepo-build.yml.tpl > $(BUNDLE)/pkgrepo-build.yml
	@ytt -f addon/metadata.yml -f addon/values.yml > $(PKG_DIR)/metadata.yml
	@for v in $(PKG_VERSIONS); do cp $(RELEASED)/$$v.yml $(PKG_DIR)/$$v.yml; done
	@echo "catalog staged at $(BUNDLE): $(PKG_REF) $(PKG_VERSIONS)"

# Render the admin-apply manifest. It is deliberately identical from one release to the
# next -- the floating tag, not the manifest, is what carries a new catalog to the
# manager. An admin applies it once; re-applying it later is a no-op rather than the
# update the webhook would reject.
install-manifest:
	@mkdir -p build/install
	@ytt -f install/addonrepository.tpl.yml -f install/values.yml \
		--data-value addon_repo_image=$(ADDON_REPO):$(CATALOG_TAG) \
		--data-value repository_name=$(REPO_NAME) \
		--data-value addon_package=$(PKG_REF) > $(INSTALL_YML)
	@echo "install manifest at $(INSTALL_YML) ($(REPO_NAME) -> $(ADDON_REPO):$(CATALOG_TAG))"

# Two gates. The bundle must serve exactly PKG_VERSIONS, or a release silently ships or
# drops a package version. And the install manifest must be free of package versions and
# of the snapshot tag: anything release-specific in there would change between releases,
# and the webhook rejects an update to an in-use AddonRepository, so an admin could never
# apply the newer one over the older.
check: bundle install-manifest
	@want=$$(printf '%s\n' $(PKG_VERSIONS) | sort | tr '\n' ' '); \
	built=$$(ls $(PKG_DIR) | sed -n 's/\.yml$$//p' | grep -v '^metadata$$' | sort | tr '\n' ' '); \
	[ "$$built" = "$$want" ] || { echo "bundle serves [$$built], PKG_VERSIONS is [$$want]"; exit 1; }; \
	grep -q '$(ADDON_REPO):$(CATALOG_TAG)' $(INSTALL_YML) || { echo "install manifest does not point at the floating tag $(CATALOG_TAG)"; exit 1; }; \
	for v in $(PKG_VERSIONS) $(REPO_VERSION); do \
		! grep -q "$$v" $(INSTALL_YML) || { echo "install manifest mentions $$v; it must be identical across releases"; exit 1; }; \
	done; \
	echo "bundle serves exactly [$$want]; install manifest is release-independent"

# Publish the catalog, the kctrl way: kbld generates the ImagesLock, imgpkg pushes the
# bundle. -t replaces kctrl's build-<TIMESTAMP> default tag. Twice, same content: the
# snapshot tag identifies this publish, and moving CATALOG_TAG onto it is what actually
# delivers the new package versions to every registered repository. The second push is
# content-addressed, so it uploads nothing and only moves the tag.
push: bundle
	kctrl package repository release -y --chdir $(BUNDLE) -v $(REPO_VERSION) -t $(REPO_VERSION)
	kctrl package repository release -y --chdir $(BUNDLE) -v $(REPO_VERSION) -t $(CATALOG_TAG)
	@echo "$(ADDON_REPO):$(CATALOG_TAG) now serves $(PKG_VERSIONS); registered repositories pick it up within ~10 minutes"

# Build the Supervisor Service package and assemble the single YAML uploaded through
# Workload Management -> Services -> Add Service. kctrl pushes the wrapper bundle, so this
# needs a registry login. The CRs it deploys are the release-independent ones, so a
# service upgrade re-applies them unchanged, which kapp treats as a no-op -- an actual
# change would be rejected, since the webhook forbids updating an in-use AddonRepository.
supervisor-service:
	@sed -e 's|@@REPO@@|$(ADDON_REPO)|g' -e 's|@@CATALOG_TAG@@|$(CATALOG_TAG)|g' \
		-e 's|@@REPO_NAME@@|$(REPO_NAME)|g' -e 's|@@PKG_REF@@|$(PKG_REF)|g' \
		supervisor-service/values.yml.tpl > supervisor-service/config/values.yml
	cd supervisor-service && kctrl package release -y -v $(REPO_VERSION) -t $(REPO_VERSION)
	@cp supervisor-service/carvel-artifacts/packages/$(SVC_PKG)/metadata.yml ./$(SVC_ARTIFACT)
	@printf '\n---\n' >> ./$(SVC_ARTIFACT)
	@cat supervisor-service/carvel-artifacts/packages/$(SVC_PKG)/package.yml >> ./$(SVC_ARTIFACT)
	@echo "supervisor service package assembled at ./$(SVC_ARTIFACT)"

# Render the AddonConfigDefinition's Go templates and validate the rendered resources
# the way the addon controller and CRD schemas will.
test:
	cd test && go vet ./... && go test ./...

# Everything a tagged release publishes: the catalog bundle, the admin-apply manifest
# and the Supervisor Service package.
release: check push supervisor-service

clean:
	rm -rf build supervisor-service/carvel-artifacts supervisor-service/config/values.yml $(SVC_ARTIFACT)

# For CI and scripts: the package versions this catalog serves.
pkg-versions:
	@echo $(PKG_VERSIONS)
