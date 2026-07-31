package test

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// releasedDir holds one frozen Package per released version. `make bundle` copies these
// into the catalog rather than re-rendering them, because each Package carries its own
// AddonConfigDefinition: re-rendering a released version would redefine it for consumers
// already pinned to it. These tests check the frozen files are internally consistent, so
// a bad hand-edit fails here rather than on a Supervisor.
const releasedDir = "../released/dayzero-addon-repo"

type releasedPackage struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		RefName string `yaml:"refName"`
		Version string `yaml:"version"`
	} `yaml:"spec"`
}

func releasedFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(releasedDir, "*.yml"))
	if err != nil {
		t.Fatalf("glob %s: %v", releasedDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no frozen packages in %s", releasedDir)
	}
	return files
}

// TestReleasedPackagesAreSelfConsistent pins the naming the addon manager relies on: the
// filename is the version, and the Package's name, refName and version agree with it. The
// bundle's layout is packages/<refName>/<version>.yml, so a mismatch is a broken catalog.
func TestReleasedPackagesAreSelfConsistent(t *testing.T) {
	for _, path := range releasedFiles(t) {
		version := strings.TrimSuffix(filepath.Base(path), ".yml")
		t.Run(version, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var pkg releasedPackage
			if err := yaml.Unmarshal(raw, &pkg); err != nil {
				t.Fatalf("not valid YAML: %v", err)
			}
			if pkg.Kind != "Package" {
				t.Errorf("kind = %q, want Package", pkg.Kind)
			}
			if pkg.Spec.Version != version {
				t.Errorf("spec.version = %q, want %q from the filename", pkg.Spec.Version, version)
			}
			if want := pkg.Spec.RefName + "." + version; pkg.Metadata.Name != want {
				t.Errorf("metadata.name = %q, want %q", pkg.Metadata.Name, want)
			}
		})
	}
}

// TestReleasedPackagesCarryTheirACD checks that each frozen Package still carries a
// decodable AddonConfigDefinition named for its own version. This is the payload the
// manager materialises, and it is opaque base64 in the file, so nothing else would catch
// a truncated or mismatched annotation.
func TestReleasedPackagesCarryTheirACD(t *testing.T) {
	const annotation = "addons.kubernetes.vmware.com/addon-config-definition"

	for _, path := range releasedFiles(t) {
		version := strings.TrimSuffix(filepath.Base(path), ".yml")
		t.Run(version, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var pkg releasedPackage
			if err := yaml.Unmarshal(raw, &pkg); err != nil {
				t.Fatalf("not valid YAML: %v", err)
			}
			encoded, ok := pkg.Metadata.Annotations[annotation]
			if !ok {
				t.Fatalf("no %s annotation", annotation)
			}

			gz, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("annotation is not base64: %v", err)
			}
			r, err := gzip.NewReader(bytes.NewReader(gz))
			if err != nil {
				t.Fatalf("annotation is not gzip: %v", err)
			}
			decoded, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("annotation does not decompress: %v", err)
			}

			var acdDoc struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
			}
			if err := yaml.Unmarshal(decoded, &acdDoc); err != nil {
				t.Fatalf("decoded annotation is not valid YAML: %v", err)
			}
			if acdDoc.Kind != "AddonConfigDefinition" {
				t.Errorf("decoded annotation is a %q, want AddonConfigDefinition", acdDoc.Kind)
			}
			// The ACD is named for the package version it ships with, so a copied-and-
			// renamed file would still be serving the wrong definition.
			if want := pkg.Spec.RefName + "." + version; acdDoc.Metadata.Name != want {
				t.Errorf("ACD name = %q, want %q", acdDoc.Metadata.Name, want)
			}
		})
	}
}
