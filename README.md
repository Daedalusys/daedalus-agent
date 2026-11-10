# daedalus-agent
daedalus-agent: heterogeneous Linux endpoint for daedalus fleet control plane (P0.5 entry ticket; replaced by daedalus-atomic on migration per spec G7)

## CI gates

Two jobs **run** on every push / PR (`.github/workflows/ci.yml`):

- **Go Test** — `go vet ./...` + `go test -race -count=1 ./...`. The SDK is checked out as a
  sibling directory (`daedalus-sdk/`), which is what `go.mod`'s
  `replace github.com/Daedalusys/daedalus-sdk => ../daedalus-sdk` resolves against.
- **Install E2E (systemd container)** — `scripts/install_test.sh` installs the agent for real
  inside a systemd container and asserts `systemctl is-active daedalus-agent.service` is
  `active`. This is the gate that closes the class of bug that let agent#2 (ExecStart missing
  the `daemon` sub-command → daemon dies immediately after install) reach `main`.

**Neither job blocks a merge, and `main` has no branch protection.** `branches/main/protection`
returns `404 Branch not protected`, the repository has no rulesets (`rulesets` → `[]`), and
`required_status_checks.enforcement_level` is `off`. The two jobs report their result; a red
run does not stop a merge. A gate that only *claims* to block merges is worse than no gate at
all, because it teaches reviewers to trust a signal nothing enforces.

Turning these two jobs into enforced merge conditions on `main` is a repository-management
decision and is **open for the maintainer** — it is **not done in this repository yet**.

The incident that motivates the workflow: the repository had **zero** workflows before it, so
every change reached `main` unverified (see the PR body of the change that added this file).
Two jobs exist now, but they are **not required** as merge conditions, so the same class of
incident can still recur.

## Known gaps

Open issues in this repository, so "has this been done yet?" is answerable by number:

| Issue | Scope |
|---|---|
| [#6](https://github.com/Daedalusys/daedalus-agent/issues/6) | `Register` returns the real sha256 of the active policy file instead of the `unhashed-policy-v0` placeholder — closed by [PR #11](https://github.com/Daedalusys/daedalus-agent/pull/11) |
| [#7](https://github.com/Daedalusys/daedalus-agent/issues/7) | `ApplyIntent` outcome events move from plain log lines into the JSONL audit hash chain |
| [#8](https://github.com/Daedalusys/daedalus-agent/issues/8) | Rollback backend: add LVM-snap / snapper (today only btrfs subvolume + rsync target are probed and selectable) |
| [#9](https://github.com/Daedalusys/daedalus-agent/issues/9) | `Heartbeat` returns the real `pending_intents` count (today hardcoded to `0`) |
| [#10](https://github.com/Daedalusys/daedalus-agent/issues/10) | Agent outbound gRPC client: `register` / `apply` / `report` now dial the peer over mTLS via `internal/agentclient` (closed; `--addr` overrides `control_plane_addr`) |
