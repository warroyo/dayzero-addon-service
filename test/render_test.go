// Package test renders the AddonConfigDefinition's Go templates the way the VKS
// addon controller does, so template bugs surface here instead of after a vCenter
// upload. There is no faster loop against a real Supervisor: creating an
// AddonConfigDefinition is denied even to the vSphere administrator, so the only
// way to exercise the real controller is to build, upload and install the service.
package test

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"gopkg.in/yaml.v3"
)

type outputResource struct {
	TargetClusterOutput struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Name       string `yaml:"name"`
		Namespace  string `yaml:"namespace"`
		Scope      string `yaml:"scope"`
	} `yaml:"targetClusterOutput"`
	Template string `yaml:"template"`
}

type acd struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Schema struct {
			OpenAPIV3Schema struct {
				Properties map[string]struct {
					Default any `yaml:"default"`
				} `yaml:"properties"`
			} `yaml:"openAPIV3Schema"`
		} `yaml:"schema"`
		TemplateOutputResources []outputResource `yaml:"templateOutputResources"`
	} `yaml:"spec"`
}

// renderConfig runs ytt over config/ exactly as the package build does.
func renderConfig(t *testing.T) acd {
	t.Helper()

	cmd := exec.Command("ytt", "-f", "../config")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ytt -f ../config failed: %v\n%s", err, stderr.String())
	}

	dec := yaml.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var doc acd
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind == "AddonConfigDefinition" {
			return doc
		}
	}
	t.Fatal("no AddonConfigDefinition in rendered config")
	return acd{}
}

// schemaDefaults mirrors the defaulting the API server applies to AddonConfig.spec.values
// from the AddonConfigDefinition's openAPIV3Schema before the controller templates it.
func schemaDefaults(a acd) map[string]any {
	values := map[string]any{}
	for name, prop := range a.Spec.Schema.OpenAPIV3Schema.Properties {
		if prop.Default != nil {
			values[name] = prop.Default
		}
	}
	return values
}

// controllerFuncs approximates the function map the VKS addon controller exposes to
// AddonConfigDefinition templates. It is sprig plus the Helm-style YAML helpers:
// toYaml is not part of sprig, but the shipped external-secrets AddonConfigDefinition
// uses it, so the controller clearly registers it the way Helm does. The controller
// also registers at least one VMware-specific function (getRegistryAuth, used by the
// shipped helm-repo definition) which this project does not rely on.
func controllerFuncs() template.FuncMap {
	funcs := sprig.TxtFuncMap()

	funcs["toYaml"] = func(v any) string {
		data, err := yaml.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.TrimSuffix(string(data), "\n")
	}
	funcs["fromYaml"] = func(s string) map[string]any {
		out := map[string]any{}
		_ = yaml.Unmarshal([]byte(s), &out)
		return out
	}

	return funcs
}

