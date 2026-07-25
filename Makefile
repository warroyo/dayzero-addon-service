PKG_NAME := bootstrap-addon.fling.vsphere.vmware.com
ARTIFACT := bootstrap-addon.yml

.PHONY: render test release clean

# Render the addon CRs exactly as the package build does.
render:
	ytt -f config

# Render the AddonConfigDefinition's Go templates the way the addon controller will.
# There is no faster loop against a real Supervisor: creating an AddonConfigDefinition
# is denied even to the vSphere administrator, so this is where template bugs get
# caught before an upload.
test:
	cd test && go vet ./... && go test ./...

# Build the Carvel package and assemble the supervisor service YAML. This is the file
# uploaded through Workload Management -> Services -> Add Service.
release:
	kctrl package release -y -v $(VERSION)
	cp carvel-artifacts/packages/$(PKG_NAME)/metadata.yml ./$(ARTIFACT)
	printf '\n---\n' >> ./$(ARTIFACT)
	cat carvel-artifacts/packages/$(PKG_NAME)/package.yml >> ./$(ARTIFACT)

clean:
	rm -rf carvel-artifacts $(ARTIFACT)
