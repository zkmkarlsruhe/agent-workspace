#!/bin/sh
# Run as root on the VM after Authentik is ready. Credentials come from the environment.
set -eu
: "${OC_DOMAIN:?set OC_DOMAIN}"
: "${AUTHENTIK_BOOTSTRAP_TOKEN:?set AUTHENTIK_BOOTSTRAP_TOKEN}"
export OC_DOMAIN AUTHENTIK_BOOTSTRAP_TOKEN
exec python3 - "$@" <<'PY'
import json
import os
import re
import secrets
import stat
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

if os.geteuid() != 0:
    raise SystemExit("Run as root: the test password belongs in /root/trial-user.pass")
if sys.argv[1:] not in ([], ["--revoke"]):
    raise SystemExit("Usage: authentik-bootstrap.sh [--revoke]")
domain = os.environ["OC_DOMAIN"]
if not re.fullmatch(r"[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?", domain):
    raise SystemExit("OC_DOMAIN must be a DNS name without a scheme or path")
token = os.environ["AUTHENTIK_BOOTSTRAP_TOKEN"]
if not token or any(ord(char) < 33 or ord(char) > 126 for char in token):
    raise SystemExit("Bootstrap token must contain only printable ASCII without whitespace")
base = f"https://auth.{domain}/api/v3/"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None  # Never forward the administrator token to another URL.


client = urllib.request.build_opener(NoRedirect)


def api(method, path, payload=None):
    request = urllib.request.Request(
        base + path,
        data=None if payload is None else json.dumps(payload).encode(),
        headers={"Authorization": "Bearer " + token,
                 "Content-Type": "application/json"},
        method=method,
    )
    try:
        with client.open(request, timeout=60) as response:
            body = response.read()
            return json.loads(body) if body else None
    except urllib.error.HTTPError as error:
        # API error bodies may echo submitted passwords or other credentials.
        raise SystemExit(f"{method} {path}: HTTP {error.code}; inspect Authentik logs") from None
    except urllib.error.URLError as error:
        raise SystemExit(f"{method} {path}: {error.reason}") from None


def lookup(path, field, value):
    data = api("GET", path + "?" + urllib.parse.urlencode({field: value}))
    matches = [item for item in data["results"] if item[field] == value]
    if data["pagination"]["next"] or len(matches) > 1:
        raise SystemExit(f"Ambiguous lookup: {path} {field}={value}")
    return matches[0] if matches else None


def flow(slug):
    result = lookup("flows/instances/", "slug", slug)
    if result is None:
        raise SystemExit(f"Missing flow {slug}; wait for default blueprints")
    return result["pk"]


if sys.argv[1:] == ["--revoke"]:
    api("DELETE", "core/tokens/authentik-bootstrap-token/")
    print("Bootstrap token revoked; remove bootstrap variables and recreate the worker")
    raise SystemExit(0)


provider_data = {
    "name": "Workspace", "mode": "forward_domain",
    "external_host": f"https://{domain}", "cookie_domain": domain,
    "authorization_flow": flow("default-provider-authorization-implicit-consent"),
    "invalidation_flow": flow("default-provider-invalidation-flow"),
}
provider = lookup("providers/proxy/", "name", "Workspace")
provider = api("PATCH", f"providers/proxy/{provider['pk']}/", provider_data) if provider else api(
    "POST", "providers/proxy/", provider_data)
application_data = {"name": "Workspace", "slug": "workspace", "provider": provider["pk"]}
application = lookup("core/applications/", "slug", "workspace")
api("PATCH", "core/applications/workspace/", application_data) if application else api(
    "POST", "core/applications/", application_data)
outpost = lookup("outposts/instances/", "name", "authentik Embedded Outpost")
if outpost is None:
    raise SystemExit("Embedded outpost not ready; wait for default blueprints")
config = dict(outpost["config"])
config["authentik_host"] = f"https://auth.{domain}/"
api("PATCH", f"outposts/instances/{outpost['pk']}/", {
    "providers": sorted(set(outpost["providers"] + [provider["pk"]])), "config": config,
})

user = lookup("core/users/", "username", "trial-user")
password_file = Path("/root/trial-user.pass")
if user and not password_file.exists():
    raise SystemExit("trial-user already exists without its password file; refusing to reset it")
if password_file.exists():
    info = password_file.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600:
        raise SystemExit("Password file must be a root-owned regular file with mode 0600")
    password = password_file.read_text().strip()
    if not password:
        raise SystemExit("Password file is empty")
else:
    password = secrets.token_urlsafe(32)
    fd = os.open(password_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as output:
        output.write(password + "\n")
if user is None:
    user = api("POST", "core/users/", {
        "username": "trial-user", "name": "Trial User", "is_active": True,
        "email": f"trial-user@{domain}", "type": "internal",
    })
api("POST", f"core/users/{user['pk']}/set_password/", {"password": password})

group = lookup("core/groups/", "name", "workspace-users")
if group is None:
    group = api("POST", "core/groups/", {"name": "workspace-users", "is_superuser": False})
api("POST", f"core/groups/{group['pk']}/add_user/", {"pk": user["pk"]})

# Read back the acceptance contract; reruns keep IDs and the saved password.
actual = api("GET", f"providers/proxy/{provider['pk']}/")
assert all(actual[key] == value for key, value in provider_data.items()), "provider mismatch"
assert api("GET", "core/applications/workspace/")["provider"] == provider["pk"]
actual = api("GET", f"outposts/instances/{outpost['pk']}/")
assert provider["pk"] in actual["providers"]
assert actual["config"]["authentik_host"] == config["authentik_host"]
assert api("GET", f"core/users/{user['pk']}/")["is_active"], "trial-user is inactive"
assert group["pk"] in api("GET", f"core/users/{user['pk']}/")["groups"], "trial-user group mismatch"
print("Verified Workspace provider, application, embedded outpost and trial-user in workspace-users")
print("Test password: /root/trial-user.pass (0600); value withheld")
PY
