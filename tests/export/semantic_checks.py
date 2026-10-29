# SPDX-License-Identifier: MIT

"""Shared bounded semantic checks for source and documentation releases."""

import re
import ipaddress
import json
from pathlib import Path

# Split policy tokens so raw inventory scans do not report the gate itself.
PROVIDERS = r"(?i)\b(?:" + "|".join(("Qw" + r"en\w*", "Mis" + "tral", "Deep" + r"Seek\w*",
                                              "FL" + "UX", "IO" + "NOS", "OV" + r"H\w*", "GW" + "DG")) + r")\b"
ROOM_NAMES = (
    "Silver Egg", "Lichthof 8+9", "Lichthof 8 + 9", "Lichthof 8", "Lichthof 9",
    "Konferenzraum 3.OG Groß", "Konferenzraum 3. OG Klein",
    "Großes Rad (Nr. 2 / 53cm)", "Lastenrad (Nr.3)",
)
CAPABILITY_CLAIMS = (
    "Capabilities are skills the helper uses in addition to writing. These are always included:",
    "searches through the [[organization]] search engine, reads single pages as text and names the source.",
    "reads and creates Word, Excel, PDF and CSV tables and draws charts.",
    "turns a text in the project into a PDF in the [[organization]] layout.",
    "searches the [[organization]] wiki, the catalogue of works and [[website]], read only. Demo.",
    "describes an image from your project, such as a photo or a scan.",
    "creates an image from your description or changes an existing one, only when you ask for it.",
    "sends a problem, an idea, praise or a question to [[recipient]] when you ask for it (see below).",
    "At the top, the <strong>Results</strong>: every file with a download, texts also “As PDF”",
    "The helper sums up your feedback, has you confirm it and sends it to [[recipient]]",
    "searches the catalogue of the [[organization]] library for books and media, read only. Demo.",
)


