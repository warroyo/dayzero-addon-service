load("@ytt:data", "data")

# Namespace the Addon, AddonRelease and AddonConfigDefinition are created in. Defaults
# to the namespace the Supervisor deploys this service into, which is the only place a
# Supervisor Service can put namespaced resources: kapp rewrites the namespace of
# everything in the package to that one. See docs/design.md.
def addon_namespace():
  return data.values.addon_namespace or data.values.namespace
end

# AddonConfigDefinition name, in the <addon>.kubernetes.vmware.com.<version> form the
# addon system expects. Referenced from the AddonRelease.
def acd_name():
  return "{}.kubernetes.vmware.com.{}".format(data.values.addon_name, data.values.addon_version)
end

# Labels the addon.validating.vmware.com webhook requires. addon-name must match the
# addon; addon-namespace is carried by the shipped addons and names where the addon
# lives.
def addon_labels():
  return {
    "addon.kubernetes.vmware.com/addon-name": data.values.addon_name,
    "addon.kubernetes.vmware.com/addon-namespace": addon_namespace(),
  }
end
