package test

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"
)

// The schema in test/schemas is extracted from the AddonConfigDefinition CRD on a live
// Supervisor (VKS 3.7), with x-kubernetes-* extensions stripped and additionalProperties
// set to false wherever the CRD does not preserve unknown fields, so a typo'd field is
// rejected rather than silently pruned. docs/plan.md has the jq filter to regenerate it.
const acdSchema = "schemas/addonconfigdefinitions.json"

// renderACDMap renders the ACD with ytt and returns it as a generic map for validation.
func renderACDMap(t *testing.T) map[string]any {
	t.Helper()
	out := yttRender(t, "-f", "../addon/addonconfigdefinition.yml", "-f", "../addon/values.yml")
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered ACD is not valid YAML: %v", err)
	}
	return doc
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

// TestACDMatchesCRDSchema validates the AddonConfigDefinition against the real CRD
// schema. It cannot be created by hand (the manager owns it), so this stands in for a
// server-side dry run.
func TestACDMatchesCRDSchema(t *testing.T) {
	doc := renderACDMap(t)
	if err := compileSchema(t, acdSchema).Validate(toJSONValue(t, doc)); err != nil {
		t.Errorf("AddonConfigDefinition does not match its CRD schema:\n%v", err)
	}
}

// TestSchemaValidationHasTeeth guards the guard: strict validation is only worth running
// if it rejects bad input.
func TestSchemaValidationHasTeeth(t *testing.T) {
	schema := compileSchema(t, acdSchema)
	doc := renderACDMap(t)

	cases := []struct {
		name   string
		mutate func(spec map[string]any)
	}{
		{
			name: "misspelled input field the API server would prune",
			mutate: func(spec map[string]any) {
				inputs := spec["templateInputResources"].([]any)
				inputs[0].(map[string]any)["inputNam"] = "typo"
			},
		},
		{
			name: "output missing its template",
			mutate: func(spec map[string]any) {
				outputs := spec["templateOutputResources"].([]any)
				delete(outputs[0].(map[string]any), "template")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var broken map[string]any
			raw, _ := json.Marshal(doc)
			if err := json.Unmarshal(raw, &broken); err != nil {
				t.Fatalf("copy: %v", err)
			}
			tc.mutate(broken["spec"].(map[string]any))
			if err := schema.Validate(toJSONValue(t, broken)); err == nil {
				t.Error("schema accepted an invalid AddonConfigDefinition; not strict enough to be worth running")
			}
		})
	}
}

// TestACDSatisfiesTheValidatingWebhook pins what the addon.validating.vmware.com webhook
// enforces and the CRD schema does not: the ACD must live in vmware-system-vks-public and
// carry the addon-name and addon-namespace labels.
func TestACDSatisfiesTheValidatingWebhook(t *testing.T) {
	const (
		addonNamespace = "vmware-system-vks-public"
		addonName      = "dayzero"
		nameLabel      = "addon.kubernetes.vmware.com/addon-name"
		nsLabel        = "addon.kubernetes.vmware.com/addon-namespace"
	)
	meta := renderACDMap(t)["metadata"].(map[string]any)

	if ns, _ := meta["namespace"].(string); ns != addonNamespace {
		t.Errorf("ACD namespace = %q, want %q", ns, addonNamespace)
	}
	labels, _ := meta["labels"].(map[string]any)
	if got, _ := labels[nameLabel].(string); got != addonName {
		t.Errorf("%s = %q, want %q", nameLabel, got, addonName)
	}
	if got, _ := labels[nsLabel].(string); got != addonNamespace {
		t.Errorf("%s = %q, want %q", nsLabel, got, addonNamespace)
	}
}

// TestPackageCarriesTheACD checks the delivery mechanism: the built Package carries the
// ACD in its addon-config-definition annotation as gzip+base64, and it decodes back to
// the same definition. This is how the manager receives our hand-written ACD.
func TestPackageCarriesTheACD(t *testing.T) {
	// Render the Package the way `make bundle` does: inject a placeholder-free encoded
	// ACD, then read the annotation back out.
	acdOut := yttRender(t, "-f", "../addon/addonconfigdefinition.yml", "-f", "../addon/values.yml")

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(acdOut)
	_ = w.Close()
	encoded := base64.StdEncoding.EncodeToString(gz.Bytes())

	pkgOut := yttRender(t,
		"-f", "../addon/package.yml", "-f", "../addon/values.yml",
		"--data-value", "acd_encoded="+encoded,
		"--data-value", "render_values_schema=x",
		"--data-value", "render=y",
	)
	var pkg map[string]any
	if err := yaml.Unmarshal(pkgOut, &pkg); err != nil {
		t.Fatalf("Package is not valid YAML: %v", err)
	}
	annos := pkg["metadata"].(map[string]any)["annotations"].(map[string]any)
	got, ok := annos["addons.kubernetes.vmware.com/addon-config-definition"].(string)
	if !ok {
		t.Fatal("Package has no addon-config-definition annotation")
	}

	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("annotation is not base64: %v", err)
	}
	r, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("annotation is not gzip: %v", err)
	}
	decoded, _ := io.ReadAll(r)

	var back map[string]any
	if err := yaml.Unmarshal(decoded, &back); err != nil {
		t.Fatalf("decoded annotation is not valid YAML: %v", err)
	}
	if back["kind"] != "AddonConfigDefinition" {
		t.Errorf("decoded annotation is a %v, want AddonConfigDefinition", back["kind"])
	}
}
