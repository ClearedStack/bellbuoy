# Bellbuoy specification

> Cross-cutting routing and notification worker that deduplicates errors by fingerprint, commits approved Portal edits and needs_subtool tasks to Plainforge, and drives owner alerts and live SSE updates.

**Status: design.** This is a specification. It describes how Bellbuoy is meant to work, not something that exists yet.

The same text is published at [clearedstack.com/platform/bellbuoy/](https://clearedstack.com/platform/bellbuoy/). To change the specification, change it here and say why.

## Overview & Role

Bellbuoy is the cross-cutting notification and routing worker in the `cs-workers` namespace (`ownerRepo: Bellbuoy`, version `v0.1.0-draft`). Added in ClearedStack v3.1 to close the edit-to-repo and auto-issue loops, it routes owner-approved Portal edits, `needs_subtool` tasks, and fingerprinted errors into Plainforge, coordinates outbound GitHub mirroring, dispatches owner webhook alerts, and drives live Server-Sent Events (SSE) updates on Conn.

Like all ten workers, Bellbuoy is a 100% deterministic Go service with zero AI at runtime.

## Runtime & Resource Budget

| Property | Value |
| --- | --- |
| **Service / Repo** | Bellbuoy (`bellbuoy`) |
| **Namespace** | `cs-workers` |
| **Kind** | `worker` (100% deterministic Go, zero runtime AI) |
| **Version** | `v0.1.0-draft` |
| **RAM Budget** | `30 MiB` |
| **Sub-Tools** | `internal-git`, `github-mirror`, `webhook-alert`, `portal-sse` |
| **Local Spool** | `/var/spool/cs` (buffers outbound notifications and acks during Muster restarts; one file per unfinished job, replaced whole by rename; at most 256 jobs staged, and no new job is leased while it is full) |
| **Contract Version** | `CONTRACT_VERSION=v1` (owned by `clearedstack.com`) |

Bellbuoy runs under the Standard Kubernetes priority class at `30 MiB` RAM, yielding before High-priority control-plane pods on a single 4 GiB `k3s` node.

## Interfaces & State Transitions

Bellbuoy pulls leased notification and routing jobs from Muster over cluster-private HTTP using its Kubernetes ServiceAccount identity (`lease`, `heartbeat`, `complete`, `fail`, `needs_subtool`), always draining `urgent` before `normal` and `normal` before `backfill`. It never connects directly to Logbook.

The calls are Muster's contract v1, and Muster's copy of that contract is the master. Every call is a `POST` with a JSON body that carries `"contract": "v1"` and only the fields the contract defines for that call. The ServiceAccount token is sent as a bearer token and is read from its file for every call, because the cluster replaces it while the pod runs. Bellbuoy follows no redirect and uses no proxy, so the token goes to the configured address and nowhere else. `POST /v1/lease` answers `200` with `lease`, `kind`, `id`, `state`, `lane`, `attempt`, `expires` and what the step is about, or `204` when there is nothing to do, after which Bellbuoy waits and asks again. Muster chooses the lane: it offers `urgent` first, then `normal`, then `backfill`. A refusal has the form `{error, cause, reason}`. Bellbuoy acts on the `error` code and on two causes, `lease_not_held` and `move_not_allowed`, and repeats none of it in its own errors or logs.

Muster's saved examples of the contract are copied into `testdata/contract/`, and `internal/muster/contract_test.go` replays every saved `.json` example: Bellbuoy's client sends the exact request in each worker example, reads every response and refusal the way `CONTRACT.md` means it, and exposes no non-worker calls.

Bellbuoy keeps a held lease alive with `heartbeat` every 20 seconds. It writes the result of a step to the spool before telling Muster, answers with `complete`, `fail` or `needs_subtool`, and removes the result from the spool only when Muster answers `200`.

Bellbuoy works on one job at a time. That makes the order of everything it does a function of the order of its input, and makes the open-or-add-to rule below safe without a lock.

A piece of work in a Plainforge repository is called a **task** in the naming registry and in the code. Where this page says issue or ticket, it means a task.

Rather than transitioning `PA-XXXX` items through pipeline stages itself, Bellbuoy reacts to pipeline events and Countersign approvals:

- **Deterministic issue deduplication:** Computes a repeatable fingerprint from the worker name, sanitized error code (`E_TIMEOUT`, `E_PARSE`, `E_NEEDS_SUBTOOL`), and failure pattern. If an open issue with that fingerprint already exists in Plainforge, Bellbuoy increments its occurrence count instead of creating duplicate tickets. A closed issue does not count: the same failure coming back opens a new one.
  - *The fingerprint* is the SHA-256, in lower-case hex, of four parts in this order: the text `bellbuoy/fingerprint/v1`, the worker name, the error code, and the sanitized failure pattern. Each part is written as its length in bytes (eight bytes, big-endian) followed by its bytes.
  - *The sanitized failure pattern* is made from the failure message like this: cut it to 4096 bytes; split it on white space; trim the characters ``. , ; : ! ? ( ) [ ] { } < > " ' ` `` from both ends of each word and drop words that are then empty; keep a word in lower case if it is 1 to 24 letters `a` to `z` in either case, and replace any other word with `#`; collapse neighbouring `#` into one; keep at most 32 words; join them with single spaces. Numbers, addresses, host names, paths and identifiers all become `#`, so two failures that differ only in those share a fingerprint.
  - An error code that is shaped like a code (`E_` and capitals) and is not one of the three is `E_NEEDS_SUBTOOL`. Anything else in that field is `E_PARSE`.
- **Edit-to-repo loop (`ownership_map.json`):** Routes owner-approved anonymous `✎ Edit` proposals (`DATA`, `CONFIG`, `ISSUE` from Countersign), committing `DATA` and `CONFIG` branches/PRs or `ISSUE` tickets to Plainforge so Keelworks can build/test and Shipkeeper can deploy.
  - A `DATA` or `CONFIG` edit carries the whole new text of one file. It is committed on the branch `edit/<edit reference>` with a request to merge it. Bellbuoy never writes to a default branch. The same edit delivered again changes nothing.
  - An `ISSUE` edit carries a summary and becomes one task. Delivered again, it is still one task.
  - An edit with no approval reference, or a proposal leased outside the `approved` state, is `E_PARSE`. Bellbuoy acts only on approved edits and proposals.
  - `ownership_map.json` has up to four parts. `elements` maps an editable Portal element identifier to `{"repository", "file", "kind"}` as in Muster's contract v1. `repos` lists every repository Bellbuoy may write to and whether it has a public mirror: `{"muster": {"mirror": true}}`. `workers` maps a worker's name to the repository that owns its failures: `{"muster": "muster"}`. `paths` maps the target of a path-based edit to a repository: `[{"prefix": "platform/", "repo": "clearedstack.com"}]`. When a map supplies only `elements`, `repos` and `workers` default to the ClearedStack naming registry. A worker or target with no entry is `E_NEEDS_SUBTOOL`. A map that is malformed in any way is refused whole.
- **Live Portal SSE:** Streams sanitized status updates so an anonymous visitor watches their edit pill move live from `Proposed` → `Approved` → `Building` → `Live`.

### Job kinds

A lease from Muster names the kind of step. Bellbuoy has rules for both kinds that Muster offers to `bellbuoy` in contract v1:

| Kind | What the lease carries | What Bellbuoy does |
| --- | --- | --- |
| `incident` | `incident`, with `id` (the item, `PA-` and four digits, or five to nine digits not starting with zero), `worker` (the worker that stopped), `code` (`E_PARSE` for a held item, `E_NEEDS_SUBTOOL` for a paused one) and `state` (the state the item was in when the worker stopped) | Opens or adds to the fingerprinted task in that worker's repository, then answers `complete` with the state it was given and no result, as the contract requires of an incident. |
| `proposal` | `proposal`, with `target`, `repository`, `file`, `edit_kind` (`DATA`, `CONFIG` or `ISSUE`), `change` and `reason`, plus optional `results` from Countersign; leased in `state: "approved"` with `id` `EP-XXXX` | Proposes a branch `edit/<id>` for `DATA`/`CONFIG` or opens one task for `ISSUE` in `repository` (recovering `file` from `elements` in `ownership_map.json` when Muster has restarted over PostgreSQL 17), then answers `complete` with `state: "building"` and the recorded change or task as `result`. |

Muster gives no failure message with an incident, so the failure pattern is built from what it does give: `held in` for `E_PARSE` or `paused in` for `E_NEEDS_SUBTOOL`, followed by the state with underscores read as spaces. Items that the same worker left the same way in the same state share one task.

An incident or proposal that contradicts itself, contradicts `ownership_map.json`, or is malformed is answered with `fail` and `E_PARSE`.

Muster lets only an item's own step be paused. It refuses `needs_subtool` for an incident or a proposal. So when Bellbuoy has no rule for such a step (a kind it does not know, a field it does not know, a worker with no owner in the ownership map) it cannot pause it, and it does not pretend the step failed or succeeded either. It writes nothing, keeps nothing, logs the reason and lets the lease run out. Muster offers the step again one lease length later and Bellbuoy looks at it afresh, so the step is dealt with as soon as the rule or the setting exists.

The kinds below are not in Muster's contract. They were assumed before the contract existed, and today only the replay mode and the in-memory stand-in use them, as `{"id", "lane", "kind", "payload"}`. They stay until Muster offers the same work under its own names.

| Kind | Payload | What Bellbuoy does |
| --- | --- | --- |
| `error`, `crash`, `dead_letter` | `worker`, `code`, `pattern`, optional `item`, optional `tier` (`P0` to `P9`) | Opens or adds to the fingerprinted task in the worker's repository. |
| `needs_subtool` | `worker`, `pattern`, optional `code` (only `E_NEEDS_SUBTOOL`), optional `item`, optional `tier` | The same, with the code `E_NEEDS_SUBTOOL`. |
| `edit` | `edit`, `class`, `target`, `approval`, and `content` (`DATA`, `CONFIG`) or `summary` (`ISSUE`) | Commits the approved edit. |
| `status` | `topic`, `edit`, `state` | Moves an edit pill on the live stream. |
| `drift` | `ref` (an opaque reference to the rule that changed) | Alerts the owner, in the `urgent` lane only. |
| `mirror` | `repo`, `content` (a list of `source`, `task`) | Brings a public mirror up to date. |

References (`id`, `edit`, `approval`, `ref`) are 1 to 64 letters, digits, dots, underscores and hyphens. A `target` is either an element identifier (`catalog.sample-calculator.description`) for a `proposal` or a plain relative path with no `.` or `..` part for an `edit`.

## Sub-Tools & Failure Modes

Bellbuoy separates delivery targets into four deterministic sub-tools under `tools/<case>/`, listed in `tools/registry.yaml`. Each has a matching rule, and every sub-tool whose rule matches a job runs, in the order of the table. `internal-git` runs first, and the others decide what to do from the task or change it records: while it is waiting on Plainforge, they wait too. A job that no rule matches is `E_NEEDS_SUBTOOL`.

| Sub-Tool | Responsibility | Target |
| --- | --- | --- |
| `internal-git` | Commits owner-approved Portal proposals and edits (`DATA`, `CONFIG`, `ISSUE`), `needs_subtool` tasks, and fingerprinted error issues. | Cluster-private Plainforge repositories. |
| `github-mirror` | Coordinates outbound code mirroring on change so public contributors can inspect commits and issues. It asks Plainforge to bring the mirror up to date; Bellbuoy holds no credential for GitHub and pushes nothing there itself. It runs after `internal-git` writes to a repository that has a mirror, and for every `mirror` job. Only `source` and `task` content may be named. A job that names anything else is paused whole. | Public GitHub mirror (source code only; zero container images). |
| `webhook-alert` | Dispatches concise owner alerts for genuinely new fingerprinted errors, `P0` crashes/dead-letters, or `urgent` regulatory drift. A repeat of a known failure alerts nobody. The alert is one JSON object posted to the webhook with no free text in it: `source`, `kind`, `lane`, `tier`, `new`, `worker`, `code`, `fingerprint`, `occurrences`, `task`, `ref`. Redirects are not followed. With no webhook configured, a job that needs an alert is `E_NEEDS_SUBTOOL`. | Configured external webhook endpoint. |
| `portal-sse` | Pushes sanitized real-time Server-Sent Events through Muster to Conn. Muster's contract v1 emits proposal `building`/`live` events directly from Muster's `/v1/feed` when `complete` or `/v1/health` is called, and has no separate worker event call, so in the cluster this sub-tool has nowhere separate to deliver yet and is not run; it works against the in-memory stand-in. Two streams have rules. `pill` carries an edit reference and one of `Proposed`, `Approved`, `Building`, `Live`; Bellbuoy sends `Building` for a `proposal`, `Approved` for a legacy `edit`, and passes on the others from `status` jobs. `operations` carries a worker, a code, a fingerprint and a count, sent each time a failure is counted. An event has no free text. The streams for the item-lifecycle and registry pages have no rule yet, because their states are not written down here; a `status` job for them is `E_NEEDS_SUBTOOL`. | Public Portal UI (`https://clearedstack.com/platform/operations/`, `https://clearedstack.com/platform/item-lifecycle/`, `https://clearedstack.com/platform/#registry`, and live `✎` pills). |

| Error Code | Trigger | Pipeline Behavior |
| --- | --- | --- |
| `E_TIMEOUT` | External webhook endpoint or Plainforge mirror target times out. Plainforge or Muster not answering counts too, as does a webhook answer of 408, 429 or 5xx. | Result remains staged in `/var/spool/cs` and retries with backoff until delivered. The job stays leased, kept alive with `heartbeat` every 20 seconds, and is answered with `complete` only when everything is delivered. The delay is one second, doubling after each try without progress, to at most five minutes, with no random part. Sub-tools that already finished are not run again, on retry, after a restart, or if Muster hands the job out again. |
| `E_PARSE` | Malformed edit proposal payload or unexpected webhook/Git API response. A webhook answer that is not 2xx and not one of the retry answers above, a redirect included, counts as unexpected. | Refuses to guess; logs sanitized error without corrupting target repo. Every check is made before anything is written. The job is answered with `fail` and the code `E_PARSE`, and is not retried. |
| `E_NEEDS_SUBTOOL` | Unknown edit class or unsupported notification target. Also: a job kind, payload field, error code, stream, edit state or repository that no rule covers. | Fails closed, pauses task in `needs_subtool`, and surfaces in `P1` attention tier until resolved. The job is answered with `needs_subtool` and the rest of the queue carries on. Where Muster does not allow a step to be paused, the step is left for Muster to offer again, as described under Job kinds. |

The result of every step is written to `/var/spool/cs` before Muster is told how the step ended, and is removed only when Muster answers `200`. If Muster cannot be reached, or refuses the answer, the result stays and the answer is sent again later. If the lease runs out meanwhile and Muster hands the same step out again, the staged result goes back under the new lease. The work is not done twice. If Muster has replaced the step with a newer incident for the same item, the staged result is dropped and the newer incident is worked on afresh.

Bellbuoy's own failures come back to it as failures of the worker `bellbuoy`. They are fingerprinted like any other, so they settle into one task per kind of failure and do not loop.

## Security & Clean-Room Boundaries

- **No raw cluster topology:** Bellbuoy only emits sanitized error codes and fingerprints; Shipkeeper strips internal node names, pod hostnames, and cluster IPs before status reaches `pub.*` or `portal-sse`. Bellbuoy does not rely on that: it sanitizes everything it emits itself, whatever was stripped upstream. A failure message reaches a task only as its sanitized pattern, and reaches an alert, an event or a log line not at all. Errors carry one of the three codes and a reason fixed in the source, never text from outside. The webhook address is treated as a credential and appears in no log and no error.
- **No database credentials:** Network policies block direct access to Logbook; all queue reads and state acknowledgements pass through Muster.
- **Source-only mirroring:** `github-mirror` mirrors MIT-licensed source code and issues only: zero container images, secrets, or raw Sextant captures are ever pushed to GitHub.
- **Unprivileged pod posture:** Non-root user, dropped Linux capabilities, no privilege escalation, and read-only root filesystem outside `/var/spool/cs`.

## Related pages

- [The pipeline and its ten workers](https://clearedstack.com/platform/pipeline/)
- [Public edits and what stays private](https://clearedstack.com/platform/public-edits/)
- [An item’s life: fourteen states and three lanes](https://clearedstack.com/platform/item-lifecycle/)
- [Operating it: status views, failures and backups](https://clearedstack.com/platform/operations/)
- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)

## Changes to this specification

Made while building Bellbuoy, each with its reason. Nothing above was removed.

| Change | Reason |
| --- | --- |
| Added `publish` to the Muster calls, then took it out again. | `portal-sse` pushes events "through Muster", and none of the five calls listed could carry one. Muster's contract v1 has since been written, with five calls and no `publish`, and Muster's contract is the master. |
| Described the five calls as Muster's contract v1 defines them, and added the `incident` and `proposal` kinds. | The contract defines `incident` and `proposal` as the two step kinds leased to `bellbuoy`. |
| Marked the other job kinds as assumptions used only by replay and the stand-in. | They were written before Muster's contract and are not in it. |
| Said that issue, ticket and task are the same thing. | This page used the first two; the naming registry and `component.json` use "task". The registry wins. |
| Wrote down the fingerprint recipe and how a failure pattern is sanitized. | The page named the three inputs and nothing else. Two builds could not have agreed on a fingerprint. |
| Said what happens to a `DATA` edit, and that edits are committed on a branch. | `DATA` was listed for `internal-git` and then left out of the sentence about what is committed. |
| Described `ownership_map.json`, including Muster's `elements` table. | Supports both Muster's `{"elements": {...}}` map and the cluster map in `deploy/bellbuoy.yaml`, and recovers `file` after a Muster restart over PostgreSQL 17. |
| Added the table of job kinds. | When it was added the Muster v1 contract did not exist, and Bellbuoy could not be built or tested without some shape for a job. |
| Said that `github-mirror` asks Plainforge to do the mirroring. | The page did not say who pushes. The failure table speaks of a "Plainforge mirror target", and keeping the GitHub credential out of Bellbuoy is the smaller surface. |
| Said what an alert and an event contain, and which streams have rules. | Neither had a format. The states for the item-lifecycle and registry streams are defined on other pages, so those two are paused here instead of guessed. |
| Said how each error code ends the job, and gave the retry delays and the webhook answer rule. | The table said what triggers a code and not what Bellbuoy then tells Muster, how long it waits, or which webhook answers mean "try again". |
| Said that Bellbuoy sanitizes its own output. | One sentence said Bellbuoy emits only sanitized output and the next gave the stripping to Shipkeeper. Both now hold: Bellbuoy does not depend on the second. |
| Said that one job is worked on at a time, and that the spool holds at most 256. | Needed for the same input to give the same output, and for the 30 MiB budget to hold when something outside is down. |
| Said that Bellbuoy's own failures do not loop. | Stage 10 handles every worker's failures, its own included. The page did not say why that ends. |

Still open, and not decided here:

- How Bellbuoy says "no rule" for a step that is not an item's own. Muster refuses `needs_subtool` for incidents and proposals, so such a step is offered again every lease length until a rule exists. A call or a state for this would stop the repeats.
- Where a task or proposed branch goes in Plainforge. Plainforge currently serves single-repository Git smart HTTP and does not yet expose multi-repository task, branch-proposal or mirror endpoints, so in `serve` Bellbuoy records tasks and proposed changes durably in `tasks.ledger` in `/var/spool/cs`. `github-mirror` waits on that Plainforge interface.
- How `operations` live events reach Conn. Proposal `building` and `live` events are emitted by Muster directly from `complete` and `/v1/health`, while Muster's contract v1 has no worker call for `operations` events.
- The name of the Kubernetes priority class. This page says "Standard"; the registry has no identifier for it.
- The names of settings. The build uses one, `BELLBUOY_WEBHOOK_URL`, which is not in the registry.
- Whether an alert should be signed. Nothing here asks for it, so it is not.
- An occurrence can be counted twice if Bellbuoy is stopped after Plainforge counts it and before the job is staged or answered. Closing that needs Plainforge to accept a key per occurrence.
