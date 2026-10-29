# SPDX-License-Identifier: MIT

"""Receive the manager credential through stdin, never argv or container labels."""
import json
import ipaddress
import os
import re
import sys

config = json.load(sys.stdin)
token = config["token"]
addresses = {k: str(ipaddress.IPv4Address(config[k])) for k in ("listen", "manager")}
if not isinstance(token, str) or not re.fullmatch(r"[a-f0-9]{64}", token):
    raise ValueError("Invalid manager credential")
fd = os.open("/run/freecad-control.json", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
with os.fdopen(fd, "w") as out:
    json.dump(addresses, out)
# Publish a complete token atomically: the desktop waits for this file.
fd = os.open("/run/freecad-token.tmp", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
with os.fdopen(fd, "w") as out:
    out.write(token)

os.rename("/run/freecad-token.tmp", "/run/freecad-token")
