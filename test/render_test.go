// Package test renders the AddonConfigDefinition's Go templates the way the VKS addon
// controller does, and the package's ytt the way the guest kapp-controller does, so a
// break in either surfaces here instead of after a bundle is published. The addon
// controller only reports template errors per cluster, well after install; see
// docs/verify.md.
package test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"gopkg.in/yaml.v3"
)

type outputTarget struct {
	APIVersion    string `yaml:"apiVersion"`
	Kind          string `yaml:"kind"`
	Name          string `yaml:"name"`
	Namespace     string `yaml:"namespace"`
	ReferenceType string `yaml:"referenceType"`
}

type outputResource struct {
	SupervisorNamespaceOutput *outputTarget `yaml:"supervisorNamespaceOutput"`
	TargetClusterOutput       *outputTarget `yaml:"targetClusterOutput"`
	Template                  string        `yaml:"template"`
}

func (o outputResource) target() outputTarget {
	if o.SupervisorNamespaceOutput != nil {
		return *o.SupervisorNamespaceOutput
	}
	if o.TargetClusterOutput != nil {
		return *o.TargetClusterOutput
	}
	return outputTarget{}
}

type acd struct {
	Kind string `yaml:"kind"`
	Spec struct {
		TemplateOutputResources []outputResource `yaml:"templateOutputResources"`
	} `yaml:"spec"`
}

// yttRender runs ytt over the given args and returns stdout.
func yttRender(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("ytt", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ytt %s failed: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes()
}

// renderACD renders addon/addonconfigdefinition.yml with ytt, the way `make bundle` does
// before encoding it into the Package annotation.
func renderACD(t *testing.T) acd {
	t.Helper()
	out := yttRender(t, "-f", "../addon/addonconfigdefinition.yml", "-f", "../addon/values.yml")
	var doc acd
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered ACD is not valid YAML: %v", err)
	}
	if doc.Kind != "AddonConfigDefinition" {
		t.Fatalf("expected an AddonConfigDefinition, got %q", doc.Kind)
	}
	return doc
}

// controllerFuncs approximates the function map the VKS addon controller exposes to
// AddonConfigDefinition templates: sprig plus the Helm-style toYaml/fromYaml. toJson is
// part of sprig and is what this definition relies on.
func controllerFuncs() template.FuncMap {
	funcs := sprig.TxtFuncMap()
	funcs["toYaml"] = func(v any) string {
		data, err := yaml.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.TrimSuffix(string(data), "\n")
	}
	return funcs
}

