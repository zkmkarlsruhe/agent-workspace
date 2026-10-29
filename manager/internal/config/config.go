// SPDX-License-Identifier: MIT

// Package config reads instance defaults and transitional environment overrides.
// The full list with defaults is documented in docs/INSTALL.md.
package config

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

type Config struct {
	Instance   *instance.Definition
	Accounting instance.Accounting

	EdgeAddr   string // OC_EDGE_ADDR: portal and <slug>.<domain> proxy, only reachable from traefik
	LLMGWAddr  string // OC_LLMGW_ADDR: llm-gw listener, reached from the user networks
	AdminAddr  string // OC_ADMIN_ADDR: /health
	AdminToken string // OC_ADMIN_TOKEN: bearer token for admin listener /admin/*; empty disables all /admin/* requests

	Domain        string         // OC_DOMAIN
	TrustedProxy  []netip.Prefix // OC_TRUSTED_PROXY_CIDRS: only these direct peers may supply identity headers
	RequiredGroup string         // OC_REQUIRED_GROUP: "-" disables the group check
	AdminGroup    string         // OC_ADMIN_GROUP: may open /admin (evaluation); "-" disables the area

	IdentityHeaders         identity.Headers // OC_IDENTITY_HEADER_{UID,USERNAME,EMAIL,NAME,GROUPS}
	IdentityGroupsSeparator string           // OC_IDENTITY_GROUPS_SEPARATOR: default |
	LogoutURL               string           // OC_LOGOUT_URL
	IdentityIssuer          string           // OC_IDENTITY_ISSUER: fixed namespace, independent of provider URLs
	IdentitySubjectHeader   string           // OC_IDENTITY_SUBJECT_HEADER

	DBPath    string // OC_DB_PATH
	UsersRoot string // OC_USERS_ROOT: host path holding <uid64>/{workspace,config,data,state}
	// OC_PROVISION_SOCKET: oc-provision unix socket; empty = the person's dir must already exist
	ProvisionSocket string

	RuntimeImage     string // OC_RUNTIME_IMAGE
	AdminConfig      string // OC_ADMIN_CONFIG: host dir or volume name mounted read-only at /etc/opencode; empty = none
	ManagerContainer string // OC_MANAGER_CONTAINER: own container name, joined to every user network as llm-gw
	EgressContainer  string // OC_EGRESS_CONTAINER: squid, joined to every user network as egress
	NetGatewayMode   string // OC_NET_GATEWAY_MODE: isolated (default, Docker 28+) or host (legacy: the host owns .1 on every bridge); stored as the Docker option value, "" = host
	UserNetPrefix    string // OC_USER_NET_PREFIX: first two octets; slot n gets <prefix>.<n>.0/24

	MasterKey []byte // OC_MASTER_KEY: >= 32 bytes, derives the per-start OpenCode passwords

	IdleTimeout    time.Duration // OC_IDLE_TIMEOUT
	TabCap         time.Duration // OC_TAB_CAP
	DrainCap       time.Duration // OC_DRAIN_CAP
	MaxLifetime    time.Duration // OC_MAX_LIFETIME
	StartTimeout   time.Duration // OC_START_TIMEOUT
	ReapInterval   time.Duration // OC_REAP_INTERVAL
	ReapParallel   int           // OC_REAP_PARALLEL: concurrent per-person reap workers; default 4, 1 = sequential
	UploadMaxBytes int64         // OC_UPLOAD_MAX_MB: portal upload limit in MiB; default 2048
	WatchdogSilent time.Duration // OC_WATCHDOG_SILENT
	MaxRunning     int           // OC_MAX_RUNNING: concurrently running user containers; 0 = no limit

	FetchChargeEUR  float64 // OC_FETCH_CHARGE_EUR: per uncached fetch, default 0.002; 0 = no charge
	SearchChargeEUR float64 // OC_SEARCH_CHARGE_EUR: per uncached search, default 0.005; 0 = no charge
	CostCapDayEUR   float64 // OC_COST_CAP_DAY_EUR: per-person LLM spend per Berlin day; 0 = no cap

	LiteLLMBaseURL string // LITELLM_BASE_URL
	LiteLLMAPIKey  string // LITELLM_API_KEY

	LLMUpstreamURL     string // OC_LLM_UPSTREAM_URL: root base; falls back to LITELLM_BASE_URL
	LLMUpstreamKey     string // OC_LLM_UPSTREAM_KEY: falls back to LITELLM_API_KEY
	LLMUpstreamKeyless bool   // OC_LLM_UPSTREAM_KEYLESS: default false; permits an empty key
	LLMUpstreamFlavor  string // OC_LLM_UPSTREAM_FLAVOR: litellm (default) or openai
	MCPUpstreamURL     string // OC_MCP_UPSTREAM_URL: root base; legacy fallback; "-" disables
	MCPUpstreamKey     string // OC_MCP_UPSTREAM_KEY: falls back to LITELLM_API_KEY

	LLMMaxBodyMB        int // OC_LLM_MAX_BODY_MB: buffered request limit in MiB, default 32; 0 = unlimited
	ImageDecodeParallel int // OC_IMAGE_DECODE_PARALLEL: concurrent image decodes/scales, default 4; 0 = unlimited
	PageCacheMB         int // OC_PAGE_CACHE_MB: page-cache payload limit in MiB, default 64; 0 = count-only legacy

	FetchDenyHosts []string // OC_FETCH_DENY_HOSTS: extra blocked hosts and suffixes, comma-separated; empty = none
	SearXNGURL     string   // OC_SEARXNG_URL: SearXNG for the Workspace-MCP tool web_search
	PDFURL         string   // OC_PDF_URL: Gotenberg for Markdown to PDF; "" = feature off
	// OC_VISION_MODEL: LiteLLM alias with image input for the Workspace-MCP tool
	// image_view, must be in the models of the LITELLM_API_KEY key; "" or "-" = tool off
	VisionModel string
	// OC_IMAGE_MODELS: LiteLLM image aliases for image_generate in fallback
	// order, comma-separated, EU first; "-" = tool off
	ImageModels []string
	// OC_IMAGE_EDIT_MODEL: alias for /v1/images/edits (image_generate source_image); "" = no editing
	ImageEditModel string

	// OC_ROOMS_URL (house timeline and MRBS, e.g. https://rooms.example.org/) and
	// OC_BIKES_URL (MRBS bikes, e.g. https://bikes.example.org/) for the Workspace-MCP
	// tools house_events, rooms_free, bikes_free, my_bookings (D61); both or
	// neither, nil = tools off
	RoomsURL, BikesURL *url.URL

	// House lists the house services a person can connect in the portal
	// (Verbindungen). Today one entry from OC_NEXTCLOUD_URL; empty = feature off.
	House []HouseService

	// OC_CATALOG_DIR: host dir of deploy/catalog, read by the manager at the same
	// path and mounted read-only at /opt/workspace/catalog; "" = no catalog, all tools on
	CatalogDir string

	// Workspace directory for the Profile facts (Department, Role, Phone, Room).
	// OC_LDAP_URL "" = facts only from Authentik. Read-only service bind, manager env only.
	LDAPURL      string // OC_LDAP_URL, e.g. ldap://ldap.example.org:389
	LDAPBaseDN   string // OC_LDAP_BASE_DN
	LDAPBindDN   string // OC_LDAP_BIND_DN; "" = anonymous bind
	LDAPPassword string // OC_LDAP_BIND_PASSWORD
}

