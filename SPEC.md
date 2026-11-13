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

Bellbuoy pulls leased notification and routing jobs from Muster over cluster-private HTTP using its Kubernetes ServiceAccount identity (`lease`, `heartbeat`, `complete`, `fail`, `needs_subtool`, `publish`), always draining `urgent` before `normal` and `normal` before `backfill`. It never connects directly to Logbook. `publish` hands one event for the live stream to Muster; it is how `portal-sse` reaches Conn.

Bellbuoy works on one job at a time. That makes the order of everything it does a function of the order of its input, and makes the open-or-add-to rule below safe without a lock.

A piece of work in a Plainforge repository is called a **task** in the naming registry and in the code. Where this page says issue or ticket, it means a task.

Rather than transitioning `PA-XXXX` items through pipeline stages itself, Bellbuoy reacts to pipeline events and Countersign approvals:

- **Deterministic issue deduplication:** Computes a repeatable fingerprint from the worker name, sanitized error code (`E_TIMEOUT`, `E_PARSE`, `E_NEEDS_SUBTOOL`), and failure pattern. If an open issue with that fingerprint already exists in Plainforge, Bellbuoy increments its occurrence count instead of creating duplicate tickets. A closed issue does not count: the same failure coming back opens a new one.
  - *The fingerprint* is the SHA-256, in lower-case hex, of four parts in this order: the text `bellbuoy/fingerprint/v1`, the worker name, the error code, and the sanitized failure pattern. Each part is written as its length in bytes (eight bytes, big-endian) followed by its bytes.
  - *The sanitized failure pattern* is made from the failure message like this: cut it to 4096 bytes; split it on white space; trim the characters ``. , ; : ! ? ( ) [ ] { } < > " ' ` `` from both ends of each word and drop words that are then empty; keep a word in lower case if it is 1 to 24 letters `a`–`z` in either case, and replace any other word with `#`; collapse neighbouring `#` into one; keep at most 32 words; join them with single spaces. Numbers, addresses, host names, paths and identifiers all become `#`, so two failures that differ only in those share a fingerprint.
  - An error code that is shaped like a code (`E_` and capitals) and is not one of the three is `E_NEEDS_SUBTOOL`. Anything else in that field is `E_PARSE`.
- **Edit-to-repo loop (`ownership_map.json`):** Routes owner-approved anonymous `✎ Edit` proposals (`DATA`, `CONFIG`, `ISSUE` from Countersign)—committing `DATA` and `CONFIG` branches/PRs or `ISSUE` tickets to Plainforge so Keelworks can build/test and Shipkeeper can deploy.
  - A `DATA` or `CONFIG` edit carries the whole new text of one file. It is committed on the branch `edit/<edit reference>` with a request to merge it. Bellbuoy never writes to a default branch. The same edit delivered again changes nothing.
  - An `ISSUE` edit carries a summary and becomes one task. Delivered again, it is still one task.
  - An edit with no approval reference is `E_PARSE`. Bellbuoy acts only on approved edits.
  - `ownership_map.json` has three parts. `repos` lists every repository Bellbuoy may write to and whether it has a public mirror: `{"muster": {"mirror": true}}`. `workers` maps a worker's name to the repository that owns its failures: `{"muster": "muster"}`. `paths` maps the target of an edit to a repository: `[{"prefix": "platform/", "repo": "clearedstack.com"}]`. A prefix ending in `/` covers everything under it, any other prefix covers that one file, and the longest matching prefix wins. A worker or target with no entry is `E_NEEDS_SUBTOOL`. A map that is malformed in any way is refused whole.
- **Live Portal SSE:** Streams sanitized status updates so an anonymous visitor watches their edit pill move live from `Proposed` → `Approved` → `Building` → `Live`.

### Job kinds

The Muster v1 contract is not published in this repository. Until it is, Bellbuoy assumes a job is `{"id", "lane", "kind", "payload"}` and that these kinds exist. A kind not listed here, or a payload field not listed here, is `E_NEEDS_SUBTOOL`.

