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

> **Status: partly built.** The worker leases steps from Muster over its v1 contract and files a task for every item Muster holds or has paused. It is checked against Muster's own saved examples of the contract and against a fake Muster. It has not talked to a running Muster and has not run in a cluster. Tasks are kept in a file for now, because Plainforge has no interface for tasks yet. See [Not yet done](#not-yet-done).

Bellbuoy is stage 10 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Bellbuoy is meant to be understood and used without the rest.

## What works today

- **A task for every held or paused item.** `bellbuoy serve` leases Muster's incident steps. Each one says which worker left which item held or paused, and in what state. Bellbuoy opens a task in that worker's repository, or adds to the one already open for the same kind of incident, then completes the step with the item's state unchanged and no result, as the contract requires.
- **Muster's five calls.** `lease`, `heartbeat`, `complete`, `fail` and `needs_subtool`, as contract v1 defines them. The ServiceAccount token is read fresh for every call. No redirect is followed and no proxy is used.
- **One task per failure.** A failure is given a fingerprint from the worker's name, the error code and the shape of the message. The first time a fingerprint is seen, a task is opened in the repository that owns the worker. Every repeat adds one to that task's count. Two hundred identical failures make one task with a count of 200.
- **Approved edits become changes.** An owner-approved `DATA` or `CONFIG` edit is committed on its own branch with a request to merge it. An `ISSUE` edit becomes a task. The same edit delivered twice changes nothing the second time.
- **Mirror requests.** After a write to a repository that has a public mirror, Plainforge is asked to bring the mirror up to date, for source and tasks only. A request that names images, secrets or captures is paused whole.
- **Owner alerts.** One JSON alert is posted to a webhook for a new fingerprint, a `P0` crash or dead letter, or urgent regulatory drift. Repeats are quiet.
- **Live stream events.** Edit pill states (`Proposed`, `Approved`, `Building`, `Live`) and failure counts are handed to Muster for the Portal.
- **Stop instead of guess.** A job that no rule covers is paused as `needs_subtool` with `E_NEEDS_SUBTOOL`, and the rest of the queue carries on.
- **Nothing is lost when something outside is down.** A timed-out delivery is staged on disk and retried, from one second up to five minutes apart, until it lands. The result of every step is written to disk before Muster is told, and removed only when Muster answers `200`. Finished work is not repeated, across retries, restarts, or a step being handed out again.
- **Nothing private gets out.** A failure message reaches a task only as its sanitized pattern (`dial tcp # timeout after # on #`). It never reaches an alert, an event or a log line. Errors carry one of three codes and a fixed reason.

## Run it

You need Go 1.24 or later. There are no other dependencies.

```sh
go test ./...
```

runs every test, including the saved examples.

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
  -owners ownership_map.json
```

leases steps from a real Muster and works on them until it is told to stop. It reads the pod's ServiceAccount token, stages results in `/var/spool/cs`, and keeps tasks in `tasks.ledger` in that folder. When Muster has nothing to lease it waits two seconds and asks again. Other flags: `-spool`, `-ledger`, `-token-file`, `-idle` and `-heartbeat`.

In `serve`, only the sub-tools with somewhere real to deliver to are run: `internal-git` always, and `webhook-alert` when `BELLBUOY_WEBHOOK_URL` is set. If it is not set, no alerts are sent. `github-mirror` and `portal-sse` are not run: the task ledger has no mirror, and Muster's contract has no call that takes a live event.

To build the image, with [`ko`](https://ko.build) and no Dockerfile:

```sh
KO_DOCKER_REPO=lazarette.clearedstack.svc.cluster.local:5000 \
  ko build --base-import-paths ./cmd/bellbuoy