class SemanticChecks:
    documents_only = False

    def files(self, *prefixes):
        if self.documents_only:
            paths = sorted(p for p in self.root.rglob("*.md") if ".git" not in p.relative_to(self.root).parts)
            self.assertTrue(paths, "No documentation scanned")
            return paths
        files = []
        for path in self.root.rglob("*"):
            name = path.relative_to(self.root).as_posix()
            if not path.is_file() or ".git" in path.relative_to(self.root).parts:
                continue
            if any(name == prefix or name.startswith(prefix.rstrip("/") + "/") for prefix in prefixes):
                files.append(path)
        self.assertTrue(files, f"No files scanned for {prefixes}")
        return sorted(files)

    def runtime_files(self):
        if self.documents_only:
            return self.files()
        return [p for p in self.files("manager", "deploy", "instance")
                if not p.name.endswith("_test.go") and "tests" not in p.parts
                and "testdata" not in p.parts and p.name not in {"go.sum", "go.mod"}]

    def scan(self, issue, paths, forbidden, exempt=None):
        paths = list(paths)
        self.assertTrue(paths, f"{issue}: empty scan")
        print(f'{issue} scanned: ' + ', '.join(p.relative_to(self.root).as_posix() for p in paths))
        hits = []
        pattern = re.compile(forbidden, re.I)
        for path in paths:
            section = ""
            for number, line in enumerate(path.read_text().splitlines(), 1):
                if re.match(r"^[a-z_]+:", line):
                    section = line.split(":", 1)[0]
                if pattern.search(line) and not (exempt and exempt(path, section, line)):
                    hits.append(f"{path.relative_to(self.root)}:{number}: {line.strip()}")
        if hits:
            self.fail(issue + " violations:\n" + "\n".join(hits))

    def example_copy(self, path, section, line):
        name = path.relative_to(self.root).as_posix()
        return name in {"instance/example/instance.yaml", "manager/internal/instance/example.yaml"} and section == "copy"

    def test_e01_provider_names(self):
        self.scan("E01", self.files("manager", "deploy", "instance"), PROVIDERS)

    def test_e02_fixed_exchange_rate(self):
        self.scan("E02", self.runtime_files(), r"1\s*EUR\s*=\s*1[.,]10\s*USD",
                  lambda p, section, line: p.relative_to(self.root).as_posix().startswith("instance/example/")
                  or p.relative_to(self.root).as_posix() == "manager/internal/instance/example.yaml")

    def test_e03_billing_claim(self):
        self.scan("E03", self.files("manager/web/templates", "manager/internal/co2", "manager/internal/portal"),
                  PROVIDERS + r".*(?:not.*charg|nicht.*berechnet|keine.*Kosten|kostenlos)")

    def test_e04_operator_reviewed_consent(self):
        if self.documents_only:
            # Only YAML consent examples need the instance review markers.
            paths = self.files()
            print("E04 scanned: " + ", ".join(str(p.relative_to(self.root)) for p in paths))
            for path in paths:
                text = path.read_text()
                for match in re.finditer(r"```(?:yaml|yml)\n(.*?)```", text, re.S):
                    block = match[1]
                    consent = re.search(r"(?m)^consent:", block)
                    if consent:
                        line = text[:match.start(1) + consent.start()].count("\n") + 1
                        self.assertIn("# Operator-reviewed example text for this instance only.", block,
                                      f"{path.relative_to(self.root)}:{line}: consent lacks review marker")
                        self.assertIn("# Other operators must review and replace these terms before enabling access.", block,
                                      f"{path.relative_to(self.root)}:{line}: consent lacks review requirement")
            return
        paths = [self.root / name for name in ("instance/example/instance.yaml", "manager/internal/instance/example.yaml")]
        print("E04 scanned: " + ", ".join(str(p.relative_to(self.root)) for p in paths))
        for path in paths:
            text = path.read_text()
            self.assertIn("\nconsent:\n", text, f"{path.relative_to(self.root)}:1: missing consent block")
            line = text[:text.find("\nconsent:")].count("\n") + 2
            context = text.split("\nconsent:", 1)[0].splitlines()[-4:]
            self.assertIn("# Operator-reviewed example text for this instance only.", context,
                          f"{path.relative_to(self.root)}:{line}: consent lacks operator-reviewed marker")
            self.assertIn("# Other operators must review and replace these terms before enabling access.", context,
                          f"{path.relative_to(self.root)}:{line}: consent lacks review requirement")

    def test_e05_no_house_inventory(self):
        paths = self.files("deploy/catalog", "deploy/integrations", "manager/internal/integration/builtins.json")
        inventory = r"(?<![a-z])(?:wiki|werke|drupal|opac)(?![a-z])|catalogue of works|catalog of works|Werkverzeichnis|catalog_search|catalog_item"
        if self.documents_only:
            # Generic prose about a wiki is valid; shipped inventory identifiers are not.
            inventory = r"werke_wiki|drupal|opac|catalogue of works|catalog of works|Werkverzeichnis|catalog_search|catalog_item"
        self.scan("E05", [p for p in paths if self.documents_only or "tests" not in p.relative_to(self.root).parts], inventory)

    def test_e06_no_private_media(self):
        paths = self.files("deploy/catalog", "manager/internal/integration/builtins.json")
        self.scan("E06", [p for p in paths if self.documents_only or "tests" not in p.relative_to(self.root).parts],
                  r'"id"\s*:\s*"media"|opencode-tool-medien|registry\.example\.org/operations/|sha256:[0-9a-f]{64}')

    def test_e07_synthetic_rooms(self):
        self.scan("E07", self.files("manager/internal/gateway/mcp/testdata/rooms", "manager/internal/gateway/mcp/rooms_test.go"),
                  "|".join(re.escape(name) for name in ROOM_NAMES))

    def test_e08_no_house_ldap_comments(self):
        self.scan("E08", self.files("manager/internal/settings/ldap.go", "manager/internal/config/config.go"),
                  r"//.*(?:Attributes as in|businessCategory|Deutsch \| English|keep the German part|source of .*directory)")

    def test_e09_capability_claims_are_conditional(self):
        paths = self.files("manager/web/templates")
        print("E09 scanned: " + ", ".join(str(p.relative_to(self.root)) for p in paths))
        hits = []
        for path in paths:
            text = path.read_text()
            stack = []
            offset = 0
            for part in re.split(r"(\{\{.*?\}\})", text, flags=re.S):
                action = part[2:-2].strip("- ") if part.startswith("{{") else ""
                word = action.split(" ", 1)[0]
                # Translation actions also contain the claim strings, so inspect before updating the stack.
                if not any(stack):
                    for claim in CAPABILITY_CLAIMS:
                        if claim in part:
                            number = text[:offset + part.index(claim)].count("\n") + 1
                            hits.append(f"{path.relative_to(self.root)}:{number}: {claim}")
                if word in {"if", "with", "range", "define", "block"}:
                    stack.append(bool(re.search(r"\.(?:Capabilities|Conns|Feedback)\b", action)))
                elif word == "end" and stack:
                    stack.pop()
                elif word == "else" and stack:
                    stack[-1] = False
                offset += len(part)
        if hits:
            self.fail("E09 violations:\n" + "\n".join(hits))

    def test_e10_no_forced_locale(self):
        self.scan("E10", self.runtime_files(), r"Suggest a short German folder name|Europe/Berlin",
                  lambda p, section, line: self.example_copy(p, section, line)
                  or (p.suffix == ".yaml" and (section == "defaults" or line.startswith("timezone:")))
                  # Schema-1 files missing these keys retain the historical defaults with a warning.
                  or (p.relative_to(self.root).as_posix() == "manager/internal/instance/instance.go"
                      and line.strip() in {
                          'd.Timezone = "Europe/Berlin"',
                          'const prompt = "Suggest a short German folder name for the following task: lower case, words separated by hyphens, at most four words. Answer with the name only."',
                      })
                  or (p.relative_to(self.root).as_posix() == "manager/internal/config/config.go"
                      and re.search(r'\bstr\("OC_TIMEZONE", "Europe/Berlin"\)', line)))

    def test_e11_no_stale_command_hosts(self):
        paths = self.files("deploy/etc-opencode/commands", "instance/minimal/rendered/commands", "instance/example/rendered/commands")
        self.scan("E11", paths, r"https?://agents\.[a-z0-9.-]*example(?:\.[a-z0-9.-]+)?")


    def test_e12_private_ip_literals(self):
        # Exact file/address exceptions cover synthetic network tests and deploy defaults.
        # New entries map addresses to review reasons; legacy entries are address lists.
        policy = Path(__file__).with_name("private-ip-fixtures.json")
        allowed = json.loads(policy.read_text())
        networks = [ipaddress.ip_network(n) for n in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")]
        paths = self.files() if self.documents_only else self.files("manager", "deploy", "instance", "docs", "services")
        hits = []
        for path in paths:
            name = path.relative_to(self.root).as_posix()
            for number, line in enumerate(path.read_text().splitlines(), 1):
                for match in re.finditer(r"(?<![\d.])(?:\d{1,3}\.){3}\d{1,3}(?![\d.])", line):
                    try:
                        address = ipaddress.ip_address(match[0])
                    except ValueError:
                        continue
                    if any(address in network for network in networks) and match[0] not in allowed.get(name, []):
                        hits.append(f"{name}:{number}: {match[0]}")
        self.assertFalse(hits, "E12 private IP violations:\n" + "\n".join(hits))
