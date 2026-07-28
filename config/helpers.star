load("@ytt:data", "data")

# AddonConfigDefinition name, in the <addon>.kubernetes.vmware.com.<version> form the
# addon system expects. Referenced from the AddonRelease.
def acd_name():
  return "{}.kubernetes.vmware.com.{}".format(data.values.addon_name, data.values.addon_version)
end