// renderTemplate evaluates one output's Go template with the context roots the addon
// controller provides.
func renderTemplate(t *testing.T, tmpl string, values, deps map[string]any) string {
	t.Helper()
	parsed, err := template.New("out").Funcs(controllerFuncs()).Parse(tmpl)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx := map[string]any{
		"Values":       values,
		"Dependencies": deps,
		"Cluster":      map[string]any{"name": "dev1-cluster"},
		"Addon":        map[string]any{"name": "bootstrap"},
	}
	var out bytes.Buffer
	if err := parsed.Execute(&out, ctx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out.String()
}

// valuesSecret returns the values.yaml the ACD emits into its output Secret for a given
// payload. This is what the addon controller wires into the guest PackageInstall.
func valuesSecret(t *testing.T, a acd, values, deps map[string]any) string {
	t.Helper()
	out := renderTemplate(t, a.Spec.TemplateOutputResources[0].Template, values, deps)

	var secret struct {
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal([]byte(out), &secret); err != nil {
		t.Fatalf("rendered Secret is not valid YAML: %v\n---\n%s", err, out)
	}
	v, ok := secret.StringData["values.yaml"]
	if !ok {
		t.Fatalf("output Secret has no stringData.values.yaml\n---\n%s", out)
	}
	return v
}

// packageRender feeds a values.yaml through the package's render files (addon/render),
// exactly as the guest kapp-controller does, and returns the resources it emits.
func packageRender(t *testing.T, valuesYAML string) []map[string]any {
	t.Helper()
	dir := t.TempDir()
	valsPath := filepath.Join(dir, "vals.yaml")
	if err := os.WriteFile(valsPath, []byte(valuesYAML), 0o600); err != nil {
		t.Fatalf("write values: %v", err)
	}
	out := yttRender(t, "-f", "../addon/render/", "--data-values-file", valsPath)

	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(out))
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

// TestOutputsAreBodyOnly guards the single most constraining API rule: a template is the
// resource body only. Leaking apiVersion, kind or metadata would malform the Secret.
func TestOutputsAreBodyOnly(t *testing.T) {
	a := renderACD(t)
	if len(a.Spec.TemplateOutputResources) == 0 {
		t.Fatal("ACD has no templateOutputResources")
	}
	for _, o := range a.Spec.TemplateOutputResources {
		out := renderTemplate(t, o.Template, map[string]any{}, nil)
		var body map[string]any
		if err := yaml.Unmarshal([]byte(out), &body); err != nil {
			t.Errorf("%s: template does not render to valid YAML: %v\n---\n%s", o.target().Kind, err, out)
			continue
		}
		for _, forbidden := range []string{"apiVersion", "kind", "metadata"} {
			if _, found := body[forbidden]; found {
				t.Errorf("%s: template sets %q, but a template is the resource body only", o.target().Kind, forbidden)
			}
		}
	}
}

// TestOutputsAreValuesSecrets pins the wiring: the addon controller marshals the ACD
// output Secret into the guest PackageInstall's values, so both outputs must be Secrets
// and at least one must be a ValuesRef.
func TestOutputsAreValuesSecrets(t *testing.T) {
	a := renderACD(t)
	valuesRef := false
	for _, o := range a.Spec.TemplateOutputResources {
		if o.target().Kind != "Secret" {
			t.Errorf("output is a %q, but the package is fed by a values Secret", o.target().Kind)
		}
		if o.target().ReferenceType == "ValuesRef" {
			valuesRef = true
		}
	}
	if !valuesRef {
		t.Error("no output Secret is a ValuesRef; nothing wires the payload into the PackageInstall")
	}
}

// TestFullRoundTrip is the end-to-end check: an AddonConfig payload is encoded by the
// ACD into a values Secret, then the package's ytt turns those values back into the
// original resources. It runs each payload source alone and all together.
func TestFullRoundTrip(t *testing.T) {
	a := renderACD(t)

	rawYAML := "# platform baseline\n" +
		"apiVersion: v1\n" +
		"kind: Namespace\n" +
		"metadata:\n" +
		"  name: team-a\n" +
		"\n" +
		"---\n" +
		"apiVersion: v1\n" +
		"kind: Secret\n" +
		"metadata:\n" +
		"  name: registry-pull\n" +
		"  namespace: team-a\n" +
		"stringData:\n" +
		"  note: \"value: with a colon\"\n"

	structured := []any{
		map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "seed", "namespace": "default"},
			"data":       map[string]any{"hello": "world"},
		},
	}

	configMap := map[string]any{
		"bootstrapConfigMap": map[string]any{
			"data": map[string]any{
				"rbac.yaml": "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata:\n  name: deployer\n  namespace: team-a\n",
			},
		},
	}

	cases := []struct {
		name    string
		values  map[string]any
		deps    map[string]any
		wantGVK []string // "kind/name"
	}{
		{
			name:    "structured only",
			values:  map[string]any{"resources": structured},
			wantGVK: []string{"ConfigMap/seed"},
		},
		{
			name:    "raw yaml only",
			values:  map[string]any{"resourcesYaml": rawYAML},
			wantGVK: []string{"Namespace/team-a", "Secret/registry-pull"},
		},
		{
			name:    "configmap only",
			deps:    configMap,
			wantGVK: []string{"RoleBinding/deployer"},
		},
		{
			name:    "all sources combined",
			values:  map[string]any{"resources": structured, "resourcesYaml": rawYAML},
			deps:    configMap,
			wantGVK: []string{"ConfigMap/seed", "Namespace/team-a", "Secret/registry-pull", "RoleBinding/deployer"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vals := valuesSecret(t, a, tc.values, tc.deps)
			docs := packageRender(t, vals)

			got := map[string]bool{}
			for _, d := range docs {
				kind, _ := d["kind"].(string)
				meta, _ := d["metadata"].(map[string]any)
				name, _ := meta["name"].(string)
				got[kind+"/"+name] = true
			}
			for _, want := range tc.wantGVK {
				if !got[want] {
					t.Errorf("payload did not round-trip to %q; rendered %v", want, keys(got))
				}
			}
		})
	}
}

// TestNoPayloadRendersEmpty confirms the nil-guard path: an AddonConfig with no payload
// produces a values Secret that renders to no resources, rather than erroring. The empty
// case is what a package's deploy has to tolerate.
func TestNoPayloadRendersEmpty(t *testing.T) {
	a := renderACD(t)
	vals := valuesSecret(t, a, map[string]any{}, nil)

	// The values Secret must carry both keys, defaulted, so the package's ytt schema is
	// satisfied and json.decode/split see empty inputs.
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(vals), &parsed); err != nil {
		t.Fatalf("values.yaml is not valid YAML: %v\n%s", err, vals)
	}
	if _, ok := parsed["resourcesJson"]; !ok {
		t.Errorf("values.yaml missing resourcesJson\n%s", vals)
	}
	if got := parsed["resourcesJson"]; got != "[]" {
		t.Errorf("resourcesJson = %v, want \"[]\" for an empty payload", got)
	}
	docs := packageRender(t, vals)
	if len(docs) != 0 {
		t.Errorf("empty payload rendered %d resources, want 0", len(docs))
	}
}

// TestResourcesJsonIsValid pins the encoding: resourcesJson must be a JSON string the
// package can decode, not a YAML structure.
func TestResourcesJsonIsValid(t *testing.T) {
	a := renderACD(t)
	vals := valuesSecret(t, a, map[string]any{
		"resources": []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}}},
	}, nil)

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(vals), &parsed); err != nil {
		t.Fatalf("values.yaml invalid: %v", err)
	}
	s, ok := parsed["resourcesJson"].(string)
	if !ok {
		t.Fatalf("resourcesJson is %T, want a JSON string", parsed["resourcesJson"])
	}
	var arr []any
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		t.Fatalf("resourcesJson is not valid JSON: %v\n%s", err, s)
	}
	if len(arr) != 1 {
		t.Errorf("decoded %d resources, want 1", len(arr))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("ytt"); err != nil {
		println("ytt not on PATH; skipping render tests")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
