# Agent Workspace

[![ZKM Karlsruhe](https://img.shields.io/badge/ZKM-Karlsruhe-blue)](https://zkm.de)
[![ZKM Open Source](https://img.shields.io/badge/ZKM-Open%20Source-blue)](https://github.com/zkmkarlsruhe)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

Agent Workspace is a self-hosted, multi-user workspace built on OpenCode.
People describe a task, add files, work with a helper in a browser, and
download the results. Each person has a persistent workspace and an isolated
runtime; projects and chats survive when that runtime stops.

> **Prototype for evaluation only. Not production software, no support, no guarantees. Do not make it reachable from the internet; run it only in an internal network. Use at your own risk.**

Developed at ZKM | Center for Art and Media Karlsruhe

## Portal navigation (2026-10-28)

Main bar: **My projects · Assistants · Capabilities · Help** / **Meine Projekte ·
Assistenten · Fähigkeiten · Hilfe**. The menu under the username holds **About me
& how I work** (**Über mich & Arbeitsweise**), Usage, Connections (Nextcloud),
Privacy and Delete account. Environment controls remain accessible through Help.

Assistants use `/assistants` and `/new?assistant=<id>`; personal settings use
`/settings`; Capabilities keeps `/tools`. Old links and form parameters remain
compatible. Instance definitions now use `assistants:`; `profiles:` is accepted
for one release with a deprecation warning. See [instance naming and compatibility](https://github.com/zkmkarlsruhe/agent-workspace-docs/blob/main/docs/instance.md).

## What it provides

- A browser portal for projects, uploads, downloads, trash and data export.
- One OpenCode environment per active person, with persistent files and chats.
- Authentication through a trusted forward-auth proxy and an identity guard.
- Model access through a manager gateway, with usage estimates and daily caps.
- Assistants with a fresh project per task or a shared project with a new session per question.
- Capabilities selected per project from each person's available catalog.
- Optional dormant-session auto-archive with restore, and watchdog notices for stalled sessions.
- An instance configuration for branding, models, policies and integrations.
- Optional catalog capabilities and service connections; the minimal instance enables
  none of the optional integrations and disables chat evaluation.

The interface supports English and German. Documentation is in English.

## Architecture

```text
Browser -> TLS proxy + identity provider -> Go manager
                                            |-> personal OpenCode runtime
                                            |-> model gateway -> LiteLLM -> model endpoint
                                            |-> filtered Docker proxy
                                            |-> filesystem provisioner
                                            `-> SQLite metadata and usage
```

The manager serves the portal, routes each person's runtime hostname, starts
and stops environments, and mediates access to external services. Personal
files and runtime state live on quota-controlled host storage. Runtimes have
private networks and restricted egress. Model credentials remain outside
them. Containers share the host kernel; this is not VM isolation.

Read the [architecture](https://github.com/zkmkarlsruhe/agent-workspace-docs/blob/main/docs/architecture.md), [privacy and retention
behavior](https://github.com/zkmkarlsruhe/agent-workspace-docs/blob/main/docs/privacy.md), and [architecture decisions](https://github.com/zkmkarlsruhe/agent-workspace-docs/blob/main/docs/architecture.md#decisions).

## Quick start

Follow [docs/INSTALL.md](docs/INSTALL.md) on a new amd64 Linux VM. The guide
uses Docker Compose, Traefik, Authentik and LiteLLM, and builds the manager
and runtime locally. It does not require a published image registry.

1. Prepare the VM, wildcard DNS, TLS, Docker user namespaces and XFS quotas.
2. Configure instance secrets, a stable identity issuer, and a streaming
   model endpoint that supports tool calls. Match model capabilities in
   `instance/minimal/instance.yaml` to that endpoint.
3. Build the images, render the minimal instance, configure the IdP and start
   the stack using the installation guide's commands.
4. Sign in, accept the notice, create a project, ask for a file, download it,
   and verify that it persists after signing out and back in.

Budget for at least 4 vCPUs, 8 GiB RAM and 80 GiB disk for the reference trial
with two concurrent people. Model requests may incur provider charges; the
displayed cost cap is an estimate, not a provider billing guarantee.

## Release and contributions

Released under the [MIT License](LICENSE). Third-party components retain their own licenses.

See [CONTRIBUTING.md](CONTRIBUTING.md) for DCO sign-off and checks, and
[SECURITY.md](SECURITY.md) for vulnerability reporting.

This is the code repository. Adopter guides and architecture decisions live in the
[documentation repository](https://github.com/zkmkarlsruhe/agent-workspace-docs/blob/main/README.md).