```

## How it is laid out

| Path | What it holds |
| --- | --- |
| [`cmd/bellbuoy`](cmd/bellbuoy) | The command. |
| [`tools/registry.yaml`](tools/registry.yaml) | The four sub-tools, in the order they are tried. |
| `tools/<name>/match.go` | The sub-tool's matching rule. |
| `tools/<name>/logic.go` | The sub-tool's logic. |
| `tools/<name>/fixtures/` | Saved examples: a job in, the exact result expected out. |
| [`internal/fingerprint`](internal/fingerprint) | The fingerprint function. |
| [`internal/tasks`](internal/tasks) | The open-or-add-to rule. |
| [`internal/sanitize`](internal/sanitize) | Failure-pattern sanitizing and shape checks for every identifier. |
| [`internal/codes`](internal/codes) | The three public error codes. |
| [`internal/ownership`](internal/ownership) | Reader for `ownership_map.json`. |
| [`internal/route`](internal/route) | Runs one job through the sub-tools. |
| [`internal/worker`](internal/worker) | The lease loop: urgent before normal before backfill. |
| [`internal/spool`](internal/spool) | Unfinished jobs on disk. |
| [`internal/muster`](internal/muster) | The five calls Bellbuoy makes to Muster: the HTTP client, an in-memory stand-in, and a strict fake server for tests. |
| [`internal/plainforge`](internal/plainforge) | The interface Bellbuoy needs from Plainforge, an in-memory stand-in, and the task ledger file. |
| [`internal/fixture`](internal/fixture) | Plays a saved example against a sub-tool and compares byte for byte. |

### Saved examples

Every rule has saved examples, and a change in behaviour fails a test.

| Where | Examples |
| --- | --- |
| `tools/internal-git/fixtures` | 41 |
| `tools/github-mirror/fixtures` | 16 |
| `tools/webhook-alert/fixtures` | 16 |
| `tools/portal-sse/fixtures` | 18 |
| `internal/tasks/testdata` | 2, one of them the two hundred identical failures |
| `internal/fingerprint/testdata` | 7 fingerprints, worked out by a separate implementation |
| `testdata/replay` | 1 run of the whole worker |
| `testdata/contract` | 51 examples copied from Muster's contract v1. The 31 that are worker calls are replayed through the client. |

After a deliberate change, `go test ./tools/... -update` rewrites the expected results. Read the difference before committing it.

## Settings

| Setting | What it is |
| --- | --- |
| `BELLBUOY_WEBHOOK_URL` | Address of the owner's webhook. Treated as a credential: never logged, never put in an error. If it is not set, `replay` keeps alerts and prints them, and `serve` sends none. |
| `-muster` | Address of Muster, for `serve`. Never logged, never put in an error. |
| `-token-file` | The ServiceAccount token file. The default is where a pod finds it. |
| `GOMEMLIMIT` | If not set, the command sets a soft limit of 24 MiB on the Go heap. |

`ownership_map.json` says which repository owns what. Its form is described in [`SPEC.md`](SPEC.md), and [`testdata/ownership_map.json`](testdata/ownership_map.json) is a made-up one.

## Requirements and where they stand

| Requirement in `SPEC.md` | State |
| --- | --- |
| Drain `urgent` before `normal` before `backfill` | Built and tested. |
| `lease`, `heartbeat`, `complete`, `fail`, `needs_subtool` over HTTP | Built to Muster's contract v1. Muster's 31 saved examples of the worker calls are replayed: the client sends exactly the request in each of the 25 a correct client can send, and reads all 31 answers as the contract means them. Not yet run against a running Muster. |
| A task for every held or paused item | Built and tested end to end against the fake server. Tasks go to a ledger file, not to Plainforge. |
| Fingerprint and open-or-add-to | Built and tested. |
| Edit-to-repo loop for `DATA`, `CONFIG`, `ISSUE` | Built and tested against the stand-in, in the shape assumed before Muster's contract. Not wired to Muster's proposals. No client for the real Plainforge. |
| `internal-git`, `github-mirror`, `webhook-alert`, `portal-sse` | Built, each with a rule, logic and saved examples. |
| Live stream: edit pills and operations | Built and tested against the stand-in. Not run in `serve`: Muster's contract has no call that takes an event. |
| Live stream: item-lifecycle and registry pages | Not done. Paused as `needs_subtool`. |
| `E_TIMEOUT`: stage in the spool and retry with backoff | Built and tested, including across a restart and a step being handed out again. |
| `E_PARSE`: refuse without touching the repository | Built and tested. |
| `E_NEEDS_SUBTOOL`: fail closed and pause | Built and tested. |
| Only sanitized codes and fingerprints leave | Built and tested. |
| Source-only mirroring | Built and tested. |
| No connection to Logbook, no database credentials | True by construction: nothing in the code opens one. |
| 30 MiB memory budget | Measured, not yet seen in a cluster. See below. |
| Non-root, no added privileges, read-only filesystem apart from the spool | The image runs as a user other than root and the program writes only to the spool. Not yet run in a cluster. No manifest. |
| Image built with `ko`, no Dockerfile | Built once with `ko build --push=false`: a 3.4 MB image that runs as user 65532. Not pushed anywhere. |

### Memory

- The built command at rest uses 5.0 MiB of resident memory. Running the nineteen-job example peaks at 6.4 MiB.
- `serve`, asking an empty queue for work every 20 milliseconds for four seconds, peaks at 10.4 MiB.
- A test runs 11,000 jobs through the lease loop with stand-ins that keep nothing. The Go heap ends at about 220 KiB, where it started, and everything the runtime has taken from the system is about 13 MiB.
- `replay` keeps the whole input and every result in memory in order to print them, so it grows with the file: 20,000 jobs peaked at 32 MiB when last measured, which is over the budget. That is the replay harness, not the loop.

## Not yet done

- **A run against a running Muster.** The client is checked against Muster's saved examples of the contract and a fake server, which is not the same as talking to Muster itself.
- **A way to say "no rule" for an incident or a proposal.** Muster refuses `needs_subtool` for those steps. When Bellbuoy has no rule for one, it writes nothing, logs the reason and lets the lease run out, so Muster offers the step again about once a minute until a rule or a setting covers it. The commonest cause will be a worker with no owner in `ownership_map.json`.
- **An ownership map for the cluster.** Bellbuoy's `ownership_map.json` has its own form, different from Muster's, and must name the repository that owns each worker. Only a made-up one for tests is in this repository.
- **A client for Plainforge.** Plainforge has no interface for tasks yet. Until it does, `serve` keeps tasks in a ledger file and nothing reaches a repository.
- **Proposals.** Muster's contract has Bellbuoy complete an approved proposal to `building`. The edit rule here still expects the shape assumed before that contract, so a proposal leased from Muster is answered with `needs_subtool`.
- **Mirroring and the live stream in `serve`.** Neither has anywhere to deliver to yet.
- **The item-lifecycle and registry streams.** Their states are not written down here. `status` jobs for them are paused.
- **A Kubernetes manifest.** The security settings, the spool volume and the priority class are not written down as a manifest. The priority class has no name in the registry.
- **An occurrence can be counted twice** if Bellbuoy is stopped after the task is counted and before the result is written to the spool.
- **Two identical incidents can be counted once.** Results are staged by item. If a result is still staged, unacknowledged, when Muster raises a second incident for the same item with the same worker, code and state, the second cannot be told from the first being offered again, and is not counted. A second incident that differs is worked on afresh.
- **Alerts are not signed.** The specification does not ask for it.
- **One open task per fingerprint relies on one copy of Bellbuoy running.** Two copies could each open a task for the same new failure.

`SPEC.md` was changed while building, to define things it named and did not define. Each change and its reason is listed at the end of that file.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Bellbuoy does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt the build was started with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
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