const DefaultLogoutURL = "/outpost.goauthentik.io/sign_out"

var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// Load reads the environment. getenv is os.Getenv outside tests.
func Load(getenv func(string) string) (Config, error) {
	d, err := instance.LoadWithIntegrations(strings.TrimSpace(getenv("OC_INSTANCE_FILE")), strings.TrimSpace(getenv("OC_INTEGRATIONS_DIR")))
	if err != nil {
		return Config{}, err
	}
	if zone := strings.TrimSpace(getenv("OC_TIMEZONE")); zone != "" {
		d.Timezone = zone
	}
	var errs []error
	str := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		if v, ok := d.Defaults[k]; ok {
			return v
		}
		return def
	}
	dur := func(k string, def time.Duration) time.Duration {
		v := getenv(k)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: invalid duration %q", k, v))
			return def
		}
		return d
	}
	nonNegative := func(k string, def int) int {
		v, err := strconv.Atoi(str(k, strconv.Itoa(def)))
		if err != nil || v < 0 || int64(v) > math.MaxInt64>>20 {
			errs = append(errs, fmt.Errorf("%s must be a non-negative integer no greater than %d", k, int64(math.MaxInt64)>>20))
			return def
		}
		return v
	}
	host, _ := os.Hostname()
	c := Config{
		Instance:         d,
		EdgeAddr:         str("OC_EDGE_ADDR", ":8080"),
		LLMGWAddr:        str("OC_LLMGW_ADDR", ":4000"),
		AdminAddr:        str("OC_ADMIN_ADDR", ":9100"),
		Domain:           str("OC_DOMAIN", ""),
		RequiredGroup:    str("OC_REQUIRED_GROUP", ""),
		AdminGroup:       str("OC_ADMIN_GROUP", ""),
		DBPath:           str("OC_DB_PATH", "/srv/opencode/manager/manager.db"),
		UsersRoot:        str("OC_USERS_ROOT", "/srv/opencode/users"),
		ProvisionSocket:  str("OC_PROVISION_SOCKET", ""),
		RuntimeImage:     str("OC_RUNTIME_IMAGE", ""),
		AdminConfig:      str("OC_ADMIN_CONFIG", ""),
		ManagerContainer: str("OC_MANAGER_CONTAINER", host),
		EgressContainer:  str("OC_EGRESS_CONTAINER", "egress"),
		UserNetPrefix:    str("OC_USER_NET_PREFIX", "10.90"),
		NetGatewayMode:   str("OC_NET_GATEWAY_MODE", "isolated"),
		MasterKey:        []byte(getenv("OC_MASTER_KEY")),
		IdleTimeout:      dur("OC_IDLE_TIMEOUT", 20*time.Minute),
		TabCap:           dur("OC_TAB_CAP", time.Hour),
		DrainCap:         dur("OC_DRAIN_CAP", 2*time.Hour),
		MaxLifetime:      dur("OC_MAX_LIFETIME", 24*time.Hour),
		StartTimeout:     dur("OC_START_TIMEOUT", 20*time.Second),
		ReapInterval:     dur("OC_REAP_INTERVAL", time.Minute),
		WatchdogSilent:   dur("OC_WATCHDOG_SILENT", 180*time.Second),
		LiteLLMBaseURL:   str("LITELLM_BASE_URL", ""),
		LiteLLMAPIKey:    str("LITELLM_API_KEY", ""),
		SearXNGURL:       str("OC_SEARXNG_URL", ""),
		PDFURL:           str("OC_PDF_URL", ""),
		VisionModel:      str("OC_VISION_MODEL", ""),
		CatalogDir:       str("OC_CATALOG_DIR", ""),
		LDAPURL:          str("OC_LDAP_URL", ""),
		LDAPBaseDN:       str("OC_LDAP_BASE_DN", ""),
		LDAPBindDN:       str("OC_LDAP_BIND_DN", ""),
		LDAPPassword:     getenv("OC_LDAP_BIND_PASSWORD"),

		AdminToken:          getenv("OC_ADMIN_TOKEN"),
		LLMMaxBodyMB:        nonNegative("OC_LLM_MAX_BODY_MB", 32),
		ImageDecodeParallel: nonNegative("OC_IMAGE_DECODE_PARALLEL", 4),
		PageCacheMB:         nonNegative("OC_PAGE_CACHE_MB", 64),

		LogoutURL:      str("OC_LOGOUT_URL", DefaultLogoutURL),
		IdentityIssuer: str("OC_IDENTITY_ISSUER", ""),
	}
	c.LLMUpstreamURL = str("OC_LLM_UPSTREAM_URL", c.LiteLLMBaseURL)
	c.LLMUpstreamKey = str("OC_LLM_UPSTREAM_KEY", c.LiteLLMAPIKey)
	c.MCPUpstreamURL = str("OC_MCP_UPSTREAM_URL", c.LiteLLMBaseURL)
	c.MCPUpstreamKey = str("OC_MCP_UPSTREAM_KEY", c.LiteLLMAPIKey)
	if c.MCPUpstreamURL == "-" {
		c.MCPUpstreamURL = ""
	}
	c.LLMUpstreamFlavor = str("OC_LLM_UPSTREAM_FLAVOR", "litellm")
	if c.LLMUpstreamFlavor != "litellm" && c.LLMUpstreamFlavor != "openai" {
		errs = append(errs, errors.New("OC_LLM_UPSTREAM_FLAVOR must be litellm or openai"))
	}
	c.LLMUpstreamKeyless, err = strconv.ParseBool(str("OC_LLM_UPSTREAM_KEYLESS", "false"))
	if err != nil {
		errs = append(errs, errors.New("OC_LLM_UPSTREAM_KEYLESS must be a boolean"))
	}
	for key, base := range map[string]string{"OC_LLM_UPSTREAM_URL": c.LLMUpstreamURL, "OC_MCP_UPSTREAM_URL": c.MCPUpstreamURL} {
		if base == "" && key == "OC_MCP_UPSTREAM_URL" {
			continue
		}
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			errs = append(errs, fmt.Errorf("%s must be an absolute HTTP(S) root-base URL without credentials, query or fragment", key))
		} else if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
			errs = append(errs, fmt.Errorf("%s must use the root-base convention: remove the trailing /v1; the manager appends /v1/... for models and /mcp/ for MCP", key))
		}
	}
	// Header names and separators must not be silently trimmed into different values.
	raw := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		if v, ok := d.Defaults[key]; ok {
			return v
		}
		return def
	}
	c.IdentityHeaders = (identity.Headers{}).WithDefaults()
	seen := map[string]bool{}
	for _, field := range []struct {
		key   string
		value *string
	}{
		{"OC_IDENTITY_HEADER_UID", &c.IdentityHeaders.UID},
		{"OC_IDENTITY_HEADER_USERNAME", &c.IdentityHeaders.Username},
		{"OC_IDENTITY_HEADER_EMAIL", &c.IdentityHeaders.Email},
		{"OC_IDENTITY_HEADER_NAME", &c.IdentityHeaders.Name},
		{"OC_IDENTITY_HEADER_GROUPS", &c.IdentityHeaders.Groups},
	} {
		*field.value = raw(field.key, *field.value)
		name := strings.ToLower(*field.value)
		if field.key == "OC_IDENTITY_HEADER_UID" && name == "-" {
			continue // No UID header: derive from the validated issuer and subject.
		}
		if name == "-" || !headerName.MatchString(name) || seen[name] {
			errs = append(errs, fmt.Errorf("%s must be a valid, non-empty, distinct HTTP header name", field.key))
		}
		seen[name] = true
	}
	// The subject may deliberately reuse a mapped header (the default is UID).
	c.IdentitySubjectHeader = raw("OC_IDENTITY_SUBJECT_HEADER", "X-Authentik-Uid")
	if c.IdentitySubjectHeader == "-" || !headerName.MatchString(c.IdentitySubjectHeader) {
		errs = append(errs, errors.New("OC_IDENTITY_SUBJECT_HEADER must be a valid, non-empty HTTP header name"))
	}
	c.IdentityGroupsSeparator = raw("OC_IDENTITY_GROUPS_SEPARATOR", "|")
	if c.IdentityGroupsSeparator == "" || strings.ContainsAny(c.IdentityGroupsSeparator, "\r\n\x00") {
		errs = append(errs, errors.New("OC_IDENTITY_GROUPS_SEPARATOR must be non-empty without CR, LF or NUL"))
	}
	logout, logoutErr := url.Parse(c.LogoutURL)
	if logoutErr != nil || logout.User != nil || strings.ContainsAny(c.LogoutURL, "\r\n\\") ||
		!((logout.Scheme == "https" || logout.Scheme == "http") && logout.Host != "" ||
			logout.Scheme == "" && logout.Host == "" && strings.HasPrefix(c.LogoutURL, "/") && !strings.HasPrefix(c.LogoutURL, "//")) {
		errs = append(errs, errors.New("OC_LOGOUT_URL must be an absolute HTTP(S) URL or a local absolute path"))
	}
	c.Accounting = d.Accounting
	c.Accounting.UpstreamCostCurrency = str("OC_UPSTREAM_COST_CURRENCY", d.Accounting.UpstreamCostCurrency)
	c.Accounting.USDPerEUR, err = strconv.ParseFloat(str("OC_USD_PER_EUR", strconv.FormatFloat(d.Accounting.USDPerEUR, 'g', -1, 64)), 64)
	if err != nil {
		errs = append(errs, errors.New("OC_USD_PER_EUR must be a number"))
	}
	if err := c.Accounting.Validate(); err != nil {
		errs = append(errs, err)
	}
	// An instance may leave the exchange rate for the environment to supply.
	if c.Accounting.UpstreamCostCurrency == "USD" && c.Accounting.USDPerEUR <= 0 {
		errs = append(errs, errors.New("OC_USD_PER_EUR or accounting.usd_per_eur must be positive for USD"))
	}
	c.ReapParallel, err = strconv.Atoi(str("OC_REAP_PARALLEL", "4"))
	if err != nil || c.ReapParallel < 1 {
		errs = append(errs, errors.New("OC_REAP_PARALLEL must be a positive integer"))
	}
	uploadMB, err := strconv.ParseInt(str("OC_UPLOAD_MAX_MB", "2048"), 10, 64)
	if err != nil || uploadMB < 1 || uploadMB > math.MaxInt64/(1<<20) {
		errs = append(errs, errors.New("OC_UPLOAD_MAX_MB must be a positive integer fitting in bytes"))
	} else {
		c.UploadMaxBytes = uploadMB << 20
	}
	switch c.NetGatewayMode {
	case "isolated":
	case "host":
		c.NetGatewayMode = ""
	default:
		errs = append(errs, errors.New("OC_NET_GATEWAY_MODE must be isolated or host"))
	}
	c.SearchChargeEUR, err = strconv.ParseFloat(str("OC_SEARCH_CHARGE_EUR", strconv.FormatFloat(d.Accounting.SearchEUR, 'g', -1, 64)), 64)
	if err != nil || c.SearchChargeEUR < 0 || math.IsNaN(c.SearchChargeEUR) || math.IsInf(c.SearchChargeEUR, 0) {
		errs = append(errs, errors.New("OC_SEARCH_CHARGE_EUR must be a finite non-negative number"))
	}
	c.FetchChargeEUR, err = strconv.ParseFloat(str("OC_FETCH_CHARGE_EUR", strconv.FormatFloat(d.Accounting.FetchEUR, 'g', -1, 64)), 64)
	if err != nil || c.FetchChargeEUR < 0 || math.IsNaN(c.FetchChargeEUR) || math.IsInf(c.FetchChargeEUR, 0) {
		errs = append(errs, errors.New("OC_FETCH_CHARGE_EUR must be a finite non-negative number"))
	}
	for _, h := range strings.Split(str("OC_FETCH_DENY_HOSTS", ""), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.FetchDenyHosts = append(c.FetchDenyHosts, h)
		}
	}
	c.MaxRunning, err = strconv.Atoi(str("OC_MAX_RUNNING", "25"))
	if err != nil || c.MaxRunning < 0 {
		errs = append(errs, fmt.Errorf("OC_MAX_RUNNING: %q", getenv("OC_MAX_RUNNING")))
	}
	c.CostCapDayEUR, err = strconv.ParseFloat(str("OC_COST_CAP_DAY_EUR", "5.00"), 64)
	if err != nil || c.CostCapDayEUR < 0 || math.IsNaN(c.CostCapDayEUR) || math.IsInf(c.CostCapDayEUR, 0) {
		errs = append(errs, fmt.Errorf("OC_COST_CAP_DAY_EUR: %q", getenv("OC_COST_CAP_DAY_EUR")))
	}
	if v := str("OC_NEXTCLOUD_URL", ""); v != "" {
		u, err := url.Parse(strings.TrimRight(v, "/"))
		if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" {
			errs = append(errs, fmt.Errorf("OC_NEXTCLOUD_URL: %q", v))
		} else {
			c.House = append(c.House, HouseService{ID: "nextcloud", Title: "Nextcloud", URL: u})
		}
	}
	for k, dst := range map[string]**url.URL{"OC_ROOMS_URL": &c.RoomsURL, "OC_BIKES_URL": &c.BikesURL} {
		if v := str(k, ""); v != "" {
			u, err := url.Parse(strings.TrimRight(v, "/") + "/")
			if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" {
				errs = append(errs, fmt.Errorf("%s: %q", k, v))
			} else {
				*dst = u
			}
		}
	}
	if (str("OC_ROOMS_URL", "") == "") != (str("OC_BIKES_URL", "") == "") {
		errs = append(errs, errors.New("OC_ROOMS_URL and OC_BIKES_URL: set both or neither"))
	}
	if c.CatalogDir != "" && !strings.HasPrefix(c.CatalogDir, "/") {
		errs = append(errs, errors.New("OC_CATALOG_DIR must be an absolute host path"))
	}
	c.ImageEditModel = str("OC_IMAGE_EDIT_MODEL", "")
	if v := str("OC_IMAGE_MODELS", ""); v != "-" {
		for _, m := range strings.Split(v, ",") {
			if m = strings.TrimSpace(m); m != "" {
				c.ImageModels = append(c.ImageModels, m)
			}
		}
	}
	if c.RequiredGroup == "-" {
		c.RequiredGroup = "" // "-" disables the group check
	}
	if c.VisionModel == "-" {
		c.VisionModel = ""
	}
	if c.AdminGroup == "-" {
		c.AdminGroup = ""
	}
	for _, s := range strings.Split(getenv("OC_TRUSTED_PROXY_CIDRS"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("OC_TRUSTED_PROXY_CIDRS: %w", err))
			continue
		}
		c.TrustedProxy = append(c.TrustedProxy, p.Masked())
	}
	if len(c.TrustedProxy) == 0 {
		errs = append(errs, errors.New("OC_TRUSTED_PROXY_CIDRS is required"))
	}
	if c.RuntimeImage == "" {
		errs = append(errs, errors.New("OC_RUNTIME_IMAGE is required"))
	}
	if len(c.MasterKey) < 32 {
		errs = append(errs, errors.New("OC_MASTER_KEY must be at least 32 bytes"))
	}
	if c.LLMUpstreamKey == "" && !c.LLMUpstreamKeyless {
		errs = append(errs, errors.New("OC_LLM_UPSTREAM_KEY (or LITELLM_API_KEY) is required unless OC_LLM_UPSTREAM_KEYLESS=true"))
	}
	if _, err := netip.ParsePrefix(c.UserNetPrefix + ".0.0/16"); err != nil {
		errs = append(errs, fmt.Errorf("OC_USER_NET_PREFIX: %w", err))
	}
	// User-facing instance copy must reflect the effective accounting overrides too.
	c.Accounting.SearchEUR, c.Accounting.FetchEUR = c.SearchChargeEUR, c.FetchChargeEUR
	d.Accounting = c.Accounting
	// Keep rendered contacts in sync with the legacy endpoint overrides.
	d.Values["domain"] = c.Domain
	for key, raw := range map[string]string{"api_host": c.LLMUpstreamURL, "nextcloud_host": str("OC_NEXTCLOUD_URL", ""), "rooms_host": str("OC_ROOMS_URL", ""), "bikes_host": str("OC_BIKES_URL", "")} {
		if raw != "" {
			u, err := url.Parse(raw)
			if err == nil && u.Hostname() != "" {
				d.Values[key] = u.Hostname()
			}
		}
	}
	if c.LDAPURL != "" && d.Values["directory_url"] == "" {
		errs = append(errs, errors.New("instance: directory_url is required when OC_LDAP_URL is enabled"))
	}
	if err := d.Validate(); err != nil {
		errs = append(errs, err)
	}
	return c, errors.Join(errs...)
}

