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

> **Status: design.** The first milestone is built and tested: the fingerprint, and the rule that the same failure seen again adds to one task. Everything else in the specification is not built. There is no worker to deploy yet.

Bellbuoy is stage 10 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Bellbuoy is meant to be understood and used without the rest.

## What works today

Two things, tested against an in-memory stand-in for Plainforge. Nothing here talks to a network.

1. **The fingerprint.** A failure is three values: the worker name, one of the three public error codes (`E_TIMEOUT`, `E_PARSE`, `E_NEEDS_SUBTOOL`), and a failure pattern. The fingerprint is the SHA-256 of those three, as 64 hexadecimal characters. The same three values give the same fingerprint on any machine. A worker name or pattern must be a plain name (lowercase letters, digits, single hyphens, at most 64 characters); anything else is refused, not repaired.
2. **Open a task or add to it.** For each failure, Bellbuoy looks for an open task with that fingerprint. If there is one, its occurrence count goes up by one. If there is none, a task is opened with a count of one. A closed task is never added to; the next failure opens a new one. A saved example shows two hundred identical failures becoming one task with a count of 200.

What happens to an item that is not a good failure report:

| Item | Result | Code |
| --- | --- | --- |
| Broken: not one JSON object, a missing field, a malformed name | Refused. Nothing is written. | `E_PARSE` |
| Valid but with no rule: another kind of item, an extra field, an error code that is not one of the three | Paused. Nothing is written. The rest carry on. | `E_NEEDS_SUBTOOL` |
| The task store takes too long | Not delivered. Nothing is written. | `E_TIMEOUT` |

A result never repeats text from the item or from the task store.

### Run it

You need Go 1.24 or later. There are no other dependencies.

```sh
go test ./...
```

To try it by hand, give `cmd/replay` one failure per line. It prints what happened to each line and the tasks left at the end:

```sh
go run ./cmd/replay < tools/internal-git/fixtures/two-hundred-identical/input.jsonl
```

`cmd/replay` is a way to try the milestone. It is not the Bellbuoy worker.

### Saved examples

| Where | What it shows |
| --- | --- |
| `internal/fingerprint/testdata/vectors.json` | The exact fingerprint for good values and the exact error for bad ones. |
| `tools/internal-git/fixtures/two-hundred-identical/` | Two hundred identical failures become one task. |
| `tools/internal-git/fixtures/distinct-failures/` | A different worker, code or pattern gets its own task. Key order and spacing do not matter. |
| `tools/internal-git/fixtures/unfamiliar-is-paused/` | Valid items with no rule are paused and the rest carry on. |
| `tools/internal-git/fixtures/broken-is-refused/` | Broken items are refused and the rest carry on. |

Each fixture folder holds `input.jsonl` and the exact `expected.json`. After a deliberate change in behaviour, `go test ./... -update` rewrites the expected files; read the diff before committing it.

### Layout

| Path | What it is |
| --- | --- |
| `internal/errcode` | The three public error codes and the only error type shown outside. |
| `internal/fingerprint` | The fingerprint function. |
| `internal/plainforge` | The small interface to Plainforge’s tasks, and the in-memory stand-in. |
| `tools/registry.yaml` | The list of sub-tools that have code. |
| `tools/internal-git` | The `internal-git` sub-tool: matching rule (`match.go`), logic (`errortask.go`), fixtures. Partial. |
| `internal/replay`, `cmd/replay` | Runs saved items through the sub-tool, for the fixture tests and for trying it by hand. |

## Not yet done

Everything in `SPEC.md` apart from the two things above. In particular:

- Leasing work from Muster (`lease`, `heartbeat`, `complete`, `fail`, `needs_subtool`) and draining `urgent`, then `normal`, then `backfill`. There is no Muster interface or stand-in yet.
- The rest of `internal-git`: approved edits (`DATA`, `CONFIG`, `ISSUE`), `ownership_map.json`, tasks for paused `needs_subtool` items, commits, branches and pull requests. Choosing which repository a task belongs in is not built; the stand-in is one repository’s tasks.
- `github-mirror`, `webhook-alert` and `portal-sse`. None is started.
- The spool at `/var/spool/cs` and retry with backoff. A timeout is reported and nothing is retried.
- Reporting a paused item to Muster and the `P1` attention tier. A paused item is only reported in the result.
- The worker itself: no service, no `ko` image, no Kubernetes manifests. The 30 MiB memory limit and the unprivileged, read-only pod have not been measured or exercised.
- Keeping one task per fingerprint when more than one copy of Bellbuoy runs. The rule is safe inside one process only.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Bellbuoy does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
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
