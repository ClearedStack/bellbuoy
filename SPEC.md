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
| **Local Spool** | `/var/spool/cs` (buffers outbound notifications and acks during Muster restarts) |
| **Contract Version** | `CONTRACT_VERSION=v1` (owned by `clearedstack.com`) |

Bellbuoy runs under the Standard Kubernetes priority class at `30 MiB` RAM, yielding before High-priority control-plane pods on a single 4 GiB `k3s` node.

## Interfaces & State Transitions

Bellbuoy pulls leased notification and routing jobs from Muster over cluster-private HTTP using its Kubernetes ServiceAccount identity (`lease`, `heartbeat`, `complete`, `fail`, `needs_subtool`), always draining `urgent` before `normal` and `normal` before `backfill`. It never connects directly to Logbook.

Rather than transitioning `PA-XXXX` items through pipeline stages itself, Bellbuoy reacts to pipeline events and Countersign approvals:

- **Deterministic issue deduplication:** Computes a repeatable fingerprint from the worker name, sanitized error code (`E_TIMEOUT`, `E_PARSE`, `E_NEEDS_SUBTOOL`), and failure pattern. If an open issue with that fingerprint already exists in Plainforge, Bellbuoy increments its occurrence count instead of creating duplicate tickets.
- **Edit-to-repo loop (`ownership_map.json`):** Routes owner-approved anonymous `✎ Edit` proposals (`DATA`, `CONFIG`, `ISSUE` from Countersign)—committing `CONFIG` branches/PRs or `ISSUE` tickets to Plainforge so Keelworks can build/test and Shipkeeper can deploy.
- **Live Portal SSE:** Streams sanitized status updates so an anonymous visitor watches their edit pill move live from `Proposed` → `Approved` → `Building` → `Live`.

## Sub-Tools & Failure Modes

Bellbuoy separates delivery targets into four deterministic sub-tools under `tools/<case>/`:

| Sub-Tool | Responsibility | Target |
| --- | --- | --- |
| `internal-git` | Commits owner-approved Portal edits (`DATA`, `CONFIG`, `ISSUE`), `needs_subtool` tasks, and fingerprinted error issues. | Cluster-private Plainforge repositories. |
| `github-mirror` | Coordinates outbound code mirroring on change so public contributors can inspect commits and issues. | Public GitHub mirror (source code only; zero container images). |
| `webhook-alert` | Dispatches concise owner alerts for genuinely new fingerprinted errors, `P0` crashes/dead-letters, or `urgent` regulatory drift. | Configured external webhook endpoint. |
| `portal-sse` | Pushes sanitized real-time Server-Sent Events through Muster to Conn. | Public Portal UI (`https://clearedstack.com/platform/operations/`, `https://clearedstack.com/platform/item-lifecycle/`, `https://clearedstack.com/platform/#registry`, and live `✎` pills). |

| Error Code | Trigger | Pipeline Behavior |
| --- | --- | --- |
| `E_TIMEOUT` | External webhook endpoint or Plainforge mirror target times out | Result remains staged in `/var/spool/cs` and retries with backoff until delivered. |
| `E_PARSE` | Malformed edit proposal payload or unexpected webhook/Git API response | Refuses to guess; logs sanitized error without corrupting target repo. |
| `E_NEEDS_SUBTOOL` | Unknown edit class or unsupported notification target | Fails closed, pauses task in `needs_subtool`, and surfaces in `P1` attention tier until resolved. |

## Security & Clean-Room Boundaries

- **No raw cluster topology:** Bellbuoy only emits sanitized error codes and fingerprints; Shipkeeper strips internal node names, pod hostnames, and cluster IPs before status reaches `pub.*` or `portal-sse`.
- **No database credentials:** Network policies block direct access to Logbook; all queue reads and state acknowledgements pass through Muster.
- **Source-only mirroring:** `github-mirror` mirrors MIT-licensed source code and issues only—zero container images, secrets, or raw Sextant captures are ever pushed to GitHub.
- **Unprivileged pod posture:** Non-root user, dropped Linux capabilities, no privilege escalation, and read-only root filesystem outside `/var/spool/cs`.

## Related pages

- [The pipeline and its ten workers](https://clearedstack.com/platform/pipeline/)
- [Public edits and what stays private](https://clearedstack.com/platform/public-edits/)
- [An item’s life: fourteen states and three lanes](https://clearedstack.com/platform/item-lifecycle/)
- [Operating it: status views, failures and backups](https://clearedstack.com/platform/operations/)
- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
