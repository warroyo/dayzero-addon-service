load("@ytt:data", "data")

# Namespace the Addon, AddonRelease and AddonConfigDefinition are created in. Defaults
# to the namespace the Supervisor deploys this service into, which it supplies as a
# data value at install time. See docs/design.md.
def addon_namespace():
  return data.values.addon_namespace or data.values.namespace
end

# AddonConfigDefinition name, in the <addon>.kubernetes.vmware.com.<version> form the
# addon system expects. Referenced from the AddonRelease.
def acd_name():
  return "{}.kubernetes.vmware.com.{}".format(data.values.addon_name, data.values.addon_version)
end
