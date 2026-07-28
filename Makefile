PKG_NAME := bootstrap-addon.fling.vsphere.vmware.com
ARTIFACT := bootstrap-addon.yml

.PHONY: render test release clean

# Inspect the addon CRs as the package build renders them.
render:
	@ytt -f config

# Render the AddonConfigDefinition's Go templates the way the addon controller will.
test:
	cd test && go vet ./... && go test ./...

# Build the bundle and assemble bootstrap-addon.yml, the file uploaded through
# Workload Management -> Services -> Add Service.
release:
	kctrl package release -y -v $(VERSION)
	cp carvel-artifacts/packages/$(PKG_NAME)/metadata.yml ./$(ARTIFACT)
	printf '\n---\n' >> ./$(ARTIFACT)
	cat carvel-artifacts/packages/$(PKG_NAME)/package.yml >> ./$(ARTIFACT)

clean:
	rm -rf carvel-artifacts $(ARTIFACT)
