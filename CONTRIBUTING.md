# Contributing

Agent Workspace is a proof of concept, provided as is, with no support or
compatibility guarantee. Issues and pull requests are welcome and may remain
unanswered. Keep changes focused on a concrete problem and describe the
resulting behavior, relevant checks and known limitations.

## Sign off your commits

Contributions use the [Developer Certificate of Origin (DCO), version
1.1](https://developercertificate.org/).
There is no Contributor License Agreement (CLA). Read the DCO before signing
off; the sign-off certifies that you have the right to submit the contribution
under the MIT License.

Add a sign-off to every contributed commit with:

```sh
git commit -s
```

Git adds `Signed-off-by: Your Name <your-email>` using your configured identity.
Use your own identity and an address you are willing to publish in commit
history. Preserve existing contributors' sign-offs when forwarding changes.

## Prepare a change

Use a branch or worktree. Include a small regression test for behavior changes,
especially authorization, identity, deletion and isolation changes. Describe
any configuration or migration effects. Documentation is written in English;
keep user-facing language changes consistent with the existing translations.

Use synthetic accounts, example domains and disposable credentials in tests
and reports. Do not commit deployment secrets, private addresses, internal
hostnames, personal transcripts, proprietary fonts or organization-specific
configuration. Follow [SECURITY.md](SECURITY.md) for vulnerability reports.

## Checks

Use the Go version declared in `manager/go.mod`. For manager changes:

```sh
cd manager
gofmt -l .
go vet ./...
go test ./...
```

Format changed Go files until `gofmt -l` produces no output. Run relevant
configuration, catalog or runtime checks when touching those components.
For deployment changes, follow the acceptance sequence in
[docs/INSTALL.md](https://zkmkarlsruhe.github.io/agent-workspace-docs/INSTALL/): login, create a project, chat, download the
actual result and verify persistence. A successful build or health endpoint
alone does not verify that sequence.

For documentation changes, check links in the published layout and check
that statements match the implementation. A design contract is not evidence
that its behavior has shipped. Record which checks ran and which could not
run, with the reason.
