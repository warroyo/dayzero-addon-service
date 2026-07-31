ADDON_NAME    := dayzero
PKG_REF       := $(ADDON_NAME).kubernetes.vmware.com
ADDON_REPO    := ghcr.io/warroyo/dayzero-addon-repo

# The catalog release: the imgpkg tag, the AddonRepository's spec.version and
# repositoryVersion, and the Supervisor Service package version. Not a package version.
# A `v*` git tag sets it.
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

# A registered AddonRepository is frozen by the validating webhook, so each catalog
# release is its own repo+install pair rather than an update of the last one. Object
# names cannot carry a dot, hence the dashed suffix.
REPO_NAME     := dayzero-addon-repo-$(subst .,-,$(subst +,-,$(REPO_VERSION)))

# The Supervisor Service wrapper: the same two AddonRepository CRs, packaged for upload
# through the vCenter Services catalogue.
SVC_PKG       := dayzero-addon.fling.vsphere.vmware.com
SVC_ARTIFACT  := dayzero-addon.yml

comma         := ,
space         := $(subst x,,x x)
PKG_CSV       := $(subst $(space),$(comma),$(strip $(PKG_VERSIONS)))

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

# Render the admin-apply manifest. The package-offerings annotation is generated from
# PKG_VERSIONS, never hand-maintained: the webhook requires it to be a complete, exact
# manifest of the bundle, and it is frozen once installed.
install-manifest:
	@mkdir -p build/install
	@ytt -f install/addonrepository.tpl.yml -f install/values.yml \
		--data-value addon_repo_image=$(ADDON_REPO):$(REPO_VERSION) \
		--data-value repository_version=$(REPO_VERSION) \
		--data-value addon_package=$(PKG_REF) \
		--data-value addon_package_versions=$(PKG_CSV) > $(INSTALL_YML)
	@echo "install manifest at $(INSTALL_YML) ($(REPO_NAME))"

# Prove the offerings annotation and the bundle agree. Both come from PKG_VERSIONS, so
# they cannot drift, but a mismatch is uncorrectable after install and worth a gate.
check: bundle install-manifest
	@want=$$(printf '%s\n' $(PKG_VERSIONS) | sort | tr '\n' ' '); \
	built=$$(ls $(PKG_DIR) | sed -n 's/\.yml$$//p' | grep -v '^metadata$$' | sort | tr '\n' ' '); \
	offered=$$(grep -o '"versions":\[[^]]*\]' $(INSTALL_YML) | grep -o '[0-9][^",]*' | sort | tr '\n' ' '); \
	[ "$$built" = "$$want" ] || { echo "bundle serves [$$built], PKG_VERSIONS is [$$want]"; exit 1; }; \
	[ "$$offered" = "$$want" ] || { echo "package-offerings lists [$$offered], bundle serves [$$want]"; exit 1; }; \
	echo "package-offerings matches the bundle exactly: $$want"

# Publish the catalog, the kctrl way: kbld generates the ImagesLock, imgpkg pushes the
# bundle. -t pins the tag to the catalog version instead of kctrl's build-<TIMESTAMP>
# default, since that tag is what the AddonRepository's imageURL points at.
push: bundle
	kctrl package repository release -y --chdir $(BUNDLE) -v $(REPO_VERSION) -t $(REPO_VERSION)

# Build the Supervisor Service package and assemble the single YAML uploaded through
# Workload Management -> Services -> Add Service. kctrl pushes the wrapper bundle, so
# this needs a registry login. The values schema is stamped first, so the service can
# never point at a catalog other than the one being released.
supervisor-service:
	@sed -e 's|@@REPO@@|$(ADDON_REPO)|g' -e 's|@@REPO_VERSION@@|$(REPO_VERSION)|g' \
		-e 's|@@PKG_VERSIONS@@|$(PKG_CSV)|g' \
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
