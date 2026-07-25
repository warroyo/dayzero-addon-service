package test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"
)

// Schemas in testdata are extracted from the CRDs on a live Supervisor (VKS 3.7) with
// two modifications: x-kubernetes-* extensions are stripped, and additionalProperties
// is set to false on every object that does not explicitly preserve unknown fields.
//
// That second change is the point. A Kubernetes API server silently *prunes* unknown
// fields in a custom resource rather than rejecting them, so a misspelled field name
// produces a quietly wrong AddonConfigDefinition rather than an error. Validating
// strictly here turns that class of typo into a test failure.
//
// Refresh with:
//
//	kubectl get crd <plural>.addons.kubernetes.vmware.com -o json | jq '...'
//
// (the full jq filter is in docs/plan.md).
var schemaFor = map[string]string{
	"AddonConfigDefinition": "schemas/addonconfigdefinitions.json",
	"Addon":                 "schemas/addons.json",
	"AddonRelease":          "schemas/addonreleases.json",
}

// renderedDocs runs ytt over config/ and returns each document as a generic map.
func renderedDocs(t *testing.T) []map[string]any {
	t.Helper()

	cmd := exec.Command("ytt", "-f", "../config")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ytt -f ../config failed: %v\n%s", err, stderr.String())
	}

	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}

func compileSchema(t *testing.T, path string) *jsonschema.Schema {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(path, bytes.NewReader(raw)); err != nil {
		t.Fatalf("add %s: %v", path, err)
	}
	schema, err := c.Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	return schema
}

// toJSONValue round-trips through JSON so the validator sees the same types the API
// server would, rather than yaml.v3's map[string]any with non-string keys.
func toJSONValue(t *testing.T, v any) any {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// TestRenderedResourcesMatchCRDSchemas validates every rendered addon resource against
// the real CRD schema. This is the closest thing to a server-side dry run available:
// creating these kinds is denied even to the vSphere administrator, and a client-side
// dry run validates nothing at all (verified: it accepts both bogus fields and invalid
// enum values).
func TestRenderedResourcesMatchCRDSchemas(t *testing.T) {
	docs := renderedDocs(t)
	if len(docs) != 3 {
		t.Fatalf("expected 3 rendered documents, got %d", len(docs))
	}

	seen := map[string]bool{}

	for _, doc := range docs {
		kind, _ := doc["kind"].(string)
		path, known := schemaFor[kind]
		if !known {
			t.Errorf("rendered an unexpected kind %q", kind)
			continue
		}
		seen[kind] = true

		if err := compileSchema(t, path).Validate(toJSONValue(t, doc)); err != nil {
			t.Errorf("%s does not match its CRD schema:\n%v", kind, err)
		}
	}

	for kind := range schemaFor {
		if !seen[kind] {
			t.Errorf("no %s was rendered", kind)
		}
	}
}

// TestSchemaValidationHasTeeth guards the guard. Strict validation is only worth
// anything if it actually rejects bad input, and the obvious alternative
// (kubectl apply --dry-run=client) turned out to accept everything.
func TestSchemaValidationHasTeeth(t *testing.T) {
	schema := compileSchema(t, schemaFor["AddonConfigDefinition"])

	var acdDoc map[string]any
	for _, doc := range renderedDocs(t) {
		if doc["kind"] == "AddonConfigDefinition" {
			acdDoc = doc
		}
	}
	if acdDoc == nil {
		t.Fatal("no AddonConfigDefinition rendered")
	}

	cases := []struct {
		name   string
		mutate func(spec map[string]any)
	}{
		{
			name: "invalid enum value",
			mutate: func(spec map[string]any) {
				outputs := spec["templateOutputResources"].([]any)
				first := outputs[0].(map[string]any)
				first["targetClusterOutput"].(map[string]any)["scope"] = "NotARealScope"
			},
		},
		{
			name: "misspelled field the API server would silently prune",
			mutate: func(spec map[string]any) {
				inputs := spec["templateInputResources"].([]any)
				inputs[0].(map[string]any)["inputNam"] = "typo"
			},
		},
		{
			name: "missing required field",
			mutate: func(spec map[string]any) {
				outputs := spec["templateOutputResources"].([]any)
				first := outputs[0].(map[string]any)
				delete(first["targetClusterOutput"].(map[string]any), "kind")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Deep copy so each case starts from the real rendered document.
			var broken map[string]any
			raw, _ := json.Marshal(acdDoc)
			if err := json.Unmarshal(raw, &broken); err != nil {
				t.Fatalf("copy: %v", err)
			}

			tc.mutate(broken["spec"].(map[string]any))

			if err := schema.Validate(toJSONValue(t, broken)); err == nil {
				t.Error("schema accepted an invalid AddonConfigDefinition; validation is not strict enough to be worth running")
			}
		})
	}
}

// TestAddonReleaseIsPackageFree pins the property the whole project rests on. If a
// future edit adds spec.package, the addon stops being pure configuration and starts
// pulling a Carvel package into the guest cluster.
func TestAddonReleaseIsPackageFree(t *testing.T) {
	for _, doc := range renderedDocs(t) {
		if doc["kind"] != "AddonRelease" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		if _, found := spec["package"]; found {
			t.Error("AddonRelease sets spec.package; this addon must stay package-free")
		}
		if _, found := spec["addonConfigDefinitionRef"]; !found {
			t.Error("AddonRelease has neither package nor addonConfigDefinitionRef and would be rejected")
		}
		return
	}
	t.Fatal("no AddonRelease rendered")
}

// TestConfigDefinitionRefsResolve checks the three resources actually point at each
// other. Names are assembled from ytt values in three separate files, so a mismatch is
// easy to introduce and would only surface as a broken install.
func TestConfigDefinitionRefsResolve(t *testing.T) {
	names := map[string]string{}
	var release map[string]any

	for _, doc := range renderedDocs(t) {
		meta := doc["metadata"].(map[string]any)
		names[doc["kind"].(string)] = meta["name"].(string)
		if doc["kind"] == "AddonRelease" {
			release = doc
		}
	}

	spec := release["spec"].(map[string]any)

	addonRef := spec["addonRef"].(map[string]any)["name"].(string)
	if addonRef != names["Addon"] {
		t.Errorf("AddonRelease.spec.addonRef.name = %q but the Addon is named %q", addonRef, names["Addon"])
	}

	acdRef := spec["addonConfigDefinitionRef"].(map[string]any)["name"].(string)
	if acdRef != names["AddonConfigDefinition"] {
		t.Errorf("AddonRelease.spec.addonConfigDefinitionRef.name = %q but the definition is named %q",
			acdRef, names["AddonConfigDefinition"])
	}

	// The AddonConfig a tenant writes must be named "<cluster>-<addon>", and the
	// ConfigMap input is discovered as "<cluster>-bootstrap". Those only line up if
	// the addon is actually called "bootstrap".
	if !strings.HasSuffix(acdRef, ".kubernetes.vmware.com."+versionOf(t, spec)) {
		t.Errorf("definition name %q does not follow the <addon>.kubernetes.vmware.com.<version> convention", acdRef)
	}
}

func versionOf(t *testing.T, spec map[string]any) string {
	t.Helper()
	v, ok := spec["version"].(string)
	if !ok {
		t.Fatal("AddonRelease has no spec.version")
	}
	return v
}
