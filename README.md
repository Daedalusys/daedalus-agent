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

## Directory-level `file` intents

A `file` intent whose `spec_json` carries `entries` treats `target` as a **directory root** and
changes only the sub-paths it names — the rest of the directory is never touched:

```json
{
  "entries": [
    { "path": "conf.d/a.conf", "op": "write",   "content": "..." },
    { "path": "current",       "op": "repoint", "symlink_target": "../releases/2" }
  ]
}
```

- `path` is relative to `target`. Absolute paths, `..`, a missing parent directory, and a parent
  that resolves through a symlink to outside `target` are all rejected — the whole batch is
  rejected before anything is snapshotted or written.
- `write` replaces the contents of an existing regular file (mode preserved) or creates a new
  one. It never writes through a symlink: if the final component is a symlink, the intent is
  rejected.
- `repoint` changes the symlink itself to point at `symlink_target` (atomic temp-link + rename).
  It can also create a new link, but refuses to replace a regular file.
- The same path may not appear twice, and no path may be both `write` and `repoint`.
- A spec without `entries` keeps the old `{"content": "..."}` meaning: `target` is a single
  regular file. Old callers see no change.

### Snapshot format

Before the first write, every named path is captured into
`<data_dir>/snapshots/<intent_id>/`:

```
<data_dir>/snapshots/<intent_id>/
    manifest.json     # per-path type, mode, uid/gid, sha256, symlink target, post hashes
    objects/<sha256>  # content-addressed file bodies, one copy per distinct content
```

- `/etc/**` and `/var/**` files are stored in full as content objects.
- `/usr/**` files are recorded as a **baseline reference only** (no local copy): that content
  comes from the immutable image, and the agent cannot restore it locally. If an apply under
  `/usr` fails, rollback reports the path instead of pretending to restore it.
- Symlinks store the link target string, never the content they point at.
- Diffs are not produced for restoring; there is no diff-driven rollback path.

After a successful apply the manifest records a post-apply hash per path. A later restore
refuses to overwrite a path whose current hash no longer matches (`rollback: refusing to
restore ... changed after apply`) instead of clobbering someone else's edit.

### Retention

Every new snapshot set triggers GC: the newest 20 intent snapshots are kept, with a 512 MiB
total cap (oldest deleted first). Objects referenced by a retained manifest are never deleted;
only unreferenced objects inside retained sets are swept. GC failures are logged and never fail
the intent.

## Rollback (package / service)

`package` and `service` intents have no file-level snapshot: the change is host state
(package database, unit state), and a file copy cannot undo it. What the agent promises
instead is a **rollback handle**: the manifest-side identifier of what it just did, returned in
`ApplyIntentResponse.rollback_handle` and stored by the control plane.

Handles are opaque to the control plane and defined by the agent:

| Handle | Meaning |
|---|---|
| `dnf-history:<id>` | the dnf transaction this apply produced |
| `apt-history:<id>` | the apt history entry this apply produced |
| `service-state:<base64url JSON>` | the unit's `ActiveState` / `UnitFileState` / `SubState` **before** the apply |
| *(empty)* | no anchor — e.g. the apply was a no-op, or the capture failed |

Capture never changes the apply outcome. A successful apply with an empty handle is still
`SUCCESS`, and the chain carries `apply_intent.rollback_handle_unavailable` so "this kind has no
handle" and "the handle was not captured" stay distinguishable. For `RISK_CLASS_HIGH`
`package`/`service` intents the agent re-judges the result after the apply: no handle →
`REFUSED` (low risk is unaffected — whether to do it is a risk question, not a capability one).

Rollback runs through `RollbackIntent` in two phases:

1. **PLAN** returns the immutable plan (`steps`), a `plan_hash` and a single-use
   `approve_token` (TTL 15 min). It executes **no** command — only read-only queries.
2. **APPLY** takes `plan_hash` + `approve_token` + the same `steps`. The agent recomputes the
   hash from the request (so a plan cannot be silently swapped), verifies the token, then runs
   the steps. Changing a command means asking for a new PLAN.

Auto vs human review:

- `dnf history undo <id>` and, on apt ≥ 3.2, the native history undo are single-step plans with
  `auto_ok` — the control plane may APPLY them unattended.
- Older apt has no native undo: the plan degrades to `apt-get remove`, then an `rm -rf` of the
  paths dpkg recorded as **conffiles** (`dpkg-query -W -f='${Conffiles}'`; `remove` keeps them),
  then a `MANUAL` checklist item for what no dpkg file list can prove (postinst/preinst side
  effects: users, groups, services, generated files). Every step is `human_review_required`.
  The delete step carries both its targets (`Paths`) and the package's full file list
  (`dpkg-query -L`) so the plan shows *why* those paths are deletable; conffiles that cannot be
  proven or cannot be expressed as an `rm -rf` argument are left to the human and named in the
  `MANUAL` step (`MANUAL` steps are never executed by the agent).
- Service rollback restores the pre-apply unit state (`start`/`stop`, `enable`/`disable`); the
  verb for a dimension that did not change is a no-op.

`rm -rf` is **always** `human_review_required`, only allowed for paths the package owns, and
only behind a valid token. Ownership is never taken from the request: APPLY steps carry no
file list, so the agent re-queries it from the handle (`apt history-info` → `dpkg-query -L`)
and refuses the whole plan when the list cannot be re-derived. APPLY is refused when the token
is missing, expired, already used, bound to another plan or another intent, or when a step
falls outside the command allowlist.
The token proves "this APPLY is bound to that plan hash" — it does **not** prove a human read
the plan; the control plane owns that. Tokens die with the process (re-plan after a restart);
handles do not, because they describe the package manager's state, not the agent's.

Every outcome lands in the same JSONL hash chain: `rollback.planned`, `rollback.applied`,
`rollback.refused`, `rollback.failed`.

## Known gaps

Open issues in this repository, so "has this been done yet?" is answerable by number:

| Issue | Scope |
|---|---|
| [#6](https://github.com/Daedalusys/daedalus-agent/issues/6) | `Register` returns the real sha256 of the active policy file instead of the `unhashed-policy-v0` placeholder — closed by [PR #11](https://github.com/Daedalusys/daedalus-agent/pull/11) |
| [#7](https://github.com/Daedalusys/daedalus-agent/issues/7) | `ApplyIntent` outcome events move from plain log lines into the JSONL audit hash chain |
| [#8](https://github.com/Daedalusys/daedalus-agent/issues/8) | Rollback backend: add LVM-snap / snapper (today only btrfs subvolume + rsync target are probed and selectable) |
| [#9](https://github.com/Daedalusys/daedalus-agent/issues/9) | `Heartbeat` returns the real `pending_intents` count (today hardcoded to `0`) |
| [#10](https://github.com/Daedalusys/daedalus-agent/issues/10) | Agent outbound gRPC client: `register` / `apply` / `report` now dial the peer over mTLS via `internal/agentclient` (closed; `--addr` overrides `control_plane_addr`) |