// render evaluates one output's template with the context roots the addon controller
// provides: .Values, .Dependencies, .Cluster and .Addon.
func render(t *testing.T, tmpl string, values, deps map[string]any) string {
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

func outputFor(t *testing.T, a acd, kind string) outputResource {
	t.Helper()
	for _, o := range a.Spec.TemplateOutputResources {
		if o.TargetClusterOutput.Kind == kind {
			return o
		}
	}
	t.Fatalf("no output resource of kind %q", kind)
	return outputResource{}
}

// appPaths renders the App output and returns its inline fetch paths.
func appPaths(t *testing.T, a acd, values, deps map[string]any) map[string]string {
	t.Helper()

	out := render(t, outputFor(t, a, "App").Template, values, deps)

	var app struct {
		Spec struct {
			ServiceAccountName string `yaml:"serviceAccountName"`
			Fetch              []struct {
				Inline struct {
					Paths map[string]string `yaml:"paths"`
				} `yaml:"inline"`
			} `yaml:"fetch"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &app); err != nil {
		t.Fatalf("rendered App is not valid YAML: %v\n---\n%s", err, out)
	}
	if len(app.Spec.Fetch) == 0 {
		t.Fatalf("rendered App has no fetch stage\n---\n%s", out)
	}
	return app.Spec.Fetch[0].Inline.Paths
}

// TestOutputsAreBodyOnly guards the single most constraining API rule: a template is
// the resource body only. Leaking apiVersion, kind or metadata into one would produce
// a malformed resource in the guest cluster.
func TestOutputsAreBodyOnly(t *testing.T) {
	a := renderConfig(t)
	values := schemaDefaults(a)

	for _, o := range a.Spec.TemplateOutputResources {
		kind := o.TargetClusterOutput.Kind
		out := render(t, o.Template, values, nil)

		var body map[string]any
		if err := yaml.Unmarshal([]byte(out), &body); err != nil {
			t.Errorf("%s: template does not render to valid YAML: %v\n---\n%s", kind, err, out)
			continue
		}
		for _, forbidden := range []string{"apiVersion", "kind", "metadata"} {
			if _, found := body[forbidden]; found {
				t.Errorf("%s: template sets %q, but a template is the resource body only", kind, forbidden)
			}
		}
	}
}

// TestClusterRoleBindingUsesConfiguredRole checks the RBAC body shape (top-level
// roleRef and subjects, no spec) and that clusterRoleName is honored.
func TestClusterRoleBindingUsesConfiguredRole(t *testing.T) {
	a := renderConfig(t)

	values := schemaDefaults(a)
	if got := values["clusterRoleName"]; got != "cluster-admin" {
		t.Fatalf("expected clusterRoleName to default to cluster-admin, got %v", got)
	}

	values["clusterRoleName"] = "edit"
	out := render(t, outputFor(t, a, "ClusterRoleBinding").Template, values, nil)

	var crb struct {
		RoleRef struct {
			Kind string `yaml:"kind"`
			Name string `yaml:"name"`
		} `yaml:"roleRef"`
		Subjects []struct {
			Kind      string `yaml:"kind"`
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"subjects"`
	}
	if err := yaml.Unmarshal([]byte(out), &crb); err != nil {
		t.Fatalf("not valid YAML: %v\n---\n%s", err, out)
	}
	if crb.RoleRef.Name != "edit" {
		t.Errorf("roleRef.name = %q, want edit", crb.RoleRef.Name)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Namespace != "vks-bootstrap" {
		t.Errorf("unexpected subjects: %+v", crb.Subjects)
	}
}

// TestNoPayloadStillRenders is the nil-guard test. Every payload source is optional,
// so an AddonConfig that sets none of them must still produce a valid App with a
// non-empty inline paths map -- kapp-controller rejects a fetch with no paths.
func TestNoPayloadStillRenders(t *testing.T) {
	a := renderConfig(t)

	paths := appPaths(t, a, schemaDefaults(a), nil)
	if len(paths) == 0 {
		t.Fatal("App rendered with no inline paths; kapp-controller needs at least one")
	}
}

// TestPayloadSources covers each payload source alone and all three together. The
// embedded values must survive as parseable YAML, which is what the toJson encoding
// in the template buys.
func TestPayloadSources(t *testing.T) {
	// Deliberately awkward: leading comment, deep indentation and a blank line, the
	// kind of payload that breaks naive indent-based embedding.
	rawYAML := `# platform bootstrap
apiVersion: v1
kind: Namespace
metadata:
  name: argocd

---
apiVersion: v1
kind: Secret
metadata:
  name: repo-creds
  namespace: argocd
stringData:
  password: "s3cr3t: with colons"
`

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
				"app-of-apps.yaml": "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: root\n",
			},
		},
	}

	cases := []struct {
		name      string
		values    map[string]any
		deps      map[string]any
		wantPath  string
		wantInner string
	}{
		{
			name:      "raw yaml only",
			values:    map[string]any{"resourcesYaml": rawYAML},
			wantPath:  "10-inline.yml",
			wantInner: "s3cr3t: with colons",
		},
		{
			name:      "structured only",
			values:    map[string]any{"resources": structured},
			wantPath:  "20-structured-000.yml",
			wantInner: "hello: world",
		},
		{
			name:      "configmap only",
			deps:      configMap,
			wantPath:  "app-of-apps.yaml",
			wantInner: "kind: Application",
		},
	}

	a := renderConfig(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := schemaDefaults(a)
			for k, v := range tc.values {
				values[k] = v
			}

			paths := appPaths(t, a, values, tc.deps)

			content, found := paths[tc.wantPath]
			if !found {
				t.Fatalf("no inline path %q; got %v", tc.wantPath, keys(paths))
			}
			if !strings.Contains(content, tc.wantInner) {
				t.Errorf("path %q missing %q; got:\n%s", tc.wantPath, tc.wantInner, content)
			}

			// The payload must round-trip as YAML, not just as a string.
			dec := yaml.NewDecoder(strings.NewReader(content))
			for {
				var doc any
				if err := dec.Decode(&doc); err != nil {
					if err.Error() == "EOF" {
						break
					}
					t.Fatalf("payload at %q is not valid YAML: %v\n---\n%s", tc.wantPath, err, content)
				}
			}
		})
	}

	t.Run("all sources combined", func(t *testing.T) {
		values := schemaDefaults(a)
		values["resourcesYaml"] = rawYAML
		values["resources"] = structured

		paths := appPaths(t, a, values, configMap)

		for _, want := range []string{"10-inline.yml", "20-structured-000.yml", "app-of-apps.yaml"} {
			if _, found := paths[want]; !found {
				t.Errorf("missing inline path %q; got %v", want, keys(paths))
			}
		}
	})
}

// TestConfigMapKeysAreQuoted covers ConfigMap keys that are not safe bare YAML keys.
func TestConfigMapKeysAreQuoted(t *testing.T) {
	a := renderConfig(t)

	deps := map[string]any{
		"bootstrapConfigMap": map[string]any{
			"data": map[string]any{
				"10:weird key.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: odd\n",
			},
		},
	}

	paths := appPaths(t, a, schemaDefaults(a), deps)
	if _, found := paths["10:weird key.yaml"]; !found {
		t.Errorf("colon-bearing key did not survive; got %v", keys(paths))
	}
}

func keys(m map[string]string) []string {
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
