# Install the minimal reference instance

> **Prototype for evaluation only. Not production software, no support, no guarantees. Do not make it reachable from the internet; run it only in an internal network. Use at your own risk.**

This example runs on one amd64 Debian 13 or Ubuntu 24.04/26.04 VM. Start with
4 vCPUs, 8 GiB RAM and 80 GiB disk for two concurrent people. It is a proof of
concept, with no support or upgrade-stability promise. A clean-VM acceptance
run is still required; configuration checks alone do not prove first login.

The supplied [Compose stack](../deploy/example/compose.yaml) runs the manager,
filtered Docker proxy, filesystem provisioner, registry-only egress proxy,
Traefik, Authentik with its embedded outpost and PostgreSQL, and LiteLLM.
The runtime-image service preloads an image and exits successfully; the manager
starts one isolated runtime per person. Containers use Docker user namespaces.
Only the Docker proxy and provisioner run outside those namespaces.

Authentik is an **example OIDC-capable IdP**. Its proxy provider handles login
and gives Traefik trusted identity headers. The manager is not a generic OIDC
client: another IdP connects through an OIDC-capable authentication proxy and
configurable trusted headers. The manager derives a 64-hex internal UID from
issuer/subject only when the UID header is absent or `OC_IDENTITY_HEADER_UID=-`;
a present malformed UID is rejected. Stored account keys remain unchanged. The
[oauth2-proxy overlay](../deploy/example/compose.oauth2-proxy.example.yaml) provides
an alternate recipe. See [Bring your own OIDC provider](https://zkmkarlsruhe.github.io/agent-workspace-docs/IDENTITY/#bring-your-own-oidc-provider)
for claims, group gates, callbacks, logout, acceptance tests and the separate
existing-account migration procedure. The default example uses local Authentik
users and needs no institutional directory.

## 1. Host, DNS and Docker

Run the host commands as root on the **new VM**. Keep SSH access restricted to
the operator. During bootstrap allow HTTPS only from the operator's address at
the VM/provider firewall. Docker-published ports can bypass ordinary UFW rules;
use a provider firewall or Docker-aware filtering. Install the Docker Engine,
Buildx and Compose plugins from Docker's repository for
[Debian](https://docs.docker.com/engine/install/debian/) or
[Ubuntu](https://docs.docker.com/engine/install/ubuntu/). Do not use rootless
Docker for this example.

```sh
apt-get update
apt-get install -y ca-certificates curl git openssl xfsprogs uidmap certbot
docker version
docker compose version
```

Use internal DNS and firewall rules that restrict all published ports to the internal network.
Do not create an internet-accessible listener. Choose a domain you control, for example `apps.example.org`. Create DNS A
records for it and `*.apps.example.org` pointing to the VM. If you publish
AAAA records, IPv6 must reach this VM too. The wildcard covers the IdP at
`auth.apps.example.org` and each person's runtime hostname. Port 443 is used
for HTTPS; port 80 only redirects. Do not expose ports 4000, 4096, 8080, 9000,
9100, 5432 or the Docker API publicly.

The fixed example networks are `172.30.0.0/29` for the edge and
`10.90.0.0/16` for dynamically created person networks. They must not overlap
host, VPN or site networks. Changing them requires coordinated edits to
Compose's edge addresses/trusted proxy, the manager's `OC_USER_NET_PREFIX`,
and Squid's source ACL. This is one stack per VM, with fixed container names.

### Configure userns-remap before creating containers

This provisioner currently fixes workspace ownership at `101000:101000`.
Container root must therefore map to host `100000:100000`, and container
UID/GID 1000 to `101000:101000`. An arbitrary Docker-assigned range will fail.
Check `/etc/subuid` and `/etc/subgid`: the range 100000 through 165535 must be
free. On a new VM, remove or relocate unused overlapping allocations before
continuing; do not remap an existing installation's data.

```sh
systemctl stop docker.service docker.socket
useradd --system --no-create-home --shell /usr/sbin/nologin dockremap
# Inspect these, including any range just allocated by useradd:
cat /etc/subuid /etc/subgid
```

Edit both files so `dockremap` has exactly this non-overlapping entry:

```text
dockremap:100000:65536
```

Create `/etc/docker/daemon.json` with the following full object on this empty
VM. If it already exists, merge these settings into the existing object.
The classic image store avoids containerd image-store restrictions on remapping.

```json
{
  "userns-remap": "dockremap",
  "features": {"containerd-snapshotter": false},
  "log-driver": "local"
}
```

```sh
systemctl start docker
docker run --rm debian:trixie-slim cat /proc/self/uid_map /proc/self/gid_map
```

Both maps must show `0 100000 65536`. This follows Docker's
[user namespace setup](https://docs.docker.com/engine/security/userns-remap/).
Only `dockerproxy` and `provision` use `userns_mode: host`; do not add that
setting to the manager, runtime, or other services.

## 2. Workspace storage and helper sockets

The supported path uses XFS with `prjquota`, Linux kernel >=5.14 and at least
32 GiB for workspace data. Prefer a dedicated XFS volume mounted at
`/srv/opencode`; persist its UUID mount with `defaults,prjquota` in `/etc/fstab`.
The provisioner sets 20 GiB hard / 18 GiB soft and 2,000,000 inodes per person.
Provisioning and account deletion both depend on project IDs and quotas.

For a VM with only a system disk, the following creates a new XFS filesystem
inside a preallocated file. It consumes 32 GiB immediately. Run once, before
any workspace data exists; the guard prevents formatting an existing file.
A loop-backed filesystem is a trial setup, not a storage growth strategy.

```sh
install -d -m 0755 /var/lib/workspace /srv/opencode
test ! -e /var/lib/workspace/data.xfs && \
  fallocate -l 32G /var/lib/workspace/data.xfs && \
  mkfs.xfs /var/lib/workspace/data.xfs
```

Add this line once to `/etc/fstab`, then mount it:

```text
/var/lib/workspace/data.xfs /srv/opencode xfs loop,defaults,prjquota 0 0
```

```sh
mount /srv/opencode
findmnt -no FSTYPE,OPTIONS /srv/opencode
xfs_quota -x -c 'state -p' /srv/opencode
```

Require XFS and project quota accounting/enforcement ON. There is no supported
`NO_QUOTA` setting: an ext4 directory in place of this mount will fail first
start. Leaving out the provision socket only works with manually provisioned
users and removes the normal account-deletion path; it is not this guide's
multi-user installation.

```sh
install -d -m 0755 /srv/opencode/users /srv/opencode/base
install -d -m 0755 /srv/opencode/base/etc-opencode /srv/opencode/base/catalog
install -d -o 100000 -g 100000 -m 0750 /srv/opencode/manager
cat > /etc/tmpfiles.d/workspace.conf <<'CONF'
d /run/oc-provision 0755 root root -
d /run/oc-docker 0755 root root -
CONF
systemd-tmpfiles --create /etc/tmpfiles.d/workspace.conf
```

Ensure storage and sockets are ready before Docker restores containers on boot:

```sh
install -d /etc/systemd/system/docker.service.d
cat > /etc/systemd/system/docker.service.d/workspace.conf <<'CONF'
[Unit]
RequiresMountsFor=/srv/opencode
After=systemd-tmpfiles-setup.service
CONF
systemctl daemon-reload
```

## 3. Source, secrets and TLS

Copy this repository's source checkout/archive to `/opt/agent-workspace` on
the VM. Keep the repository layout: Compose mounts `../../instance/minimal`.
No public manager/runtime registry is assumed; both images build locally.
The example Dockerfiles do not use private registries or copy house assets.

```sh
cd /opt/agent-workspace/deploy/example
umask 077
cp .env.example .env
openssl rand -hex 32
```

Edit `.env`. Every variable is documented in [.env.example](../deploy/example/.env.example).
Generate a fresh random value separately for `OC_MASTER_KEY`, `OC_ADMIN_TOKEN`,
`AUTHENTIK_SECRET_KEY`, `POSTGRES_PASSWORD` and `LITELLM_MASTER_KEY` (prefix the
last with `sk-`). Set `OC_DOMAIN`, the persistent `OC_IDENTITY_ISSUER`, and the
upstream `MODEL_API_BASE`, `MODEL_API_KEY`, `MODEL_ID`. For a keyless local
endpoint set a nonempty dummy key. Never use `localhost` to mean the VM from
inside LiteLLM; use an address reachable from its Docker network.

LiteLLM maps `default` and `small` to the same upstream model. `MODEL_ID` uses
LiteLLM's `openai/` prefix for an OpenAI-compatible API; `MODEL_API_BASE`
includes `/v1`. The upstream must support streaming chat completions and tool
calls. Before rendering, match all three models' limits and input capabilities
in `instance/minimal/instance.yaml` to your upstream (the sample advertises
262144/131072 context and image input). Match `accounting.largest_context` and
`accounting.max_output_tokens` too. Remove image input for text-only models.
Set `values.domain` and `defaults.OC_DOMAIN` to your domain before rendering;
Compose's `OC_DOMAIN` does not replace domain text in rendered help commands.
See the [endpoint configuration](https://docs.litellm.ai/docs/providers/openai_compatible).
Only LiteLLM gets the provider credential; runtimes reach the manager's
`llm-gw:4000/v1` on their private network. The manager supplies the internal
LiteLLM key and meters requests. The daily cap is an estimate, not a billing
guarantee from the upstream provider.

A direct OpenAI-compatible model endpoint is also supported. Follow
[Connect a model provider](https://zkmkarlsruhe.github.io/agent-workspace-docs/connect-provider/) (source tree:
`docs/public/connect-provider.md`) and the commented manager
alternative in Compose. `OC_LLM_UPSTREAM_URL`/`OC_LLM_UPSTREAM_KEY` configure the
model API; `OC_MCP_UPSTREAM_URL`/`OC_MCP_UPSTREAM_KEY` configure the optional central
MCP broker. Each defaults to `LITELLM_BASE_URL`/`LITELLM_API_KEY`, so existing
installations keep working. Set `OC_MCP_UPSTREAM_URL=-` to explicitly disable MCP,
including an inherited instance URL; no MCP URL also disables it with a startup
log. `OC_LLM_UPSTREAM_FLAVOR` defaults to `litellm`; select `openai` for a direct
endpoint. `OC_LLM_UPSTREAM_KEYLESS` defaults to `false`; set `true` and omit both
model and legacy keys for a keyless server. Manager upstream bases exclude `/v1`;
a trailing `/v1` is rejected. The supported subset is chat completions, models,
embeddings and optional manager-only image generation/editing. The provider guide
documents forced fields, response-header filtering and capability requirements.
The manager does no chat failover; fallback chains require LiteLLM configuration.

Obtain a certificate with both `OC_DOMAIN` and `*.OC_DOMAIN` as SANs. For a
trial, Certbot's manual DNS challenge needs no web listener. Substitute your
actual domain and follow the prompts to create DNS TXT records:

```sh
certbot certonly --manual --preferred-challenges dns \
  -d apps.example.org -d '*.apps.example.org'
install -d -m 0755 certs
install -m 0644 /etc/letsencrypt/live/apps.example.org/fullchain.pem certs/fullchain.pem
install -o 100000 -g 100000 -m 0600 \
  /etc/letsencrypt/live/apps.example.org/privkey.pem certs/privkey.pem
```

Manual DNS certificates do not renew unattended. Before longer use, configure
your DNS provider's Certbot renewal integration and a deploy hook copying
these files and restarting Traefik. The private key is readable by remapped
Traefik root only. Use a publicly trusted certificate for this example so the
browser and IdP trust the same issuer.

### Optional HTTPS source allowlist

For an additional restriction at Traefik, merge this middleware into
`deploy/example/traefik.yaml` under `http.middlewares`. Replace the example
ranges with your operator, LAN, VPN and site egress ranges:

```yaml
    internal-only:
      ipAllowList:
        sourceRange:
          - 127.0.0.1/32
          - 192.168.0.0/16
          - 198.51.100.0/24
          - 192.0.2.0/24 # Replace with your site's public egress range.
```

Add `middlewares: [internal-only]` to both the `identity` and `outpost`
routers. Change the `workspace` router to
`middlewares: [internal-only, authenticate]`. Restrict every HTTPS router,
including the IdP and authentication callback. This checks the connecting
address; do not enable trust of arbitrary forwarded headers. Host requests
through Docker's published loopback port can arrive with the bridge gateway
as their source; use the VM's allowed address for the HTTPS check instead of
adding the entire Docker subnet. This middleware does not restrict port 80
or replace the host/provider firewall.

## 4. Build, render and start the IdP

The supplied `egress` service runs as `user: "13:13"` with `cap_drop: [ALL]`.
This matches the `proxy` account and log/cache ownership in
`ubuntu/squid:6.13-25.04_edge`, avoiding the `setgid`/`initgroups` failures
that occur when starting as root without capabilities. The shipped
`squid.conf` sets `pinger_enable off` because the optional ICMP helper cannot
open raw sockets with these capabilities dropped. No manual edit is needed.

From `deploy/example`, validate without printing secrets:

```sh
docker compose config --quiet
docker compose build manager runtime-image
docker compose pull traefik authentik authentik-worker postgres egress litellm
```

The third-party image tags in `.env.example` are bootstrap references, not
release pins. After pulling, record each image's digest, for example:

```sh
docker image inspect traefik:v3.7.15 --format '{{index .RepoDigests 0}}'
```

Replace each third-party `*_IMAGE` in `.env` with its resolved `repository@sha256:…`
reference. Keep the local manager/runtime tags for the local build; record
`docker image inspect workspace-manager:local workspace-runtime:local --format '{{.Id}}'`
with the source revision. For distribution, push the clean builds to your own
registry and set `MANAGER_IMAGE`/`RUNTIME_IMAGE` to those digests too. Inspect
all image layers before publishing; deleting a private asset in a later
layer does not remove it. No registry publication is part of these commands.

Render the mounted minimal instance into the host directory used by dynamic
runtime binds. The temporary renderer runs without network as host root so
its output is owned by root; it is not the running manager service:

For the scripted Authentik alternative below, configure its bootstrap
environment **before** the first `docker compose up`.

```sh
docker run --rm --network none --userns host --user 0:0 \
  --entrypoint /render-instance \
  -v /opt/agent-workspace/instance/minimal:/instance:ro \
  -v /srv/opencode/base/etc-opencode:/rendered \
  workspace-manager:local -instance /instance/instance.yaml -out /rendered
chmod -R a+rX /srv/opencode/base/etc-opencode
docker compose up -d postgres authentik authentik-worker traefik
docker compose logs --tail=50 authentik authentik-worker
```

If using a different manager image ref, use it in the renderer command too.
`/srv/opencode/base/catalog` stays empty. Provision does not sync any baked
catalog or prompts. Runtime bind sources are **host paths**, so changing only
the manager's container mount path is insufficient.

Open `https://auth.YOUR_DOMAIN/if/flow/initial-setup/` (including the trailing
slash), set the initial administrator password, and enter the admin interface.
Then configure:

1. Create an application with a **Proxy Provider**, name `Workspace`, slug
   `workspace`, authorization flow `default-provider-authorization-implicit-consent`.
   Select **Forward auth (domain level)**. Set **Authentication URL** to
   `https://YOUR_DOMAIN` and **Cookie domain** to `YOUR_DOMAIN` (no scheme).
2. In **Applications > Outposts**, edit the **authentik Embedded Outpost**.
   Select the Workspace application. Set `authentik_host` to
   `https://auth.YOUR_DOMAIN/`. Save and wait for the provider to load.
3. Under **Directory > Users**, create an active test user, such as `trial-user`,
   with name, email and password. Never reuse usernames for different people.
   Create the `workspace-users` group and add `trial-user` to it. The manager
   defaults `OC_REQUIRED_GROUP` to `workspace-users`, matching the oauth2-proxy
   overlay. Only members may enter; add other people deliberately.

These settings follow Authentik's [domain-level forward auth](https://docs.goauthentik.io/add-secure-apps/providers/proxy/forward_auth/)
and [embedded outpost](https://docs.goauthentik.io/add-secure-apps/outposts/embedded/)
configuration. No external outpost token or Docker-socket integration is needed.
Traefik replaces client-supplied `X-Authentik-*` headers after authentication;
the manager trusts only Traefik's fixed `172.30.0.2/32` address.
The manager removes identity headers and cookies before proxying to runtimes.

### Scripted Authentik alternative

On a fresh installation, replace the browser setup above with
[authentik-bootstrap.sh](../deploy/example/authentik-bootstrap.sh).
Run these commands as root from `deploy/example`, **before the first IdP
start**. Authentik reads bootstrap credentials only on its first startup;
putting them in `.env` alone does not pass them to the worker.

```sh
apt-get install -y python3
umask 077
printf '\nAUTHENTIK_BOOTSTRAP_PASSWORD=%s\nAUTHENTIK_BOOTSTRAP_TOKEN=%s\n' \
  "$(openssl rand -hex 32)" "$(openssl rand -hex 32)" >> .env
cat > compose.override.yaml <<'YAML'
services:
  authentik-worker:
    environment:
      AUTHENTIK_BOOTSTRAP_PASSWORD: ${AUTHENTIK_BOOTSTRAP_PASSWORD:?set bootstrap password}
      AUTHENTIK_BOOTSTRAP_TOKEN: ${AUTHENTIK_BOOTSTRAP_TOKEN:?set bootstrap token}
YAML
docker compose up -d postgres authentik authentik-worker traefik
```

Merge the worker environment into an existing override instead of replacing
it. Wait for Authentik's default blueprints and HTTPS endpoint to be ready,
then read only the two required values from the locally created `.env` and run:

```sh
OC_DOMAIN=$(sed -n 's/^OC_DOMAIN=//p' .env) \
AUTHENTIK_BOOTSTRAP_TOKEN=$(sed -n 's/^AUTHENTIK_BOOTSTRAP_TOKEN=//p' .env) \
  ./authentik-bootstrap.sh
```

The script uses the verified HTTPS API to create/update the Workspace proxy
provider, application and embedded outpost. It preserves the outpost's other
configuration and providers. It creates `workspace-users`, adds active `trial-user` to that group and stores its
generated password only in `/root/trial-user.pass` (root-owned, mode 0600).
Reruns reuse the objects and saved password; an existing user without that
file is rejected. The script reads back the settings as a self-check.
The generated token is an administrator credential. After verification, revoke
it with the API and remove both bootstrap variables from the worker override and
`.env`. Keep the initial administrator password in your password manager first.
The `.env` commands above expect unquoted domain and generated hexadecimal token
values; they do not source `.env` or export its other secrets.

```sh
OC_DOMAIN=$(sed -n 's/^OC_DOMAIN=//p' .env) \
AUTHENTIK_BOOTSTRAP_TOKEN=$(sed -n 's/^AUTHENTIK_BOOTSTRAP_TOKEN=//p' .env) \
  ./authentik-bootstrap.sh --revoke
sed -i '/^AUTHENTIK_BOOTSTRAP_\(PASSWORD\|TOKEN\)=/d' .env
```

Remove the two bootstrap environment entries from `compose.override.yaml`, then
recreate the worker with `docker compose up -d --force-recreate authentik-worker`.
Protect `.env`, which still holds other deployment secrets. See Authentik's [automated installation](https://docs.goauthentik.io/install-config/automated-install).
Browser login and file acceptance below are still required.

## 5. Login, new project, chat and download

```sh
docker compose up -d
docker compose ps -a
curl -fsS http://127.0.0.1:9100/health
```

Wait for the helper sockets and model endpoint to be ready. `runtime-image`
must show exit code 0, while the other services stay running. `/health` must
report healthy store, engine and provision checks. It also reports disk space
and the empty catalog. It does not test model availability or exercise quota
creation. An initial failure while services start is not an acceptance pass.
For the default broker path, check both LiteLLM aliases separately before opening the first project:

```sh
docker compose exec -T litellm python - <<'PY'
import json, os, urllib.request
for model in ("default", "small"):
    request = urllib.request.Request(
        "http://127.0.0.1:4000/v1/chat/completions",
        data=json.dumps({"model": model, "messages": [
            {"role": "user", "content": "Reply with the word ready."}
        ]}).encode(),
        headers={"Authorization": "Bearer " + os.environ["LITELLM_MASTER_KEY"],
                 "Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=120) as response:
        result = json.load(response)
    assert result["choices"], model
    print(model, "responded")
PY
```

These two requests incur normal upstream usage. For the direct path, use the
provider guide's contract tests and verify each configured raw model ID in the
browser. The browser test below applies to both paths and checks naming,
streaming and file tool execution.

1. In a private browser window, open `https://YOUR_DOMAIN`. The IdP must ask
   for login. Sign in as the test user, accept the minimal consent text, and
   finish the first-visit personal settings screen.
2. On **My projects**, enter this in **What should the helper do?**:
   `Create results/hello.txt containing exactly Hello from my workspace. Use
   the file editing tool, then tell me where to download it.` Click **Get
   started**. This starts the environment, derives a project name and opens
   its first chat with that instruction. Use the ordinary helper, not the
   disabled Support assistant. No shell command is needed; minimal denies
   execution by default.
3. Wait for the chat reply. Open the project's **Results** area, download
   `hello.txt`, and verify its contents. Click **All as ZIP** in that section
   and check it contains the same file. A chat reply without a real file is a
   failed acceptance run.
4. Sign out, confirm a fresh private window still requires authentication,
   then sign back in and reopen the project. Confirm the file persists.

Check the runtime's ownership and storage quota on the VM:

```sh
ls -ln /srv/opencode/users
xfs_quota -x -c 'report -p -N -h' /srv/opencode
docker ps --filter name=oc-user
```

The person directory is owned by `101000:101000`. Record the source revision,
image IDs/digests, OS, and the four results (login/project/chat/download) when
reporting a completed clean-VM trial. Only that trial completes the milestone.

## Troubleshooting and operations

- **403 / missing identity:** check the outpost's selected application,
  domain-level mode, cookie domain and authentication URL. Do not widen the
  manager's trusted CIDR or disable authentication to work around it.
- **Environment start fails:** check `docker compose logs manager provision
  dockerproxy`. Verify UID mapping, socket modes, XFS quota state and that the
  runtime image was built/pulled. Socket directories must survive reboot via
  tmpfiles. A healthy provision socket alone does not prove quotas work.
- **Chat fails / missing model:** check `docker compose logs litellm manager`.
  Verify the upstream key, `/v1` URL and tool support. Both aliases in
  `litellm.yaml` must be present. Match model limits/capabilities in
  `instance/minimal/instance.yaml` to the actual model: the sample metadata
  advertises 262144/131072 context and image input. Regenerate after editing.
- **TLS or runtime-host failure:** check wildcard DNS, SANs and the IdP's
  public URL. Do not bypass certificate verification.

Back up `/srv/opencode`, the PostgreSQL volume (or a database dump), IdP media,
`.env`, certificates and the instance together. Stop services and person
runtimes before a filesystem backup; `docker compose down` alone does not
remove the manager-created `oc-user*` containers/networks. Never run `down -v`
unless intentionally deleting the IdP database. Keep the identity issuer and
keys stable across upgrades. Stop active environments, render instance
changes again, then recreate the manager; changes are read at startup.

## What the reference leaves out

Private fonts, house templates/branding, organization-specific catalogs,
LDAP, room and bicycle booking, cloud-file connections, internal knowledge,
feedback routing, evaluation, image tools, web research, PDF workers and
media toolbox containers are not enabled. The reference runtime installs
only distro fonts and ordinary document/data tools; presentation-specific
extras and office/browser sidecars are omitted. The example builds fresh
layers and never derives from a private font-bearing image.

The committed `minimal` instance is mounted explicitly. Set `OC_INSTANCE_FILE`
to its path inside the manager (`/instance/instance.yaml` in Compose). An unset
or blank variable selects the single embedded definition marked `default: true`.
The public export contains no such flag and requires explicit selection when
multiple instances are embedded. With no flag, only a sole embedded `minimal`
can be selected automatically. Multiple flagged defaults fail initialization. A missing
or invalid explicit file always fails; it never falls back to another instance.
Keep the mount and environment variable. The disabled Support assistant is a preview and is not
part of the ordinary helper acceptance path. No changes to institutional
services or to the existing deployment are needed.

## Optional: enable an assistant

Complete the ordinary helper trial first. This example helps describe a problem and drafts text for a person to send. It has no knowledge source and must not invent institutional instructions.

### Change the reviewed definition

On the VM, open the canonical instance:

```sh
cd /opt/agent-workspace
nano instance/minimal/instance.yaml
```

Under the existing `assistants:` list, find `id: support`. Change these fields in that entry; keep its prompt, rules and entry text:

```yaml
    enabled: true
    version: 3
    consent_mode: support
    evaluation: false
    audience: []
```

Use a revision greater than the one currently in your checkout. Do not add a second `assistants:` key. Keep `project_mode: per_task`, `knowledge_sources: []`, `skills: []`, `mcp_tools: []` and `builtin_tools: [question]`. An empty audience permits every account already authorized to use this instance, so keep the IdP trial audience restricted.

Review the instance's consent text and bump its version if its meaning changes. `support` is the current schema's accepted consent mode for enabled assistants; it is not a promise of a production support service. The neutral instance has no invented wiki or service credentials.

### Render and restart

Wait for active work to finish. Stop the test person's environment using **Help**. Then run:

```sh
cd /opt/agent-workspace/deploy/example
docker run --rm --network none --userns host --user 0:0 \
  --entrypoint /render-instance \
  -v /opt/agent-workspace/instance/minimal:/instance:ro \
  -v /srv/opencode/base/etc-opencode:/rendered \
  workspace-manager:local -instance /instance/instance.yaml -out /rendered
chmod -R a+rX /srv/opencode/base/etc-opencode
docker compose up -d --force-recreate manager
```

If validation fails, fix the source definition before continuing. Do not hand-edit generated files. Use your actual manager image reference if it differs.

### Try the card

1. Sign in and open **Assistants**. Find **Support**.
2. Submit: `My spreadsheet will not open. Help me describe the problem for support.`
3. Confirm the browser enters a normal OpenCode session and the helper asks for information without requesting passwords.
4. Ask for a draft summary. Confirm it appears in chat, cites no invented source and sends nothing.
5. Submit another question from the card. In `per_task` mode this creates a new task, rather than resuming the earlier one.

A changed definition does not upgrade old project pins. Use a new project after changing the revision. To disable this example, set `enabled: false`, rerender and restart.

### Add real knowledge later

An operator must register the existing central MCP, review its tool allowlist and source instructions, then reference that source ID in `knowledge_sources`. Upstream credentials stay at the gateway. Instance enablement, operator grants and native tool policy remain required.

[The MCP guide](https://zkmkarlsruhe.github.io/agent-workspace-docs/mcp-server/) uses the application's Go MCP example and explains central gateway registration and assistant source grants. It does not establish strict read-only execution isolation. Current assistants share the person's runtime; their prompts and permissions are not a separate security boundary.

## Optional: build and connect an MCP server

The optional [hello overlay](../deploy/example/mcp-hello) supplies a locally built
streamable-HTTP service, LiteLLM registration, catalog entry and operator manifest.
Follow [the MCP guide](https://zkmkarlsruhe.github.io/agent-workspace-docs/mcp-server/) for the instance diff, rendering, Compose
commands, secret placement, opt-in and the read-only assistant variant.

`OC_INTEGRATIONS_DIR` defaults to empty (embedded manifests only). Set it to the
mounted directory to add validated `*.json` manifests without rebuilding the
manager. Invalid files or duplicate names refuse startup. `integrations.hello`
defaults to off; the shipped minimal instance stays unchanged. Disable it and
restart to revoke access. LiteLLM remains the supported MCP broker; direct remote
registration and per-user OAuth are outside this release.

The in-memory tests cover discovery, native calls, allowlist denial, read-only
annotations and revocation. A live LiteLLM/container smoke run remains required
on the trial VM.

## Uninstall the trial

These commands apply only to the dedicated trial VM. Export any results you want to keep first. Stopping services preserves data. Destroying the VM or deleting its volumes does not.

### Stop and retain the data

Stop active work and use **Help** to stop each person's environment. From a root shell on the trial VM:

```sh
cd /opt/agent-workspace/deploy/example
docker compose stop manager
docker ps -a --filter label=oc.managed=1 \
  --format 'table {{.ID}}\t{{.Names}}\t{{.Status}}'
docker network ls --filter label=oc.managed=1
```

Review the list. On this one-stack VM, these labels identify dynamically managed runtimes, toolboxes and person networks. They are not Compose services. The next commands stop/remove those containers and remove the Compose stack, while retaining host storage and named volumes:

```sh
docker ps -aq --filter label=oc.managed=1 | xargs -r docker stop
docker ps -aq --filter label=oc.managed=1 | xargs -r docker rm
docker compose down
docker network ls -q --filter label=oc.managed=1 | xargs -r docker network rm
```

If a network still has endpoints, inspect it with `docker network inspect NETWORK_NAME`. Remove only reviewed trial endpoints; do not force-remove unrelated workloads. The gateway and egress containers may have occupied the dynamic networks until Compose removed them.

Back up `/srv/opencode`, the retained PostgreSQL and IdP-media volumes, `.env`, certificates and instance definitions as one set. Preserve the identity issuer and keys. Keep services stopped while making a filesystem copy. Protect the backup as personal data and credentials.

### Delete everything

For the disposable-VM path, destroy the VM and its attached trial disks after verifying the exports. Remove internal DNS records, certificate-renewal jobs and any firewall rules created for this trial. Dispose of snapshots/backups under your chosen retention policy.

If retaining the empty VM, these steps **permanently delete the IdP database and media**:

```sh
cd /opt/agent-workspace/deploy/example
docker compose down --volumes
```

With all person containers removed, this deletes the loop-backed storage created in step 1:

```sh
umount /srv/opencode
```

Remove the exact `/var/lib/workspace/data.xfs` entry from `/etc/fstab`, then delete only the trial artifacts:

```sh
rm -- /var/lib/workspace/data.xfs
rm -- /etc/tmpfiles.d/workspace.conf
rm -- /etc/systemd/system/docker.service.d/workspace.conf
systemctl daemon-reload
```

Do not use that loop-file deletion for a dedicated XFS block volume. Erase or detach that volume using the VM/storage tooling only after verifying its identity. Delete the application and documentation copies, `.env` and certificate copies when no longer needed. Remove unused trial images individually; avoid a host-wide Docker prune.

### External data remains separate

Revoke the upstream model credential if it was dedicated to this trial. Disconnect any connected services before teardown. Local deletion does not erase provider logs, model requests, downloaded files, remote cloud files or backups. The account deletion UI and the IdP account lifecycle are also separate operations.

## Identity and logout options

The default Authentik header and logout behavior is unchanged. Environment
values override `instance.yaml` defaults; unset/empty environment values fall back
to those defaults, then built-in values. The subject header may reuse UID; the
field mappings must otherwise be valid, non-empty, distinct HTTP headers. Only
`OC_IDENTITY_HEADER_UID` accepts `-` to disable the header and always derive the UID;
the oauth2-proxy overlay uses this setting.

| Option | Default |
|---|---|
| `OC_IDENTITY_ISSUER` | `authentik` (reference `.env`: `reference-authentik`) |
| `OC_IDENTITY_SUBJECT_HEADER` | `X-Authentik-Uid` |
| `OC_IDENTITY_HEADER_UID` | `X-Authentik-Uid` |
| `OC_IDENTITY_HEADER_USERNAME` | `X-Authentik-Username` |
| `OC_IDENTITY_HEADER_EMAIL` | `X-Authentik-Email` |
| `OC_IDENTITY_HEADER_NAME` | `X-Authentik-Name` |
| `OC_IDENTITY_HEADER_GROUPS` | `X-Authentik-Groups` |
| `OC_IDENTITY_GROUPS_SEPARATOR` | `\|` |
| `OC_LOGOUT_URL` | `/outpost.goauthentik.io/sign_out` |
| `OC_TRUSTED_PROXY_CIDRS` | required; example `172.30.0.2/32` |

Groups match exactly after trimming each separated value. The Authentik Compose
example disables both gates with `OC_REQUIRED_GROUP=-` and `OC_ADMIN_GROUP=-`.
The alternate overlay defaults to `workspace-users` and `workspace-admins`;
admins need both groups. Disable administration separately from ordinary access.

For a new external-OIDC installation, complete sections 1–3 and base `.env`
preparation, then follow the [alternate recipe](https://zkmkarlsruhe.github.io/agent-workspace-docs/IDENTITY/#oauth2-proxy-recipe-new-installation)
instead of starting/bootstrapping Authentik. The overlay selects a second Traefik
file and manager header mapping; no code edits are required. Callback and sign-out
routes belong to the auth proxy and never bypass consent inside the manager.
Changing issuer/subject settings on an existing installation requires the reviewed
[migration procedure](https://zkmkarlsruhe.github.io/agent-workspace-docs/IDENTITY/#migration-of-existing-accounts), not a fresh-install switch.

## Optional live Office editing

See [Office integration](https://zkmkarlsruhe.github.io/agent-workspace-docs/OFFICE/) for the separate browser worker, image preload,
network allowlist, account connection and project permission. It is disabled by default.

## FreeCAD application

Follow the [FreeCAD operator guide](https://zkmkarlsruhe.github.io/agent-workspace-docs/freecad/) for setup, per-person Python
authority, persistent files and autosave limits before enabling the capability.