// DayStart is midnight in the configured timezone: the day of the cost cap.
func DayStart(t time.Time) time.Time {
	location := instance.Location()
	y, m, d := t.In(location).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, location)
}

// HouseService is one connectable house system (docs/adr/README.md). ID is the key
// in the store and the portal URL; every entry is a Nextcloud for now.
type HouseService struct {
	ID, Title string
	URL       *url.URL // without trailing slash, e.g. https://cloud.example.org
}

// Retention rules (portal copy in hilfe.html, papierkorb.html and start.html says 30 days).
const (
	// ProjectIdleTrash: a project without activity this long goes to the trash.
	ProjectIdleTrash = 30 * 24 * time.Hour
	// TrashRetention: a trash item stays restorable this long, then it is purged.
	TrashRetention = 30 * 24 * time.Hour
)

// Fixed addresses inside every user network <prefix>.<slot>.0/24 (same layout as the phase-1 compose).
const (
	HostEgress  = 2  // squid, alias egress
	HostManager = 3  // the manager, alias llm-gw
	HostUser    = 10 // the person's OpenCode container
	MaxSlot     = 254
)

// SlotNetwork is the docker network name of a slot.
func SlotNetwork(slot int) string { return fmt.Sprintf("oc-u%d", slot) }

// SlotContainer is the container name of a slot.
func SlotContainer(slot int) string { return fmt.Sprintf("oc-user%d", slot) }

