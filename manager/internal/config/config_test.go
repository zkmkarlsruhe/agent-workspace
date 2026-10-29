// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func load(env map[string]string) (Config, error) {
	base := map[string]string{"OC_INSTANCE_FILE": "../../../instance/example/instance.yaml", "OC_TRUSTED_PROXY_CIDRS": "10.0.0.0/8", "OC_RUNTIME_IMAGE": "x",
		"OC_MASTER_KEY": strings.Repeat("k", 32), "LITELLM_API_KEY": "k"}
	for k, v := range env {
		base[k] = v
	}
	return Load(func(k string) string { return base[k] })
}

// TestOptionalTools: rooms and image_view are off unless configured.
func TestOptionalTools(t *testing.T) {
	c, err := load(nil)
	if err != nil || c.RoomsURL != nil || c.BikesURL != nil || c.VisionModel != "" {
		t.Fatalf("defaults: %v %+v", err, c)
	}
	if c, _ = load(map[string]string{"OC_VISION_MODEL": "-"}); c.VisionModel != "" {
		t.Errorf("vision -: %q", c.VisionModel)
	}
	c, err = load(map[string]string{"OC_ROOMS_URL": "https://rooms.example.org", "OC_BIKES_URL": "https://bikes.example.org/"})
	if err != nil || c.RoomsURL.String() != "https://rooms.example.org/" || c.BikesURL.String() != "https://bikes.example.org/" {
		t.Errorf("rooms: %v %v %v", err, c.RoomsURL, c.BikesURL)
	}
	for _, env := range []map[string]string{{"OC_ROOMS_URL": "https://rooms.example.org/"}, {"OC_ROOMS_URL": "ftp://x/", "OC_BIKES_URL": "https://b/"}} {
		if _, err := load(env); err == nil {
			t.Errorf("%v accepted", env)
		}
	}
}

