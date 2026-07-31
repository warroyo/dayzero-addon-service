# Template for the staged bundle's pkgrepo-build.yml. `make bundle` stamps the registry
# from the Makefile's ADDON_REPO and writes it next to the assembled packages/ tree, which
# is what `kctrl package repository release` reads. Edit this file, not the generated one.
#
# kctrl runs `kbld -f packages --imgpkg-lock-output .imgpkg/images.yml` and then pushes the
# bundle, so the ImagesLock is generated from the packages rather than hand-written. Our
# packages fetch inline and reference no images, so it comes out empty.
apiVersion: kctrl.carvel.dev/v1alpha1
kind: PackageRepositoryBuild
metadata:
  name: dayzero-addon-repo
spec:
  export:
    imgpkgBundle:
      image: "@@REPO@@"
