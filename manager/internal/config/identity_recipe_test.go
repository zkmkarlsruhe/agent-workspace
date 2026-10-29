// SPDX-License-Identifier: MIT

package config

import (
	"bytes"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"go.yaml.in/yaml/v3"
)

// Exercise the shipped mapping and edge template with oauth2-proxy response fixtures.
func TestOAuth2ProxyRecipe(t *testing.T) {
	read := func(name string, value any) {
		t.Helper()
		b, err := os.ReadFile("../../../deploy/example/" + name)
		if err != nil {
			t.Fatal(err)
		}
		tmpl, err := template.New(name).Funcs(template.FuncMap{
			"env":     func(string) string { return "workspace.example.org" },
			"replace": func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
		}).Parse(string(b))
		if err != nil {
			t.Fatal(err)
		}
		var rendered bytes.Buffer
		if err := tmpl.Execute(&rendered, nil); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(rendered.Bytes(), value); err != nil {
			t.Fatal(err)
		}
	}
	var compose struct {
		Services map[string]struct {
			Environment map[string]string
			Command     []string
			Volumes     []string
			Profiles    []string
		}
	}
	read("compose.oauth2-proxy.example.yaml", &compose)
	env := map[string]string{"OC_IDENTITY_ISSUER": "test-oidc", "OC_REQUIRED_GROUP": "workspace-users"}
	for k, v := range compose.Services["manager"].Environment {
		if strings.HasPrefix(k, "OC_IDENTITY_") && !strings.Contains(v, "${") {
			env[k] = v
		}
	}
	c, err := load(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.IdentityHeaders.UID != "-" || compose.Services["manager"].Environment["OC_ADMIN_GROUP"] != "${OC_ADMIN_GROUP:-workspace-admins}" ||
		compose.Services["manager"].Environment["OC_LOGOUT_URL"] != "${OC_LOGOUT_URL:-/oauth2/sign_out}" {
		t.Fatal("role/logout defaults changed")
	}
	for _, flag := range []string{"--provider=oidc", "--upstream=static://202", "--reverse-proxy=true", "--set-xauthrequest=true", "--scope=openid email profile groups"} {
		if !slices.Contains(compose.Services["oauth2-proxy"].Command, flag) {
			t.Errorf("missing %s", flag)
		}
	}
	if !slices.Contains(compose.Services["traefik"].Volumes, "./traefik.oauth2-proxy.yaml:/etc/traefik/dynamic.yaml:ro") {
		t.Fatal("alternate edge config not mounted")
	}
	for _, name := range []string{"postgres", "authentik", "authentik-worker"} {
		if !slices.Contains(compose.Services[name].Profiles, "authentik") {
			t.Errorf("%s enabled in alternate stack", name)
		}
	}
	var edge struct {
		HTTP struct {
			Routers map[string]struct {
				Rule        string
				Priority    int
				Service     string
				Middlewares []string
			}
			Middlewares map[string]struct {
				ForwardAuth struct {
					Address                  string
					TrustForwardHeader       bool     `yaml:"trustForwardHeader"`
					AuthRequestHeaders       []string `yaml:"authRequestHeaders"`
					AuthResponseHeaders      []string `yaml:"authResponseHeaders"`
					AuthResponseHeadersRegex string   `yaml:"authResponseHeadersRegex"`
				} `yaml:"forwardAuth"`
			}
		}
	}
	read("traefik.oauth2-proxy.yaml", &edge)
	forward := edge.HTTP.Middlewares["authenticate"].ForwardAuth
	if forward.Address != "http://oauth2-proxy:4180/" || forward.TrustForwardHeader ||
		!slices.Equal(forward.AuthRequestHeaders, []string{"Cookie"}) {
		t.Fatal("unsafe forward-auth configuration")
	}
	callback := edge.HTTP.Routers["oauth2"]
	if !strings.Contains(callback.Rule, "PathPrefix(`/oauth2/`)") || callback.Service != "oauth2-proxy" ||
		callback.Priority <= edge.HTTP.Routers["workspace"].Priority || len(callback.Middlewares) != 0 {
		t.Fatal("callback intercepted")
	}
	scrub := regexp.MustCompile(forward.AuthResponseHeadersRegex)
	for _, name := range []string{c.IdentityHeaders.UID, c.IdentityHeaders.Username, c.IdentityHeaders.Email, c.IdentityHeaders.Name, c.IdentityHeaders.Groups, c.IdentitySubjectHeader, "x-authentik-uid"} {
		if name != "-" && !scrub.MatchString(name) {
			t.Errorf("client header not stripped: %s", name)
		}
	}
	response := map[string]string{"X-Auth-Request-User": "subject-42", "X-Auth-Request-Preferred-Username": "alice", "X-Auth-Request-Email": "alice@example.org", "X-Auth-Request-Groups": "workspace-users,workspace-admins"}
	r := httptest.NewRequest("GET", "https://workspace.example.org/", nil)
	r.RemoteAddr = "172.30.0.2:1234"
	for _, name := range forward.AuthResponseHeaders {
		value, ok := response[name]
		if !ok {
			t.Fatalf("unexpected response header: %s", name)
		}
		r.Header.Set(name, value)
	}
	checker := identity.Checker{Trusted: []netip.Prefix{netip.MustParsePrefix("172.30.0.2/32")}, Headers: c.IdentityHeaders,
		Issuer: c.IdentityIssuer, SubjectHeader: c.IdentitySubjectHeader, GroupsSeparator: c.IdentityGroupsSeparator, RequiredGroup: c.RequiredGroup}
	id, err := checker.FromRequest(r)
	if err != nil || !identity.ValidUID(id.UID) || id.Subject != "subject-42" || id.Username != "alice" || id.Email != "alice@example.org" || id.Name != "" ||
		!slices.Equal(id.Groups, []string{"workspace-users", "workspace-admins"}) {
		t.Fatalf("recipe identity: %+v %v", id, err)
	}
	r.Header.Set("X-Auth-Request-Groups", "workspace-admins")
	if _, err := checker.FromRequest(r); err != identity.ErrNoGroup {
		t.Fatalf("admin group bypasses required group: %v", err)
	}
}

func TestIdentityMappingEmptyDefaults(t *testing.T) {
	for _, key := range []string{"OC_IDENTITY_HEADER_UID", "OC_IDENTITY_HEADER_USERNAME", "OC_IDENTITY_HEADER_EMAIL", "OC_IDENTITY_HEADER_NAME", "OC_IDENTITY_HEADER_GROUPS", "OC_IDENTITY_SUBJECT_HEADER", "OC_IDENTITY_GROUPS_SEPARATOR"} {
		t.Run(key, func(t *testing.T) {
			data, err := os.ReadFile("../../../instance/minimal/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var definition map[string]any
			if err := yaml.Unmarshal(data, &definition); err != nil {
				t.Fatal(err)
			}
			definition["defaults"].(map[string]any)[key] = ""
			data, err = yaml.Marshal(definition)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "instance.yaml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := load(map[string]string{"OC_INSTANCE_FILE": path}); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("empty %s accepted: %v", key, err)
			}
		})
	}
}