| Kind | Payload | What Bellbuoy does |
| --- | --- | --- |
| `error`, `crash`, `dead_letter` | `worker`, `code`, `pattern`, optional `item` (`PA-XXXX`), optional `tier` (`P0`–`P9`) | Opens or adds to the fingerprinted task in the worker's repository. |
| `needs_subtool` | `worker`, `pattern`, optional `code` (only `E_NEEDS_SUBTOOL`), optional `item`, optional `tier` | The same, with the code `E_NEEDS_SUBTOOL`. |
| `edit` | `edit`, `class`, `target`, `approval`, and `content` (`DATA`, `CONFIG`) or `summary` (`ISSUE`) | Commits the approved edit. |
| `status` | `topic`, `edit`, `state` | Moves an edit pill on the live stream. |
| `drift` | `ref` (an opaque reference to the rule that changed) | Alerts the owner, in the `urgent` lane only. |
| `mirror` | `repo`, `content` (a list of `source`, `task`) | Brings a public mirror up to date. |

References (`id`, `edit`, `approval`, `ref`) are 1 to 64 letters, digits, dots, underscores and hyphens. A `target` is a plain relative path with no `.` or `..` part.

## Sub-Tools & Failure Modes

Bellbuoy separates delivery targets into four deterministic sub-tools under `tools/<case>/`, listed in `tools/registry.yaml`. Each has a matching rule, and every sub-tool whose rule matches a job runs, in the order of the table. `internal-git` runs first, and the others decide what to do from the task or change it records: while it is waiting on Plainforge, they wait too. A job that no rule matches is `E_NEEDS_SUBTOOL`.

| Sub-Tool | Responsibility | Target |
| --- | --- | --- |
| `internal-git` | Commits owner-approved Portal edits (`DATA`, `CONFIG`, `ISSUE`), `needs_subtool` tasks, and fingerprinted error issues. | Cluster-private Plainforge repositories. |
| `github-mirror` | Coordinates outbound code mirroring on change so public contributors can inspect commits and issues. It asks Plainforge to bring the mirror up to date; Bellbuoy holds no credential for GitHub and pushes nothing there itself. It runs after `internal-git` writes to a repository that has a mirror, and for every `mirror` job. Only `source` and `task` content may be named. A job that names anything else is paused whole. | Public GitHub mirror (source code only; zero container images). |
| `webhook-alert` | Dispatches concise owner alerts for genuinely new fingerprinted errors, `P0` crashes/dead-letters, or `urgent` regulatory drift. A repeat of a known failure alerts nobody. The alert is one JSON object posted to the webhook with no free text in it: `source`, `kind`, `lane`, `tier`, `new`, `worker`, `code`, `fingerprint`, `occurrences`, `task`, `ref`. Redirects are not followed. With no webhook configured, a job that needs an alert is `E_NEEDS_SUBTOOL`. | Configured external webhook endpoint. |
| `portal-sse` | Pushes sanitized real-time Server-Sent Events through Muster to Conn, with Muster's `publish`. Two streams have rules. `pill` carries an edit reference and one of `Proposed`, `Approved`, `Building`, `Live`; Bellbuoy sends `Approved` itself when it commits an edit, and passes on the others from `status` jobs. `operations` carries a worker, a code, a fingerprint and a count, sent each time a failure is counted. An event has no free text. The streams for the item-lifecycle and registry pages have no rule yet, because their states are not written down here; a `status` job for them is `E_NEEDS_SUBTOOL`. | Public Portal UI (`https://clearedstack.com/platform/operations/`, `https://clearedstack.com/platform/item-lifecycle/`, `https://clearedstack.com/platform/#registry`, and live `✎` pills). |

| Error Code | Trigger | Pipeline Behavior |
| --- | --- | --- |
| `E_TIMEOUT` | External webhook endpoint or Plainforge mirror target times out. Plainforge or Muster not answering counts too, as does a webhook answer of 408, 429 or 5xx. | Result remains staged in `/var/spool/cs` and retries with backoff until delivered. The job stays leased, kept alive with `heartbeat`, and is answered with `complete` only when everything is delivered. The delay is one second, doubling after each try without progress, to at most five minutes, with no random part. Sub-tools that already finished are not run again, on retry, after a restart, or if Muster hands the job out again. |
| `E_PARSE` | Malformed edit proposal payload or unexpected webhook/Git API response. A webhook answer that is not 2xx and not one of the retry answers above, a redirect included, counts as unexpected. | Refuses to guess; logs sanitized error without corrupting target repo. Every check is made before anything is written. The job is answered with `fail` and is not retried. |
| `E_NEEDS_SUBTOOL` | Unknown edit class or unsupported notification target. Also: a job kind, payload field, error code, stream, edit state or repository that no rule covers. | Fails closed, pauses task in `needs_subtool`, and surfaces in `P1` attention tier until resolved. The job is answered with `needs_subtool` and the rest of the queue carries on. |

