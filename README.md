<div align="center">

# zygote

**The flight recorder for AI agents.**
Record any agent run, replay it exactly, fork it at the step where it went wrong.

[![Go Reference](https://pkg.go.dev/badge/github.com/mindfire/zygote.svg)](https://pkg.go.dev/github.com/mindfire/zygote)
[![Go Report Card](https://goreportcard.com/badge/github.com/mindfire/zygote)](https://goreportcard.com/report/github.com/mindfire/zygote)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
![Go 1.24+](https://img.shields.io/badge/go-1.24%2B-00ADD8.svg)
![Dependencies](https://img.shields.io/badge/dependencies-0-brightgreen.svg)

</div>

---

You changed your agent's code. Did anything break?

```
step 0  import     match
step 1  plan       match
step 2  patch      DIVERGED

  trace: replay diverged from recording at step 2 ("patch")
         recorded 615807ff, replayed 32bd0271

  what actually differs at that step:
    ~ src/fetch.go

  the model never changed — replay isolated the code change that did.
```

That is real output from `go run ./cmd/zygdemo`, not a mockup. The model was held
constant by replaying its recorded answers, so the only thing that could have
moved the world hash was the agent's own logic — and you get the step and the
file.

> **Status: v0.x prototype.** The library works, is tested under `-race`, and has
> zero dependencies. The `zyg` CLI, the language-independent harness, and
> redaction are specified but **not built** — see [Project status](#project-status).
> The recording format will break before v1.

---

## The problem

An agent run is a one-way street. It mutates a directory, calls a
nondeterministic model, and leaves you with a result and no way back.

- You tweak a prompt. **Did behaviour change?** You cannot tell, because
  re-running calls a nondeterministic model against a repo that has moved on.
- A user reports a bad run. **You cannot reproduce it.**
- Your evals are flaky and you **cannot tell whether that is your agent or the
  model.**
- Step 40 goes wrong and your only options are start over or clean up by hand.

There is no `git bisect` for agents. That is the gap zygote fills.

## What it does

Records what an agent did — every step, the exact state of its world at each
step, and every nondeterministic answer it received — into a single file that
replays anywhere, offline, with no model API and no network.

```go
r := trace.Record(store)
r.Set("model", "claude-opus-5")

// Inputs: state the agent consumes but does not compute.
// Carried in the recording, so replay never needs the original repo.
r.Input("checkout", func(w *vfs.World) error {
    _, err := localdir.Import(w, "./repo", nil)
    return err
})

// Effects: nondeterministic answers. Recorded now, substituted on replay.
plan, _ := r.Model("plan the fix", callTheModel)
r.World().WriteFile("PLAN.md", []byte(plan), 0o644)
r.Step("plan")

trace.ExportFile("run.zip", store, r.Recording())
```

Later, on any machine:

```go
rec, _, _ := trace.ImportFile("run.zip", vfs.NewMemStore())
replay := trace.Replaying(store, rec)

err := myAgent(replay)   // same code, zero model calls, no network
// err names the first step whose world differs, if any
```

## Install

```bash
go get github.com/mindfire/zygote
```

Or run the end-to-end demonstration without installing anything:

```bash
git clone https://github.com/mindfire/zygote && cd zygote
go test -race ./...
go run ./cmd/zygdemo
```

The demo records an agent against a real directory, forks a step three ways in
parallel, applies the winner back to disk, bundles the run to a 7 KB zip,
imports it into an empty store on a simulated other machine, replays every step
with zero model calls, then changes the agent's code and reports divergence at
the exact step with a file-level diff.

## What you get

|  |  |
|---|---|
| **Reproduce any run** | On any machine, offline, from a file you can attach to an issue. |
| **Bisect behaviour changes** | Hold the model constant, change your code, learn which step first differs. |
| **Fork any step** | Independent worlds exploring different continuations in parallel — 8 ns per fork, no containers. |
| **Rewind** | Restore any step exactly after a bad tool call, instead of starting over. |
| **Deterministic evals** | The nondeterminism is on disk, so the eval measures your agent. |
| **`io/fs` native** | `*vfs.World` implements `fs.FS`, `fs.ReadFileFS`, `fs.ReadDirFS`, `fs.StatFS`. Passes `fstest.TestFS`. |

## GitHub Action

`zyg verify` runs as a GitHub Action: it replays a committed corpus of recordings on every pull request,
comments the first diverging step of each recording, and fails the job on any unapproved divergence.

```yaml
name: zyg verify
on: pull_request
permissions:
  contents: read
  pull-requests: write
jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4        # whatever runtime your agent needs
      - uses: mindfire-test/zygote@<ref>
        with:
          corpus: recordings
          agent: node agent.js
          passphrase: ${{ secrets.ZYG_PASSPHRASE }}   # only for encrypted bundles
```

A complete example is in [examples/github-action/zyg-verify.yml](examples/github-action/zyg-verify.yml).

| Input | Default | |
|---|---|---|
| `corpus` | required | Directory of committed `*.zip` recordings |
| `agent` | required | Command that starts your agent |
| `zyg-version` | `source` | `source` builds zyg from the action's checkout; or a release tag such as `v0.3.0` (downloaded and checked against `checksums.txt`) |
| `passphrase`, `identity` | | Secrets for encrypted bundles; masked, written to a mode 600 file and passed as `ZYG_PASSPHRASE_FILE` / `ZYG_IDENTITY_FILE` |
| `approvals` | `<corpus>/.zygote/approvals.yaml` | Approvals file |
| `timeout` | `5m` | Per-recording time limit |
| `comment` | `true` | Post or update the pull request comment |
| `github-token` | `${{ github.token }}` | Token used for the comment |

Outputs: `exit-code` (the `zyg verify` exit code), `report` (path to the JSON report), `markdown` (path to the Markdown report).

The report is always added to the job summary. On `pull_request` events it is also posted as one comment per corpus,
which later pushes update instead of duplicating. Posting needs `pull-requests: write`. Pull requests from forks get a
read-only token, so no comment is posted there (and a 403 never fails the job); the result is in the job summary.
The job fails with `zyg verify`'s exit code; approved divergences do not fail it. Linux and macOS runners only.

**Warning:** do not trigger this action with `pull_request_target` on untrusted PR code: it runs the PR's agent with your repository secrets. Use `pull_request` only.

## How it works

### Worlds are content-addressed

Every write produces a new immutable Merkle tree that structurally shares every
unchanged subtree with its parent — the trick git uses to make branching free,
pointed at agent runtime state.

- A snapshot is **one 32-byte hash**, so comparing two steps is a hash comparison.
- A fork is a **struct copy**. No bytes move, ever.
- A diff **skips any subtree whose hash matches**, so its cost tracks what
  changed rather than the size of the repo.
- **Identical content always hashes identically**, regardless of write order.
  That last property is what lets a replay *prove* it matched rather than merely
  claim it.

### Determinism lives at the boundary

An agent is not nondeterministic. An agent is a deterministic program that
*calls* nondeterministic things — model, clock, RNG, network, tools. The code in
between reproduces perfectly. Record every crossing in order, and the run
replays by substitution.

A recording therefore carries two distinct things, and conflating them is the
classic record/replay bug:

| | **Effect** | **Input** |
|---|---|---|
| What | An answer the agent received | State it consumed but did not compute |
| Examples | Model completion, clock, HTTP response, tool result | The starting repository, a fixture |
| Replayed by | Substituting the recorded value | Restoring the recorded world |
| If you get it wrong | Replay calls a live API | Replay reads a repo the replaying machine does not have |

We found this the hard way: the first end-to-end run failed because replay tried
to re-read the original repository. `Run.Input` exists because of it.

## It runs nothing

zygote does **not** execute your agent, sandbox it, or replace its runtime.

Your agent keeps whatever it already has — a working directory, a container, a
hosted sandbox, a V8 isolate — and an adapter moves state in and out. That is a
deliberate boundary, not a gap: it is exactly why a run recorded against one
backend replays against another.

```
        your agent, unmodified
                  │
   ┌──────────────▼───────────────┐
   │           zygote             │  record · replay · fork · diff
   ├──────────────────────────────┤
   │           adapters           │
   └──┬────────┬────────┬─────────┘
      │        │        │
  local dir  agentOS  E2B / Daytona / container
   (runtimes zygote does not replace)
```

`source/localdir` ships today and covers any agent that edits a working
directory. An adapter is ~150 lines — contributions very welcome.

## What it does not do

Every competitor overclaims here, so these limits are published rather than
buried.

- **It cannot make a nondeterministic program deterministic.** Nondeterminism
  that never crosses the boundary — map iteration order, goroutine scheduling,
  a clock read through an uninstrumented path — is not captured. zygote
  *detects* it as divergence with no corresponding code change, which is itself
  a useful bug report.
- **Concurrency is not solved.** The effect log is one ordered sequence, so a
  run whose effects are issued concurrently replays faithfully only if that
  ordering is itself deterministic. Multi-agent runs need per-actor journals and
  a recorded interleaving. Design target, not a shipped feature.
- **Adapters have honest fidelity tiers.** Tier A captures every file at any
  point (`localdir`). Tier B captures state at step boundaries only. Tier C
  captures only what the runtime chooses to expose.
- **Bundles contain your source and your prompts.** Redaction and secret
  scanning are specified for M1 and **not yet implemented**. Until then, treat a
  bundle as sensitive and do not attach one to a public issue.
- **No semantic diffing.** zygote reports file-level change. Symbol-level
  analysis belongs to a layer above.

## Measured

On a 4 vCPU reference node, `go test -race ./...` green.

| | |
|---|---|
| Fork a 10,000-file world | **8 ns** |
| Two 200-file forks, each rewriting one file | **6 new objects**, not 400 |
| A four-step recorded run, bundled | **~7 KB**, self-contained |
| `fstest.TestFS` conformance | passes |
| Direct dependencies | **0** |
| Source | 3,079 lines of Go |

## Project status

| Package | What | Status |
|---|---|---|
| `vfs` | Content-addressed CoW worlds; snapshot, fork, diff, reachability, integrity | ✅ implemented |
| `journal` | Ordered effect log; record, replay, divergence detection | ✅ implemented |
| `trace` | Runs, steps, inputs, effects, per-step verification, portable bundles | ✅ implemented |
| `source/localdir` | Adapter for agents that edit a working directory | ✅ implemented |
| `harness` | Stdio JSON-RPC so agents in **any language** can be recorded | M1 |
| `redact` | Secret scanning, path exclusion, digest-only retention | M1 |
| `cmd/zyg` | CLI: `record`, `replay`, `steps`, `diff`, `fork`, `verify` | M1 |
| `source/*` | Adapters for agentOS, E2B, Daytona, OCI | M2 |
| `mcp` | MCP server | M2 |
| `ci` | `zyg verify` corpus runner + GitHub Action | M3 |
| `spec/v1` | Format specification and conformance suite | M4 |

**39 of 78 functional requirements implemented.** Full breakdown in [SRS.md](SRS.md).

### Coming in M1

```bash
zyg record --agent ./my-agent --dir ./repo -o run.zip
zyg replay run.zip --agent ./my-agent
zyg steps  run.zip
zyg fork   run.zip 2 --into ./branch
zyg verify --corpus ./recordings --agent ./my-agent
```

Plus a stdio protocol and thin Python/TypeScript clients, so the agent being
recorded does not have to be written in Go. That is the highest-priority work in
the project — see [DESIGN.md](DESIGN.md) for why.

## Documentation

| | |
|---|---|
| [SRS.md](SRS.md) | Full requirements specification: object model, interfaces, 78 FRs and 38 NFRs, verification plan, risks |
| [DESIGN.md](DESIGN.md) | Why this project exists, how it is positioned, and what would kill it |
| [pkg.go.dev](https://pkg.go.dev/github.com/mindfire/zygote) | API reference |

## Contributing

Contributions are welcome, especially **adapters** — including for runtimes that
compete with each other. A recorder that only works with one vendor is a
feature; one that replays across all of them is a category.

- Sign off commits with [DCO](https://developercertificate.org/)
  (`git commit -s`). There is no CLA.
- `go test -race ./...` must stay the whole story. A test that needs a bespoke
  harness is a design smell.
- New dependencies in the core module need written justification in the PR. The
  budget is 15 direct modules; we are at 0.

## License

[Apache-2.0](LICENSE) for code. The format specification, once published, is
CC-BY-4.0.