// ToolContainer is the name of a slot's toolbox container (D60); toolbox is a catalog id.
func ToolContainer(slot int, toolbox string) string {
	return fmt.Sprintf("oc-user%d-tb-%s", slot, toolbox)
}

// SlotSubnet returns <prefix>.<slot>.0/24.
func (c Config) SlotSubnet(slot int) netip.Prefix {
	return netip.MustParsePrefix(fmt.Sprintf("%s.%d.0/24", c.UserNetPrefix, slot))
}

// SlotAddr returns <prefix>.<slot>.<host>.
func (c Config) SlotAddr(slot, host int) netip.Addr {
	return netip.MustParseAddr(fmt.Sprintf("%s.%d.%d", c.UserNetPrefix, slot, host))
}

// SlotOf maps a source address on a user network back to its slot. Only the
// person's own container address counts, so egress or anything else attached
// to the same network never acts as the person.
func (c Config) SlotOf(a netip.Addr) (int, bool) {
	a = a.Unmap()
	if !a.Is4() || !netip.MustParsePrefix(c.UserNetPrefix+".0.0/16").Contains(a) {
		return 0, false
	}
	b := a.As4()
	if b[2] < 1 || b[2] > MaxSlot || b[3] != HostUser {
		return 0, false
	}
	return int(b[2]), true
}