If Muster cannot be told how a job ended, the answer is staged in `/var/spool/cs` and sent when Muster is back. The work is not done twice.

Bellbuoy's own failures come back to it as failures of the worker `bellbuoy`. They are fingerprinted like any other, so they settle into one task per kind of failure and do not loop.

## Security & Clean-Room Boundaries

- **No raw cluster topology:** Bellbuoy only emits sanitized error codes and fingerprints; Shipkeeper strips internal node names, pod hostnames, and cluster IPs before status reaches `pub.*` or `portal-sse`. Bellbuoy does not rely on that: it sanitizes everything it emits itself, whatever was stripped upstream. A failure message reaches a task only as its sanitized pattern, and reaches an alert, an event or a log line not at all. Errors carry one of the three codes and a reason fixed in the source, never text from outside. The webhook address is treated as a credential and appears in no log and no error.
- **No database credentials:** Network policies block direct access to Logbook; all queue reads and state acknowledgements pass through Muster.
- **Source-only mirroring:** `github-mirror` mirrors MIT-licensed source code and issues only—zero container images, secrets, or raw Sextant captures are ever pushed to GitHub.
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
| Added `publish` to the Muster calls. | `portal-sse` pushes events "through Muster", and none of the five calls listed could carry one. |
| Said that issue, ticket and task are the same thing. | This page used the first two; the naming registry and `component.json` use "task". The registry wins. |
| Wrote down the fingerprint recipe and how a failure pattern is sanitized. | The page named the three inputs and nothing else. Two builds could not have agreed on a fingerprint. |
| Said what happens to a `DATA` edit, and that edits are committed on a branch. | `DATA` was listed for `internal-git` and then left out of the sentence about what is committed. |
| Described `ownership_map.json`. | The file was named with no contents. The `repos` part with its `mirror` flag is new: something has to say which repositories have a public mirror. |
| Added the table of job kinds. | The Muster v1 contract is not in this repository, and Bellbuoy cannot be built or tested without some shape for a job. This table is an assumption to be replaced by the contract. |
| Said that `github-mirror` asks Plainforge to do the mirroring. | The page did not say who pushes. The failure table speaks of a "Plainforge mirror target", and keeping the GitHub credential out of Bellbuoy is the smaller surface. |
| Said what an alert and an event contain, and which streams have rules. | Neither had a format. The states for the item-lifecycle and registry streams are defined on other pages, so those two are paused here instead of guessed. |
| Said how each error code ends the job, and gave the retry delays and the webhook answer rule. | The table said what triggers a code and not what Bellbuoy then tells Muster, how long it waits, or which webhook answers mean "try again". |
| Said that Bellbuoy sanitizes its own output. | One sentence said Bellbuoy emits only sanitized output and the next gave the stripping to Shipkeeper. Both now hold: Bellbuoy does not depend on the second. |
| Said that one job is worked on at a time, and that the spool holds at most 256. | Needed for the same input to give the same output, and for the 30 MiB budget to hold when something outside is down. |
| Said that Bellbuoy's own failures do not loop. | Stage 10 handles every worker's failures, its own included. The page did not say why that ends. |

Still open, and not decided here:

- The name of the Kubernetes priority class. This page says "Standard"; the registry has no identifier for it.
- The names of settings. The build uses one, `BELLBUOY_WEBHOOK_URL`, which is not in the registry.
- Whether an alert should be signed. Nothing here asks for it, so it is not.
- An occurrence can be counted twice if Bellbuoy is stopped after Plainforge counts it and before the job is staged or answered. Closing that needs Plainforge to accept a key per occurrence.