func TestWatchdogSilent(t *testing.T) {
	c, err := load(nil)
	if err != nil || c.WatchdogSilent != 180*time.Second {
		t.Fatalf("default %s: %v", c.WatchdogSilent, err)
	}
	c, err = load(map[string]string{"OC_WATCHDOG_SILENT": "4m"})
	if err != nil || c.WatchdogSilent != 4*time.Minute {
		t.Fatalf("configured %s: %v", c.WatchdogSilent, err)
	}
	for _, value := range []string{"0", "-1s", "180", "bad"} {
		if _, err := load(map[string]string{"OC_WATCHDOG_SILENT": value}); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestIdentityConfig(t *testing.T) {
	c, err := load(nil)
	if err != nil || c.IdentityIssuer != "authentik" || c.IdentitySubjectHeader != "X-Authentik-Uid" {
		t.Fatalf("identity config defaults: %v", err)
	}
	c, err = load(map[string]string{"OC_IDENTITY_ISSUER": "https://id.example/", "OC_IDENTITY_SUBJECT_HEADER": "X-Subject"})
	if err != nil || c.IdentityIssuer != "https://id.example/" || c.IdentitySubjectHeader != "X-Subject" {
		t.Fatalf("identity config overrides: %v", err)
	}
}

func TestInstanceOverrides(t *testing.T) {
	c, err := load(map[string]string{"OC_INSTANCE_FILE": "../../../instance/minimal/instance.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Domain != "apps.example.org" || c.AdminGroup != "" || c.SearXNGURL != "" || len(c.ImageModels) != 0 || c.LDAPBaseDN != "" {
		t.Fatalf("minimal inherited production defaults: %+v", c)
	}
	c, err = load(map[string]string{"OC_INSTANCE_FILE": "../../../instance/minimal/instance.yaml", "OC_DOMAIN": "workspace.example.net", "OC_ADMIN_GROUP": "admins", "OC_LDAP_BASE_DN": "dc=example,dc=net"})
	if err != nil || c.Domain != "workspace.example.net" || c.AdminGroup != "admins" || c.LDAPBaseDN != "dc=example,dc=net" {
		t.Fatalf("legacy overrides lost: %v", err)
	}
	if _, err := load(map[string]string{"OC_INSTANCE_FILE": "/missing/instance.yaml"}); err == nil {
		t.Fatal("missing explicit instance must not fall back to Workspace")
	}
}

func TestInstanceSelectionFallback(t *testing.T) {
	for _, file := range []string{"", " \t"} {
		c, err := load(map[string]string{"OC_INSTANCE_FILE": file})
		if instance.Current().Default {
			if err != nil || !c.Instance.Default || c.Domain != instance.Value("domain") {
				t.Fatalf("private default requires environment changes: %v", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "OC_INSTANCE_FILE") {
			t.Fatalf("unflagged public default accepted: %v", err)
		}
	}
}

func TestDirectoryValueRequiredWhenEnabled(t *testing.T) {
	_, err := load(map[string]string{
		"OC_INSTANCE_FILE": "../../../instance/minimal/instance.yaml",
		"OC_LDAP_URL":      "ldap://directory.example.org",
	})
	if err == nil || !strings.Contains(err.Error(), "directory_url") {
		t.Fatalf("directory enabled without its URL: %v", err)
	}
}

func TestSecurityConfig(t *testing.T) {
	c, err := load(nil)
	if err != nil || c.NetGatewayMode != "isolated" || c.SearchChargeEUR != 0.005 || len(c.FetchDenyHosts) != 0 {
		t.Fatalf("defaults: mode=%q charge=%v hosts=%v err=%v", c.NetGatewayMode, c.SearchChargeEUR, c.FetchDenyHosts, err)
	}
	c, err = load(map[string]string{"OC_NET_GATEWAY_MODE": "host", "OC_SEARCH_CHARGE_EUR": "0", "OC_FETCH_DENY_HOSTS": " rooms.test, example.org, "})
	if err != nil || c.NetGatewayMode != "" || c.SearchChargeEUR != 0 || strings.Join(c.FetchDenyHosts, ",") != "rooms.test,example.org" {
		t.Fatalf("overrides: mode=%q charge=%v hosts=%v err=%v", c.NetGatewayMode, c.SearchChargeEUR, c.FetchDenyHosts, err)
	}
	for _, value := range []string{"-1", "NaN", "+Inf", "bad"} {
		if _, err := load(map[string]string{"OC_SEARCH_CHARGE_EUR": value}); err == nil {
			t.Errorf("accepted charge %q", value)
		}
	}
	if _, err := load(map[string]string{"OC_NET_GATEWAY_MODE": "nat"}); err == nil {
		t.Fatal("accepted unsupported gateway mode")
	}
}

func TestGatewayLimits(t *testing.T) {
	c, err := load(nil)
	if err != nil || c.ImageDecodeParallel != 4 || c.PageCacheMB != 64 || c.LLMMaxBodyMB != 32 || c.AdminToken != "" {
		t.Fatalf("gateway defaults: %+v, %v", c, err)
	}
	c, err = load(map[string]string{"OC_IMAGE_DECODE_PARALLEL": "0", "OC_PAGE_CACHE_MB": "0", "OC_LLM_MAX_BODY_MB": "0", "OC_ADMIN_TOKEN": "secret"})
	if err != nil || c.ImageDecodeParallel != 0 || c.PageCacheMB != 0 || c.LLMMaxBodyMB != 0 || c.AdminToken != "secret" {
		t.Fatalf("gateway overrides: %+v, %v", c, err)
	}
	for _, key := range []string{"OC_IMAGE_DECODE_PARALLEL", "OC_PAGE_CACHE_MB", "OC_LLM_MAX_BODY_MB"} {
		for _, value := range []string{"-1", "bad", "8796093022208"} {
			if _, err := load(map[string]string{key: value}); err == nil {
				t.Errorf("accepted %s=%s", key, value)
			}
		}
	}
}

func TestReapParallelAndUploadLimit(t *testing.T) {
	c, err := load(nil)
	if err != nil || c.ReapParallel != 4 || c.UploadMaxBytes != 2<<30 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = load(map[string]string{"OC_REAP_PARALLEL": "1", "OC_UPLOAD_MAX_MB": "32"})
	if err != nil || c.ReapParallel != 1 || c.UploadMaxBytes != 32<<20 {
		t.Fatalf("overrides: %+v %v", c, err)
	}
	for _, key := range []string{"OC_REAP_PARALLEL", "OC_UPLOAD_MAX_MB"} {
		for _, bad := range []string{"0", "-1", "invalid", "9223372036854775808"} {
			if _, err := load(map[string]string{key: bad}); err == nil {
				t.Fatalf("accepted %s=%s", key, bad)
			}
		}
	}
	if _, err := load(map[string]string{"OC_UPLOAD_MAX_MB": "8796093022208"}); err == nil {
		t.Fatal("byte overflow accepted")
	}
}

func TestFetchCharge(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
	}{{"", 0.002}, {"0", 0}, {"0.01", 0.01}} {
		c, err := load(map[string]string{"OC_FETCH_CHARGE_EUR": tc.value})
		if err != nil || c.FetchChargeEUR != tc.want {
			t.Fatalf("charge %q: %g, %v", tc.value, c.FetchChargeEUR, err)
		}
	}
	for _, value := range []string{"-1", "NaN", "+Inf", "bad"} {
		if _, err := load(map[string]string{"OC_FETCH_CHARGE_EUR": value}); err == nil {
			t.Errorf("accepted charge %q", value)
		}
	}
}

func TestAccountingConfig(t *testing.T) {
	c, err := load(map[string]string{"OC_INSTANCE_FILE": "../../../instance/minimal/instance.yaml"})
	if err != nil || c.Accounting.UpstreamCostCurrency != "none" || c.Accounting.USDPerEUR != 0 || c.Accounting.ReserveEUR() != .3 || c.Accounting.EmbeddingEURPerMillion != 0 {
		t.Fatalf("minimal: %+v %v", c.Accounting, err)
	}
	c, err = load(map[string]string{"OC_INSTANCE_FILE": "../../../instance/minimal/instance.yaml", "OC_UPSTREAM_COST_CURRENCY": "USD", "OC_USD_PER_EUR": "1.25"})
	if err != nil || c.Accounting.UpstreamCostCurrency != "USD" || c.Accounting.USDPerEUR != 1.25 {
		t.Fatalf("override: %+v %v", c.Accounting, err)
	}
	if c.Instance.Accounting.UpstreamCostCurrency != "USD" || c.Instance.Accounting.USDPerEUR != 1.25 || !strings.Contains(c.Instance.CopyText("pricing", "en"), "Conversion rate: 1.25 USD per EUR") {
		t.Fatal("effective accounting overrides missing from instance copy")
	}
	c, err = load(map[string]string{"OC_UPSTREAM_COST_CURRENCY": "none"})
	if err != nil || c.Accounting.UpstreamCostCurrency != "none" {
		t.Fatalf("off: %+v %v", c.Accounting, err)
	}
	for _, env := range []map[string]string{
		{"OC_UPSTREAM_COST_CURRENCY": "GBP"},
		{"OC_UPSTREAM_COST_CURRENCY": "USD", "OC_USD_PER_EUR": "0"},
		{"OC_USD_PER_EUR": "NaN"},
		{"OC_USD_PER_EUR": "-1"},
		{"OC_USD_PER_EUR": "bad"},
	} {
		if _, err := load(env); err == nil {
			t.Fatalf("accepted %v", env)
		}
	}
}

func TestAccountingRateFromEnvironment(t *testing.T) {
	b, err := os.ReadFile("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "instance.yaml")
	if err := os.WriteFile(file, []byte(strings.Replace(string(b), "upstream_cost_currency: none", "upstream_cost_currency: USD", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := load(map[string]string{"OC_INSTANCE_FILE": file, "OC_USD_PER_EUR": "1.25"})
	if err != nil || c.Accounting.UpstreamCostCurrency != "USD" || c.Accounting.USDPerEUR != 1.25 {
		t.Fatalf("%+v %v", c.Accounting, err)
	}
	if _, err := load(map[string]string{"OC_INSTANCE_FILE": file}); err == nil {
		t.Fatal("USD accepted without effective rate")
	}
}

func TestIdentityMappingConfig(t *testing.T) {
	c, err := load(nil)
	if err != nil || c.IdentityHeaders.UID != "X-Authentik-Uid" || c.IdentityHeaders.Username != "X-Authentik-Username" ||
		c.IdentityHeaders.Email != "X-Authentik-Email" || c.IdentityHeaders.Name != "X-Authentik-Name" || c.IdentityHeaders.Groups != "X-Authentik-Groups" ||
		c.IdentityGroupsSeparator != "|" || c.LogoutURL != DefaultLogoutURL {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = load(map[string]string{"OC_IDENTITY_HEADER_UID": "X-User", "OC_IDENTITY_HEADER_USERNAME": "X-Login",
		"OC_IDENTITY_HEADER_EMAIL": "X-Mail", "OC_IDENTITY_HEADER_NAME": "X-Name", "OC_IDENTITY_HEADER_GROUPS": "X-Groups",
		"OC_IDENTITY_SUBJECT_HEADER": "X-User", "OC_IDENTITY_GROUPS_SEPARATOR": ",", "OC_LOGOUT_URL": "/oauth2/sign_out"})
	if err != nil || c.IdentityHeaders.UID != "X-User" || c.IdentityHeaders.Username != "X-Login" || c.IdentityHeaders.Email != "X-Mail" ||
		c.IdentityHeaders.Name != "X-Name" || c.IdentityHeaders.Groups != "X-Groups" || c.IdentityGroupsSeparator != "," || c.LogoutURL != "/oauth2/sign_out" {
		t.Fatalf("overrides: %+v %v", c, err)
	}
	c, err = load(map[string]string{"OC_IDENTITY_HEADER_UID": "-", "OC_IDENTITY_SUBJECT_HEADER": "X-Subject"})
	if err != nil || c.IdentityHeaders.UID != "-" {
		t.Fatalf("disabled UID mapping: %+v %v", c.IdentityHeaders, err)
	}
	for _, key := range []string{"OC_IDENTITY_HEADER_UID", "OC_IDENTITY_HEADER_USERNAME", "OC_IDENTITY_HEADER_EMAIL", "OC_IDENTITY_HEADER_NAME", "OC_IDENTITY_HEADER_GROUPS", "OC_IDENTITY_SUBJECT_HEADER"} {
		if key != "OC_IDENTITY_HEADER_UID" {
			if _, err := load(map[string]string{key: "-"}); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("accepted disabled required header %s: %v", key, err)
			}
		}
		for _, bad := range []string{" ", "X Bad", "X:Bad", "X-Bad\r\n", "X-Üser"} {
			if _, err := load(map[string]string{key: bad}); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("accepted %s=%q: %v", key, bad, err)
			}
		}
	}
	if _, err := load(map[string]string{"OC_IDENTITY_HEADER_NAME": "x-authentik-email"}); err == nil {
		t.Fatal("duplicate headers accepted")
	}
	for _, bad := range []string{"\n", "\x00"} {
		if _, err := load(map[string]string{"OC_IDENTITY_GROUPS_SEPARATOR": bad}); err == nil {
			t.Errorf("accepted separator %q", bad)
		}
	}
	for _, bad := range []string{"//evil.example/", "javascript:alert(1)", "relative", "https://user:pass@example.org/", "/\\evil.example/", "/%"} {
		if _, err := load(map[string]string{"OC_LOGOUT_URL": bad}); err == nil {
			t.Errorf("accepted logout URL %q", bad)
		}
	}
	if _, err := load(map[string]string{"OC_LOGOUT_URL": "https://id.example/logout?return=/"}); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamAliases(t *testing.T) {
	c, err := load(map[string]string{"LITELLM_BASE_URL": "https://legacy.example", "LITELLM_API_KEY": "legacy"})
	if err != nil || c.LLMUpstreamURL != c.LiteLLMBaseURL || c.MCPUpstreamURL != c.LiteLLMBaseURL ||
		c.LLMUpstreamKey != "legacy" || c.MCPUpstreamKey != "legacy" || c.LLMUpstreamKeyless || c.LLMUpstreamFlavor != "litellm" {
		t.Fatalf("legacy defaults: %+v %v", c, err)
	}
	c, err = load(map[string]string{"OC_LLM_UPSTREAM_URL": "https://model.example/api", "OC_LLM_UPSTREAM_KEY": "model",
		"OC_MCP_UPSTREAM_URL": "https://mcp.example", "OC_MCP_UPSTREAM_KEY": "mcp", "OC_LLM_UPSTREAM_FLAVOR": "openai"})
	if err != nil || c.LLMUpstreamURL != "https://model.example/api" || c.MCPUpstreamURL != "https://mcp.example" ||
		c.LLMUpstreamKey != "model" || c.MCPUpstreamKey != "mcp" || c.LLMUpstreamFlavor != "openai" {
		t.Fatalf("split overrides: %+v %v", c, err)
	}
	if c, err := load(map[string]string{"OC_MCP_UPSTREAM_URL": "-"}); err != nil || c.MCPUpstreamURL != "" {
		t.Fatalf("explicit MCP disable: %+v %v", c, err)
	}
}

func TestUpstreamKeylessAndDisabledMCP(t *testing.T) {
	b, err := os.ReadFile("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "instance.yaml")
	b = []byte(strings.ReplaceAll(string(b), "  LITELLM_BASE_URL: https://models.example.org\n", ""))
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"OC_INSTANCE_FILE": file, "OC_LLM_UPSTREAM_URL": "http://models.local:8000", "LITELLM_API_KEY": "",
		"OC_LLM_UPSTREAM_KEYLESS": "true"}
	c, err := load(env)
	if err != nil || !c.LLMUpstreamKeyless || c.LLMUpstreamKey != "" || c.MCPUpstreamURL != "" {
		t.Fatalf("keyless, no MCP: %+v %v", c, err)
	}
	for _, value := range []string{"", "false", "invalid"} {
		env["OC_LLM_UPSTREAM_KEYLESS"] = value
		if _, err := load(env); err == nil || !strings.Contains(err.Error(), "OC_LLM_UPSTREAM_KEYLESS") {
			t.Fatalf("accepted keyless=%q: %v", value, err)
		}
	}
	env["OC_LLM_UPSTREAM_KEYLESS"], env["OC_LLM_UPSTREAM_URL"] = "true", ""
	if _, err := load(env); err == nil || !strings.Contains(err.Error(), "OC_LLM_UPSTREAM_URL") {
		t.Fatalf("accepted missing model URL: %v", err)
	}
}

func TestUpstreamURLAndFlavorValidation(t *testing.T) {
	for _, key := range []string{"OC_LLM_UPSTREAM_URL", "OC_MCP_UPSTREAM_URL", "LITELLM_BASE_URL"} {
		for _, value := range []string{"https://models.example/v1", "https://models.example/api/v1/", "https://models.example/%76%31//"} {
			if _, err := load(map[string]string{key: value}); err == nil || !strings.Contains(err.Error(), "root-base convention") {
				t.Errorf("accepted %s=%s: %v", key, value, err)
			}
		}
		for _, value := range []string{"/relative", "ftp://models.example", "https://u:secret@models.example", "https://models.example?key=secret", "https://models.example#fragment"} {
			if _, err := load(map[string]string{key: value}); err == nil {
				t.Errorf("accepted %s=%s", key, value)
			}
		}
	}
	if _, err := load(map[string]string{"OC_LLM_UPSTREAM_FLAVOR": "unsupported"}); err == nil || !strings.Contains(err.Error(), "OC_LLM_UPSTREAM_FLAVOR") {
		t.Fatalf("accepted unsupported flavor: %v", err)
	}
}

func TestIntegrationsDirectoryOption(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(file, []byte(`{"schema":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(map[string]string{"OC_INTEGRATIONS_DIR": dir}); err == nil || !strings.Contains(err.Error(), file) {
		t.Fatalf("startup error: %v", err)
	}
	if _, err := load(map[string]string{"OC_INTEGRATIONS_DIR": ""}); err != nil {
		t.Fatalf("off: %v", err)
	}
	if _, err := load(map[string]string{"OC_INTEGRATIONS_DIR": "../../../deploy/example/mcp-hello/integrations"}); err != nil {
		t.Fatalf("valid: %v", err)
	}
}

func TestDayStartTimezones(t *testing.T) {
	for _, tc := range []struct{ zone, instant, want string }{
		{"UTC", "2026-03-29T23:30:00Z", "2026-03-29T00:00:00Z"},
		{"Europe/Berlin", "2026-03-29T23:30:00Z", "2026-03-29T22:00:00Z"},
		{"America/New_York", "2026-03-29T23:30:00Z", "2026-03-29T04:00:00Z"},
		{"Europe/Berlin", "2026-10-25T12:00:00Z", "2026-10-24T22:00:00Z"},
		{"America/New_York", "2026-11-01T12:00:00Z", "2026-11-01T04:00:00Z"},
	} {
		t.Run(tc.zone+tc.instant, func(t *testing.T) {
			c, err := load(map[string]string{"OC_INSTANCE_FILE": "../../../instance/minimal/instance.yaml", "OC_TIMEZONE": tc.zone})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(instance.Select(c.Instance))
			now, _ := time.Parse(time.RFC3339, tc.instant)
			if got := DayStart(now).UTC().Format(time.RFC3339); got != tc.want {
				t.Fatalf("midnight = %s, want %s", got, tc.want)
			}
		})
	}
	for _, bad := range []string{"Local", "not/a-zone", "../UTC"} {
		if _, err := load(map[string]string{"OC_TIMEZONE": bad}); err == nil {
			t.Fatalf("accepted timezone override %q", bad)
		}
	}
}
