ADDON_NAME    := dayzero
PKG_REF       := $(ADDON_NAME).kubernetes.vmware.com
ADDON_REPO    := ghcr.io/warroyo/dayzero-addon-repo
VERSION       ?= 1.0.2
BUNDLE        := build/bundle
PKG_DIR       := $(BUNDLE)/packages/$(PKG_REF)

# The Supervisor Service wrapper: the same two AddonRepository CRs, packaged for upload
# through the vCenter Services catalogue.
SVC_PKG       := dayzero-addon.fling.vsphere.vmware.com
SVC_ARTIFACT  := dayzero-addon.yml

.PHONY: render bundle push supervisor-service release test clean

# Show the addon resources the way they will be materialised: the ACD (decoded from the
# Package annotation) and the Package itself.
render: bundle
	@echo "--- Package ($(PKG_DIR)/$(VERSION).yml) ---"
	@cat $(PKG_DIR)/$(VERSION).yml
	@echo "--- AddonConfigDefinition (decoded from the annotation) ---"
	@base64 -d < build/acd.b64 | gunzip

# Assemble the addon-repository imgpkg bundle: a Carvel package repository whose one
# Package carries the ACD in its addon-config-definition annotation.
bundle:
	@rm -rf $(BUNDLE)
	@mkdir -p $(PKG_DIR) $(BUNDLE)/.imgpkg
	@ytt -f addon/addonconfigdefinition.yml -f addon/values.yml --data-value addon_version=$(VERSION) \
		| gzip -c | base64 -w0 > build/acd.b64
	@ytt -f addon/metadata.yml -f addon/values.yml --data-value addon_version=$(VERSION) > $(PKG_DIR)/metadata.yml
	@ytt -f addon/package.yml -f addon/values.yml --data-value addon_version=$(VERSION) \
		--data-value-file acd_encoded=build/acd.b64 \
		--data-value-file render_values_schema=addon/render/values-schema.yml \
		--data-value-file render=addon/render/render.yml > $(PKG_DIR)/$(VERSION).yml
	@printf 'apiVersion: imgpkg.carvel.dev/v1alpha1\nkind: ImagesLock\nimages: []\n' > $(BUNDLE)/.imgpkg/images.yml
	@echo "bundle assembled at $(BUNDLE) (empty ImagesLock: no container images)"

# Publish the addon-repository bundle. The AddonRepository fetches it from here.
push: bundle
	imgpkg push -b $(ADDON_REPO):$(VERSION) -f $(BUNDLE)

# Build the Supervisor Service package and assemble the single YAML uploaded through
# Workload Management -> Services -> Add Service. kctrl pushes the wrapper bundle, so
# this needs a registry login. The values schema is stamped from VERSION first, so the
# service can never point at a bundle version other than the one being released.
supervisor-service:
	@sed -e 's|@@REPO@@|$(ADDON_REPO)|g' -e 's|@@VERSION@@|$(VERSION)|g' \
		supervisor-service/values.yml.tpl > supervisor-service/config/values.yml
	cd supervisor-service && kctrl package release -y -v $(VERSION)
	@cp supervisor-service/carvel-artifacts/packages/$(SVC_PKG)/metadata.yml ./$(SVC_ARTIFACT)
	@printf '\n---\n' >> ./$(SVC_ARTIFACT)
	@cat supervisor-service/carvel-artifacts/packages/$(SVC_PKG)/package.yml >> ./$(SVC_ARTIFACT)
	@echo "supervisor service package assembled at ./$(SVC_ARTIFACT)"

# Render the AddonConfigDefinition's Go templates and validate the rendered resources
# the way the addon controller and CRD schemas will.
test:
	cd test && go vet ./... && go test ./...

# Everything a tagged release publishes: the addon-repository bundle and the
# Supervisor Service package.
release: push supervisor-service

clean:
	rm -rf build supervisor-service/carvel-artifacts supervisor-service/config/values.yml $(SVC_ARTIFACT)
