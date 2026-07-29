ADDON_NAME    := dayzero
PKG_REF       := $(ADDON_NAME).kubernetes.vmware.com
ADDON_REPO    := ghcr.io/warroyo/dayzero-addon-repo
VERSION       ?= 1.0.0
BUNDLE        := build/bundle
PKG_DIR       := $(BUNDLE)/packages/$(PKG_REF)

.PHONY: render bundle push release test clean

# Show the addon resources the way they will be materialised: the ACD (decoded from the
# Package annotation) and the Package itself.
render: bundle
	@echo "--- Package ($(PKG_DIR)/$(VERSION).yml) ---"
	@cat $(PKG_DIR)/$(VERSION).yml
	@echo "--- AddonConfigDefinition (decoded from the annotation) ---"
	@ytt -f addon --data-value addon_version=$(VERSION) -f addon/addonconfigdefinition.yml 2>/dev/null

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

# Render the AddonConfigDefinition's Go templates and validate the rendered resources
# the way the addon controller and CRD schemas will.
test:
	cd test && go vet ./... && go test ./...

release: push

clean:
	rm -rf build
