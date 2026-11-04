# daedalus-agent
daedalus-agent: heterogeneous Linux endpoint for daedalus fleet control plane (P0.5 entry ticket; replaced by daedalus-atomic on migration per spec G7)

## CI gates

Two required jobs run on every push / PR (`.github/workflows/ci.yml`):

- **Go Test** — `go vet ./...` + `go test -race -count=1 ./...`. The SDK is checked out as a
  sibling directory (`daedalus-sdk/`), which is what `go.mod`'s
  `replace github.com/Daedalusys/daedalus-sdk => ../daedalus-sdk` resolves against.
- **Install E2E (systemd container)** — `scripts/install_test.sh` installs the agent for real
  inside a systemd container and asserts `systemctl is-active daedalus-agent.service` is
  `active`. This is the gate that closes the class of bug that let agent#2 (ExecStart missing
  the `daemon` sub-command → daemon dies immediately after install) reach `main`.

Neither job merges for you — the repository had **zero** workflows before this, so every change
reached `main` unverified. See the PR body of the change that added this file for the incident.

