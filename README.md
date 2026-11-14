<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Bellbuoy" height="56">
  </picture>
</p>

<p align="center">
  <strong>Turns failures, paused items and approved edits into one piece of work in the repository that owns the fix.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: ready to deploy against Muster contract v1 and Plainforge Git smart HTTP.** The worker leases both `incident` and `proposal` steps from Muster over contract v1, files deduplicated tasks for held or paused items on `refs/heads/bellbuoy/tasks` in Plainforge, pushes approved `DATA` and `CONFIG` proposals to `refs/heads/edit/EP-XXXX` in Plainforge over Git smart HTTP (and `ISSUE` proposals as tasks) before completing them to `building`, replays all 52 saved contract examples from Muster, and ships hardened `cs-workers` manifests under [`deploy/`](deploy), [`kustomization.yaml`](kustomization.yaml), and [`app.yaml`](app.yaml). See [Not yet done and needed from other components](#not-yet-done-and-needed-from-other-components).

Bellbuoy is stage 10 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Bellbuoy is meant to be understood and used without the rest.

## What works today

- **A task for every held or paused item.** `bellbuoy serve` leases Muster's `incident` steps. Each one says which worker left which item held or paused, and in what state. Bellbuoy opens a task in that worker's repository (on `refs/heads/bellbuoy/tasks` in Plainforge when `-plainforge` is set, or in the local ledger file otherwise), or adds to the one already open for the same kind of incident, then completes the step with the item's state unchanged and no result, as the contract requires.
- **Approved Portal proposals move to `building`.** `bellbuoy serve` leases Muster's `proposal` steps in `state: "approved"`, verifies the proposal against `ownership_map.json` (recovering `file` from `elements` when Muster restarts over PostgreSQL 17), pushes branch `refs/heads/edit/EP-XXXX` to Plainforge over Git smart HTTP for `DATA`/`CONFIG` or opens one task for `ISSUE`, and completes the step to `state: "building"` with the recorded change or task as `result`.
- **Muster's five calls.** `lease`, `heartbeat`, `complete`, `fail` and `needs_subtool`, as contract v1 defines them. All 52 saved examples from Muster's `contract/v1/examples/` are replayed in `internal/muster/contract_test.go`. The ServiceAccount token is read fresh for every call. No redirect is followed and no proxy is used.
- **One task per failure.** A failure is given a fingerprint from the worker's name, the error code and the shape of the message. The first time a fingerprint is seen, a task is opened in the repository that owns the worker. Every repeat adds one to that task's count. Two hundred identical failures make one task with a count of 200.
- **Approved edits become changes.** An owner-approved `DATA` or `CONFIG` proposal or edit is committed on its own branch with a request to merge it. An `ISSUE` proposal or edit becomes a task. The same proposal or edit delivered twice changes nothing the second time.
- **Mirror requests.** After a write to a repository that has a public mirror, Plainforge is asked to bring the mirror up to date, for source and tasks only. A request that names images, secrets or captures is paused whole. In the cluster, Plainforge mirrors `refs/heads/main` and tags automatically while keeping `refs/heads/edit/*` and `refs/heads/bellbuoy/tasks` cluster-private.
- **Owner alerts.** One JSON alert is posted to a webhook for a new fingerprint, a `P0` crash or dead letter, or urgent regulatory drift. Repeats are quiet.
- **Live stream events.** Edit pill states (`Proposed`, `Approved`, `Building`, `Live`) and failure counts are prepared for the Portal.
- **Stop instead of guess.** A job that no rule covers is paused as `needs_subtool` with `E_NEEDS_SUBTOOL`, and the rest of the queue carries on.
- **Nothing is lost when something outside is down.** A timed-out delivery is staged on disk and retried, from one second up to five minutes apart, until it lands. The result of every step is written to disk before Muster is told, and removed only when Muster answers `200`. Finished work is not repeated, across retries, restarts, or a step being handed out again.
- **Nothing private gets out.** A failure message reaches a task only as its sanitized pattern (`dial tcp # timeout after # on #`). It never reaches an alert, an event or a log line. Errors carry one of three codes and a fixed reason.

## Run it

You need Go 1.24 or later. There are no other dependencies.

```sh
go vet ./...
go test -race ./...
```

runs every static check and test, including all sub-tool fixtures, real Git smart HTTP exchanges checked with `git fsck --full --strict`, and all 52 saved Muster contract examples.

```sh
go run ./cmd/bellbuoy replay \
  -jobs testdata/replay/jobs.jsonl \
  -owners testdata/ownership_map.json
```

runs nineteen made-up jobs of every kind through the whole worker and prints, as JSON, how each job was answered and everything that would have been sent to Plainforge, Muster and the webhook. The same input prints the same output every time; the expected output is saved in [`testdata/replay/want.json`](testdata/replay/want.json) and a test compares the two byte for byte.

`replay` uses in-memory stand-ins, so it touches nothing outside the process, with one exception: if `BELLBUOY_WEBHOOK_URL` is set, alerts are posted to that address for real. Add `-spool DIR` to choose where unfinished jobs are staged; by default a temporary folder is used and removed.

```sh
bellbuoy serve \
  -muster http://ADDRESS-OF-MUSTER \
  -plainforge http://ADDRESS-OF-PLAINFORGE \
  -owners ownership_map.json
```

leases steps from a real Muster and works on them until it is told to stop. It reads the pod's ServiceAccount token, stages unfinished results in `/var/spool/cs`, pushes proposed `DATA`/`CONFIG` changes to `refs/heads/edit/EP-XXXX` in the owning Plainforge repository over Git smart HTTP, and keeps tasks in `tasks.json` on `refs/heads/bellbuoy/tasks` in the owning repository so task counts survive pod rescheduling across nodes. When `-plainforge` is omitted, tasks and proposed changes are kept in a local file (`tasks.ledger` in `/var/spool/cs` by default, or `-ledger FILE`). When Muster has nothing to lease it waits two seconds and asks again. Other flags: `-spool`, `-ledger`, `-token-file`, `-idle` and `-heartbeat`.

In `serve`, only the sub-tools with somewhere real to deliver to are run: `internal-git` always, and `webhook-alert` when `BELLBUOY_WEBHOOK_URL` is set. If it is not set, no alerts are sent. `github-mirror` and `portal-sse` are not run: Plainforge pushes `refs/heads/main` and tags to GitHub on its own, and Muster's `/v1/feed` emits proposal `building` events directly when Bellbuoy completes a proposal without a separate worker event call.

To build the `arm64` image with [`ko`](https://ko.build) and no Dockerfile (base image pinned by digest in [`.ko.yaml`](.ko.yaml)):

```sh
KO_DOCKER_REPO=lazarette.clearedstack.svc.cluster.local:5000 \
  ko build --base-import-paths --platform=linux/arm64 ./cmd/bellbuoy
```

## How it is laid out

| Path | What it holds |
| --- | --- |
| [`cmd/bellbuoy`](cmd/bellbuoy) | The command. |
| [`deploy/`](deploy) | Kubernetes manifests (`bellbuoy.yaml` and `networkpolicy.yaml`) for namespace `cs-workers`, referenced by [`kustomization.yaml`](kustomization.yaml) alongside [`app.yaml`](app.yaml). |
| [`tools/registry.yaml`](tools/registry.yaml) | The four sub-tools, in the order they are tried. |
| `tools/<name>/match.go` | The sub-tool's matching rule. |
| `tools/<name>/logic.go` | The sub-tool's logic. |
| `tools/<name>/fixtures/` | Saved examples: a job in, the exact result expected out. |
| [`internal/fingerprint`](internal/fingerprint) | The fingerprint function. |
| [`internal/tasks`](internal/tasks) | The open-or-add-to rule. |
| [`internal/sanitize`](internal/sanitize) | Failure-pattern sanitizing and shape checks for every identifier. |
| [`internal/codes`](internal/codes) | The three public error codes. |
| [`internal/ownership`](internal/ownership) | Reader for `ownership_map.json` (both Muster's `elements` form and the cluster map). |
| [`internal/route`](internal/route) | Runs one job through the sub-tools. |
| [`internal/worker`](internal/worker) | The lease loop: urgent before normal before backfill. |
| [`internal/spool`](internal/spool) | Unfinished jobs on disk. |
| [`internal/muster`](internal/muster) | The five calls Bellbuoy makes to Muster: the HTTP client, an in-memory stand-in, and a strict fake server for tests. |
| [`internal/plainforge`](internal/plainforge) | The interface Bellbuoy needs from Plainforge, a pure-stdlib Git smart HTTP client (`githttp.go`), an in-memory stand-in, and the local task ledger fallback. |
| [`internal/fixture`](internal/fixture) | Plays a saved example against a sub-tool and compares byte for byte. |

### Saved examples

Every rule has saved examples, and a change in behaviour fails a test.

| Where | Examples |
| --- | --- |
| `tools/internal-git/fixtures` | 48 |
| `tools/github-mirror/fixtures` | 17 |
| `tools/webhook-alert/fixtures` | 16 |
| `tools/portal-sse/fixtures` | 19 |
| `internal/tasks/testdata` | 2, one of them the two hundred identical failures |
| `internal/fingerprint/testdata` | 7 fingerprints, worked out by a separate implementation |
| `testdata/replay` | 1 run of the whole worker |
| `testdata/contract` | All 52 examples copied from Muster's `contract/v1/examples/`, every one replayed in `internal/muster/contract_test.go`. |

After a deliberate change, `go test ./tools/... -update` rewrites the expected results. Read the difference before committing it.

## Settings

| Setting | What it is |
| --- | --- |
| `BELLBUOY_WEBHOOK_URL` | Address of the owner's webhook. Treated as a credential: never logged, never put in an error. Mounted optionally from Secret `bellbuoy-webhook` in `cs-workers`. If it is not set, `replay` keeps alerts and prints them, and `serve` sends none. |
| `-muster` | Address of Muster, for `serve`. Never logged, never put in an error. |
| `-plainforge` | Address of Plainforge over Git smart HTTP, for `serve`. Never logged, never put in an error. |
| `-token-file` | The ServiceAccount token file. The default is where a pod finds it. |
| `GOMEMLIMIT` | If not set, the command sets a soft limit of 24 MiB on the Go heap. |

`ownership_map.json` says which repository owns what. Its form is described in [`SPEC.md`](SPEC.md); [`deploy/bellbuoy.yaml`](deploy/bellbuoy.yaml) embeds the cluster map for all 17 ClearedStack repositories, and [`testdata/ownership_map.json`](testdata/ownership_map.json) is the made-up one for tests.

## Requirements and where they stand

| Requirement in `SPEC.md` | State |
| --- | --- |
| Drain `urgent` before `normal` before `backfill` | Built and tested. |
| `lease`, `heartbeat`, `complete`, `fail`, `needs_subtool` over HTTP | Built to Muster's contract v1. All 52 saved examples from `contract/v1/examples/` are replayed in `internal/muster/contract_test.go`. |
| A task for every held or paused item (`incident`) | Built and tested end to end against `mustertest.Fake` and real Git smart HTTP (`refs/heads/bellbuoy/tasks`), including Muster's saved `lease-incident.json` and pod rescheduling onto a fresh spool. |
| Fingerprint and open-or-add-to | Built and tested. |
| Edit-to-repo loop for `DATA`, `CONFIG`, `ISSUE` (`proposal` and `edit`) | Built and tested end to end for Muster's `proposal` steps (`approved` to `building`), pushing `refs/heads/edit/EP-XXXX` to Plainforge over Git smart HTTP (checked with `git fsck --full --strict`), including after a Muster restart over PostgreSQL 17. |
| `internal-git`, `github-mirror`, `webhook-alert`, `portal-sse` | Built, each with a rule, logic and saved examples. |
| Live stream: edit pills and operations | Proposal `building` pills are driven via Muster's `POST /v1/complete` (`state: "building"`). `portal-sse` is built and tested against the stand-in; Muster's contract v1 has no separate worker event endpoint. |
| Live stream: item-lifecycle and registry pages | Paused as `needs_subtool` until their event shapes are specified in the contract. |
| `E_TIMEOUT`: stage in the spool and retry with backoff | Built and tested, including across a restart and a step being handed out again. |
| `E_PARSE`: refuse without touching the repository | Built and tested. |
| `E_NEEDS_SUBTOOL`: fail closed and pause | Built and tested. |
| Only sanitized codes and fingerprints leave | Built and tested. |
| Source-only mirroring | Built and tested. |
| No connection to Logbook, no database credentials | True by construction and enforced by [`deploy/networkpolicy.yaml`](deploy/networkpolicy.yaml). |
| 30 MiB memory budget | Measured well below 30 MiB and capped at `30Mi` in [`deploy/bellbuoy.yaml`](deploy/bellbuoy.yaml) (`GOMEMLIMIT=24MiB`). |
| Non-root, no added privileges, read-only filesystem apart from the spool | Enforced in [`deploy/bellbuoy.yaml`](deploy/bellbuoy.yaml) (`runAsNonRoot: true`, UID `65532`, `readOnlyRootFilesystem: true`, `drop: ["ALL"]`, `arm64` node affinity excluding `cm3588-plus`) and verified by `TestDeployManifestsAndPosture`. |
| Image built with `ko`, no Dockerfile, pinned by digest | Base image pinned by digest in [`.ko.yaml`](.ko.yaml); workload image pinned by `@sha256:` in [`deploy/bellbuoy.yaml`](deploy/bellbuoy.yaml) and [`app.yaml`](app.yaml). |

### Memory

- The built command at rest uses 5.0 MiB of resident memory. Running the nineteen-job example peaks at 6.4 MiB.
- `serve`, asking an empty queue for work every 20 milliseconds for four seconds, peaks at 10.4 MiB.
- A test runs 11,000 jobs through the lease loop with stand-ins that keep nothing. The Go heap ends at about 220 KiB, where it started, and everything the runtime has taken from the system is about 13 MiB.
- `replay` keeps the whole input and every result in memory in order to print them, so it grows with the file: 20,000 jobs peaked at 32 MiB when last measured, which is over the budget. That is the replay harness, not the loop.

## Not yet done and needed from other components

- **Plainforge dedicated task HTTP contract (`ClearedStack/plainforge`, optional).** Plainforge deliberately exposes only Git smart HTTP today, so `bellbuoy serve -plainforge ...` pushes `DATA`/`CONFIG` proposals to `refs/heads/edit/EP-XXXX` and stores tasks in `tasks.json` on `refs/heads/bellbuoy/tasks` inside each repository (which Plainforge keeps cluster-private and never mirrors to GitHub or announces to Keelworks). If Plainforge later adds a dedicated HTTP task and GitHub-issue-mirror contract, `plainforge.Client` can switch its task methods to that endpoint behind `plainforge.Forge`.
- **Muster contract / Logbook columns (`ClearedStack/muster`, `ClearedStack/logbook`).**
  - In PostgreSQL 17 (`core.edit_proposal`), `file` and `reason` are kept in Muster's in-memory `beside` map rather than columns in `core.edit_proposal`; Bellbuoy recovers `file` from `ownership_map.json` when Muster restarts, and persisting `file` and `reason` in `core.edit_proposal` would preserve visitor rationale across Muster restarts as well.
  - Muster's contract v1 offers `incident` and `proposal` steps to `bellbuoy`; it has no step kind yet for regulatory `drift` or explicit `mirror` jobs, and no worker call to publish `operations`, `item-lifecycle` or `registry` SSE events (`proposal` `building` and `live` events are emitted by Muster itself).
  - Muster refuses `needs_subtool` on `incident` and `proposal` steps (`409 move_not_allowed`), so if a step names an unlisted worker or repository, Bellbuoy writes nothing and lets the lease expire for Muster to re-offer once configuration is updated.
- **Keelworks, Lazarette, and Shipkeeper (`ClearedStack/keelworks`, `ClearedStack/lazarette`, `ClearedStack/shipkeeper`).** Build `./cmd/bellbuoy` with `ko` for `linux/arm64`, push to `lazarette.clearedstack.svc.cluster.local:5000/bellbuoy`, and pin the resulting `sha256` digest in [`app.yaml`](app.yaml) and [`deploy/bellbuoy.yaml`](deploy/bellbuoy.yaml) before Shipkeeper reconciles `cs-workers`.
- **Cluster Secret for alerts (optional).** If owner webhook alerts are desired, create or seal Secret `bellbuoy-webhook` (`key: url`) in namespace `cs-workers`.
- **Single replica assumption.** One replica of Bellbuoy runs (`replicas: 1`, `strategy: Recreate`), with optimistic compare-and-swap retries on `refs/heads/bellbuoy/tasks` if a push ever races.

`SPEC.md` was changed while building, to define things it named and did not define. Each change and its reason is listed at the end of that file.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Bellbuoy does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt the build was started with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`app.yaml`](app.yaml) | Desired workload state for Keelworks digest pinning and Shipkeeper reconciliation. |
| [`kustomization.yaml`](kustomization.yaml) | Kustomize entrypoint referencing [`deploy/networkpolicy.yaml`](deploy/networkpolicy.yaml) and [`deploy/bellbuoy.yaml`](deploy/bellbuoy.yaml). |
| [`deploy/`](deploy) | Kubernetes manifests (`ServiceAccount`, `ConfigMap`, `Deployment`, `NetworkPolicy`) for `cs-workers`. |
| [`brand/`](brand) | The Bellbuoy logo and mark, for light and dark backgrounds. |

## At a glance

| | |
| --- | --- |
| **Stage** | 10 of 10 in the pipeline |
| **Formerly** | `cs-10-notify`, then Foghorn |
| **Namespace** | `cs-workers` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/bellbuoy` |
| **Memory budget** | 30 MiB |
| **Depends on** | [Muster](https://github.com/ClearedStack/muster), [Plainforge](https://github.com/ClearedStack/plainforge) |
| **Depended on by** | Nothing else in the platform |
| **Rules for particular cases** | `internal-git`, `github-mirror`, `webhook-alert`, `portal-sse` |

### Boundaries

- Leases routing work from Muster
- Commits changes and opens tasks in Plainforge
- Sends owner alerts to a configured webhook
- Feeds the live update stream through Muster

## Names

`bellbuoy` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
