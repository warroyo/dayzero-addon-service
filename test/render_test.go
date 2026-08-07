// Package test renders the AddonConfigDefinition's Go templates the way the VKS addon
// controller does, and the package's ytt the way the guest kapp-controller does, so a
// break in either surfaces here instead of after a bundle is published. The addon
// controller only reports template errors per cluster, well after install; see
// docs/verify.md.
package test

import (
	"bytes"
	"encoding/json"
	"maps"
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

// withSchemaDefaults fills in the ACD's schema defaults for keys the caller left unset,
// the way CRD defaulting does at the apiserver before the controller ever sees the
// object. resources defaults to [] and resourcesYaml to "".
func withSchemaDefaults(values map[string]any) map[string]any {
	out := map[string]any{"resources": []any{}, "resourcesYaml": ""}
	maps.Copy(out, values)
	return out
}

const (
	testClusterName = "dev1-cluster"
	testClusterUID  = "9e1c0d3a-7f42-4b18-96ad-2c5be0f4d871"
)

// withClusterCR adds the required clusterCR dependency, so cases render against the shape
// the controller actually passes: .Dependencies non-empty even when the optional ConfigMap
// is absent, which is what the hasKey guards exist for.
func withClusterCR(deps map[string]any) map[string]any {
	out := map[string]any{
		"clusterCR": map[string]any{
			"metadata": map[string]any{
				"name": testClusterName,
				"uid":  testClusterUID,
			},
		},
	}
	maps.Copy(out, deps)
	return out
}

// executeTemplate evaluates one output's Go template with the context roots the addon
// controller provides, returning any error rather than failing the test. Tests that
// assert the template *refuses* to render need the error.
func executeTemplate(tmpl string, values, deps map[string]any) (string, error) {
	parsed, err := template.New("out").Option("missingkey=error").Funcs(controllerFuncs()).Parse(tmpl)
	if err != nil {
		return "", err
	}
	ctx := map[string]any{
		"Values":       withSchemaDefaults(values),
		"Dependencies": deps,
		"Cluster":      map[string]any{"name": testClusterName},
		"Addon":        map[string]any{"name": "dayzero"},
	}
	var out bytes.Buffer
	if err := parsed.Execute(&out, ctx); err != nil {
		return "", err
	}
	return out.String(), nil
}

// renderTemplate is executeTemplate for the cases that expect success.
func renderTemplate(t *testing.T, tmpl string, values, deps map[string]any) string {
	t.Helper()
	out, err := executeTemplate(tmpl, values, deps)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

// valuesSecret returns the values.yaml the ACD emits into its output Secret for a given
// payload. This is what the addon controller wires into the guest PackageInstall.
func valuesSecret(t *testing.T, a acd, values, deps map[string]any) string {
	t.Helper()
	out := renderTemplate(t, a.Spec.TemplateOutputResources[0].Template, values, withClusterCR(deps))

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
		out := renderTemplate(t, o.Template, map[string]any{}, withClusterCR(nil))
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

// TestOutputIsOneGuestValuesSecret pins the wiring. The guest PackageInstall resolves
// secretRef in its own namespace, so the one output that matters is the Secret delivered
// into the guest package namespace -- that alone is what feeds the render, as the 85
// shipped addons that declare no supervisorNamespaceOutput at all demonstrate.
//
// A second, supervisor-side copy of the same Secret was carried until 1.0.3. It produced
// a duplicate entry in the PackageInstall's values pointing at this same guest Secret,
// and nothing read its contents. Adding one back would reintroduce two bodies that have
// to be kept identical by hand.
func TestOutputIsOneGuestValuesSecret(t *testing.T) {
	a := renderACD(t)
	if len(a.Spec.TemplateOutputResources) != 1 {
		t.Fatalf("expected exactly 1 output, got %d", len(a.Spec.TemplateOutputResources))
	}
	o := a.Spec.TemplateOutputResources[0]
	if o.TargetClusterOutput == nil {
		t.Fatal("the output is not a targetClusterOutput; nothing lands in the guest")
	}
	if got := o.target().Kind; got != "Secret" {
		t.Errorf("output is a %q, but the package is fed by a values Secret", got)
	}
	if got := o.target().Namespace; got != "vmware-system-tkg" {
		t.Errorf("output namespace is %q, want the guest package namespace", got)
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
		"dayzeroConfigMap": map[string]any{
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

// TestTokenSubstitution covers the identity tokens across every payload source. The
// ConfigMap case is the load-bearing one: it is the only case that fails if substitution
// runs before the ConfigMap concatenation.
func TestTokenSubstitution(t *testing.T) {
	a := renderACD(t)
	wantAudience := testClusterName + "-" + testClusterUID

	authenticator := func(source string) string {
		return "apiVersion: authentication.concierge.pinniped.dev/v1alpha1\n" +
			"kind: JWTAuthenticator\n" +
			"metadata:\n" +
			"  name: " + source + "\n" +
			"spec:\n" +
			"  audience: ${CLUSTER_NAME}-${CLUSTER_UID}\n"
	}

	cases := []struct {
		name   string
		values map[string]any
		deps   map[string]any
	}{
		{
			name: "structured payload",
			values: map[string]any{"resources": []any{
				map[string]any{
					"apiVersion": "authentication.concierge.pinniped.dev/v1alpha1",
					"kind":       "JWTAuthenticator",
					"metadata":   map[string]any{"name": "structured"},
					"spec":       map[string]any{"audience": "${CLUSTER_NAME}-${CLUSTER_UID}"},
				},
			}},
		},
		{
			name:   "raw yaml payload",
			values: map[string]any{"resourcesYaml": authenticator("raw")},
		},
		{
			name: "configmap payload",
			deps: map[string]any{"dayzeroConfigMap": map[string]any{
				"data": map[string]any{"auth.yaml": authenticator("configmap")},
			}},
		},
		{
			name: "all sources at once",
			values: map[string]any{
				"resourcesYaml": authenticator("raw"),
				"resources": []any{map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "${CLUSTER_NAME}-seed"},
				}},
			},
			deps: map[string]any{"dayzeroConfigMap": map[string]any{
				"data": map[string]any{"auth.yaml": authenticator("configmap")},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vals := valuesSecret(t, a, tc.values, tc.deps)
			if strings.Contains(vals, "${CLUSTER_") {
				t.Fatalf("values Secret still carries an unexpanded token:\n%s", vals)
			}
			docs := packageRender(t, vals)
			if len(docs) == 0 {
				t.Fatal("payload rendered no resources")
			}
			for _, d := range docs {
				spec, ok := d["spec"].(map[string]any)
				if !ok {
					continue
				}
				if got := spec["audience"]; got != wantAudience {
					t.Errorf("audience = %v, want %q", got, wantAudience)
				}
			}
		})
	}
}

// TestNoTokensRenderUnchanged is the regression guard for every existing consumer: a
// payload with no tokens must come out exactly as it went in.
func TestNoTokensRenderUnchanged(t *testing.T) {
	a := renderACD(t)

	raw := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: team-a\n"
	structured := []any{map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "seed", "namespace": "default"},
		"data":       map[string]any{"note": "a literal $ and a {braced} thing"},
	}}

	vals := valuesSecret(t, a, map[string]any{"resources": structured, "resourcesYaml": raw}, nil)
	docs := packageRender(t, vals)

	got := map[string]bool{}
	for _, d := range docs {
		kind, _ := d["kind"].(string)
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		got[kind+"/"+name] = true
	}
	for _, want := range []string{"Namespace/team-a", "ConfigMap/seed"} {
		if !got[want] {
			t.Errorf("token-free payload did not round-trip to %q; rendered %v", want, keys(got))
		}
	}
	for _, d := range docs {
		if data, ok := d["data"].(map[string]any); ok {
			if data["note"] != "a literal $ and a {braced} thing" {
				t.Errorf("token-free content was altered: %v", data["note"])
			}
		}
	}
}

// TestUnresolvedClusterUIDFails covers the fail-closed guard: it must fire from either
// payload string, and must not fire for payloads that never asked for a UID.
func TestUnresolvedClusterUIDFails(t *testing.T) {
	a := renderACD(t)
	tmpl := a.Spec.TemplateOutputResources[0].Template

	t.Run("raw yaml asks for the uid", func(t *testing.T) {
		_, err := executeTemplate(tmpl, map[string]any{"resourcesYaml": "audience: ${CLUSTER_UID}\n"}, nil)
		if err == nil {
			t.Fatal("template rendered an empty UID instead of failing")
		}
		if !strings.Contains(err.Error(), "did not resolve") {
			t.Errorf("error does not name the cause: %v", err)
		}
	})

	t.Run("structured payload asks for the uid", func(t *testing.T) {
		values := map[string]any{"resources": []any{
			map[string]any{"spec": map[string]any{"audience": "${CLUSTER_UID}"}},
		}}
		if _, err := executeTemplate(tmpl, values, nil); err == nil {
			t.Fatal("template rendered an empty UID instead of failing")
		}
	})

	t.Run("no token, no clusterCR, still renders", func(t *testing.T) {
		// The shape of every payload predating this feature.
		values := map[string]any{"resourcesYaml": "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: team-a\n"}
		if _, err := executeTemplate(tmpl, values, nil); err != nil {
			t.Fatalf("token-free payload was rejected: %v", err)
		}
	})
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
