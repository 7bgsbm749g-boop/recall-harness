# Recall harness

Recall is a fork of [Unreal Agent](https://github.com/unreallabsai/unreal-agent) with
background compaction, source-linked memory layers, and a local ACP provider for T3 Code.
The upstream MIT license and module path are retained.

## Use Recall in T3 Code

Build with Go 1.27 or later:

```sh
go build -trimpath -o dist/recall ./cmd/recall
./dist/recall doctor
python3 scripts/install.py --binary dist/recall --register --worker
```

The installer registers **Recall** as a local ACP provider on the personal fleet.
For another T3 installation, add a **Local ACP command** under **Settings → Providers**:
use the installed Recall executable with one argument, `acp`.

Recall uses the existing Codex subscription file login by default. It reads credentials
as needed and does not change the Codex auth file. Renew credentials with Codex when
needed. Other upstream model providers work through `RECALL_PROVIDER`, `RECALL_MODEL`,
and their normal credential environment variables.

The default model is `gpt-6.1-sol`. Model and reasoning choices appear in T3's composer.
Ask mode requests T3 approval for commands and tools that can change state. Full access
runs those tools with the current user's permissions. Plan mode permits read-only tools.
T3 keeps control of its projects, threads, checkpoints, and MCP tools.

On Windows, build or download `recall.exe`, then run:

```powershell
python scripts/install.py --binary dist/recall.exe --register --worker --sync-peer fleet-vps
```

The installer uses a per-user logon task for the memory worker and optional fleet sync.
T3 launches the versioned `.exe` directly. Windows binaries cross-compile on Linux;
native verification still requires an online Windows host.

## Memory behavior

Each Recall project has its own archive and memory layers. Git worktrees and matching
fleet checkouts use the same origin identity. Non-Git directories use their absolute path;
set `RECALL_PROJECT_KEY` when those directories should share an identity across hosts.

- Original model-visible events are stored before compaction. Immutable references contain
  content hashes. Changed summaries create new versions and retain links to their sources.
- A dedicated model worker prepares thread summaries while the foreground agent continues.
  Completed short turns also enter project memory. The persistent worker resumes unfinished
  updates after an ACP process exits.
- Thread and project summaries form bounded layers. A full layer produces a more concise
  parent. Each parent keeps references to its children and the original evidence remains readable.
- New threads receive the current project brief. `memory_search` searches both summaries and
  original transcripts. `memory_read` follows exact references and supports bounded reads.
- Standing instruction records must quote a cited user message. Higher summaries retain them
  when a model omits them. A replacement must cite the instruction it supersedes in the same
  scope. Turn and thread instructions do not become standing project instruction records.
- A prepared summary identifies the exact prefix it covers. A context switch keeps every later
  event and preserves complete tool exchanges. A late worker causes a compaction wait;
  a failed compaction preserves the original history and returns an explicit error.

The default scan interval is about 50,000 estimated tokens. Compaction starts at 160,000,
with a 200,000-token context budget, a 24,000-token reserve, and 12,000 recent tokens retained.
Tool definitions count toward the budget. Estimates use `o200k_base` with framing overhead;
they are not exact token counts for every provider. Actual provider usage is checked too.

Memory lives outside the workspace: `~/.local/state/recall` on Linux and
`%LOCALAPPDATA%/Recall` on Windows. `RECALL_DATA_DIR` selects another location.
The memory service makes no model calls when no thread update is pending.
See [the architecture](docs/recall-architecture.md) for publication and recovery behavior.

```sh
recall memory status --workspace /path/to/project
recall memory search --workspace /path/to/project --query 'release failure'
recall memory read --workspace /path/to/project --reference 'recall://PROJECT/memory/HASH'
recall memory verify --workspace /path/to/project
recall memory worker --once
```

Runtime limits can be set with `RECALL_SCAN_TOKENS`, `RECALL_COMPACT_TOKENS`,
`RECALL_CONTEXT_TOKENS`, `RECALL_RESERVE_TOKENS`, `RECALL_RETAIN_TOKENS`,
`RECALL_SUMMARY_BYTES`, and `RECALL_LAYER_FANOUT`. Invalid combinations fail at startup.

Recall reads existing fleet and workspace `AGENTS.md` instructions and discovers global
and workspace skills. Memory does not remove those instructions. Project memory starts
with conversations run through Recall; existing T3 conversations remain available through
T3's history tools. Prompt text and embedded text resources are supported. Image and audio
prompt capabilities are not advertised in this release; unsupported input is rejected.

Summaries and retrieved facts still need interpretation. Source integrity establishes what
was recorded. It does not establish that a recorded claim was correct or remains current.

## Fleet memory sync

The optional sync component uses the existing private SSH aliases. It transfers missing
immutable objects and transcript heads, then rebuilds local project views. It skips unchanged
projects. Login credentials, approvals, and command execution state are outside the transfer
format. Transcript content can contain private data and stays on the selected fleet hosts.

```sh
python ~/.local/share/recall/sync-memory.py --peer fleet-vps
```

An interrupted transfer cannot replace transcript heads. Identical transfers are idempotent.
Divergent versions of the same thread produce an error and preserve both sets of evidence.
Sync is eventual: an offline host keeps its local memory until it reconnects.

## Verify or remove Recall

```sh
go test ./...
go test -race ./recall/...
python3 -m unittest discover -s scripts -p 'test_*.py'
python3 scripts/smoke-acp.py --binary dist/recall --evidence /tmp/recall-evidence
```

The live smoke uses a separate synthetic workspace. It checks prepared compaction, a user
correction, project memory in a new session, and source integrity. It makes real model calls.
Keep evidence and test fixtures outside synced user vaults.

On Linux, stop background work with `systemctl --user disable --now recall-memory.service`
and, when installed, `recall-memory-sync.service`. On Windows, stop and unregister the
`RecallMemory` and `RecallMemorySync` scheduled tasks. Remove the provider in T3 settings.
Archives remain on disk. Reinstall an earlier verified binary to roll back the executable.

## Upstream foundation

The original async-first harness remains available as a library. Recall's memory and ACP
integration run through `cmd/recall`; the upstream runner and TUI retain their own behavior.

- [harness/](harness/) — the library.
- [cmd/](cmd/) — executables that use the library.
  - [cmd/recall](cmd/recall) - Recall's ACP provider and memory commands.
  - [cmd/unreal-agent-runner](cmd/unreal-agent-runner) - cloud agent runner.
  - [cmd/unreal-agent-tui](cmd/unreal-agent-tui) - cozy TUI.
- [benchmarks/](benchmarks/) — benchmark runners.

## Glossary

- **Input**: an event with a caller-supplied globally unique ID that remains
  stable across redeliveries.
- **Inbox**: session-scoped, in-memory deduplication of external, control, and
  crash inputs.
- **Session**: append-only persisted history that can be forked.
- **LLM turn**: the coordinator-managed sequence around one logical LLM request.
- **Tool**: a capability described by a schema and bound to a translator.
- **Tool call**: a model-produced request to use a tool.
- **Tool translator**: validates a tool call and translates it into one or more
  operations. It runs synchronously on the coordinator's event loop and must not
  perform I/O or suspend the loop.
- **Tool call status**: the translation outcome: a validation error or references
  to submitted operations. Operation execution state is tracked separately;
  the translator formats these into a model-facing result.
- **Operation**: a serializable description of work produced by a tool translator
  for asynchronous execution. Implementations are encouraged to use the available
  [primitives](harness/primitives/).

## Components

| Component | Responsibility |
| --- | --- |
| Session inbox | Volatile, session-scoped input idempotency. |
| Coordinator | Persist accepted inputs, run LLM turns, resolve tool translators through the registry, and dispatch committed operations. |
| Session store | Persist canonical session history and operation state; support recovery and forks; atomically record tool-call status with operations. |
| Context builder | Statefully assemble model input in memory. Return the model input together with a record of anything omitted, truncated, or compacted. Perform no I/O and accept no persistence dependencies. |
| LLM Adapter | Send prepared model input to a provider and return a normalized completed response. Own authentication, cancellation, and provider errors. |
| Tool registry | Own the fixed Bash, ViewImage, and skill-use definitions and their translators; expose the host-selected set. |
| Tool translator | Validate a tool call and produce its status and operations. Format a recorded call status and prepared operation output into model results. Perform no I/O. |
| Operation manager | Actor runtime for durable operations. The local implementation is swappable. |

## Extending the harness

Harness components are composable, and alternative implementations of their interfaces are encouraged.

We intend to preserve these invariants:

- Session-store items are serializable, and the storage format is versioned.
- We'll do our best to maintain backwards compatibility for sessions.
  An unsupported session version will always cause an explicit error on resume.
- Operations are versioned and always serializable.

For example, a proxy operations manager can send serialized operations to a
local operations manager running in a process inside a remote sandbox, allowing
tools to execute there.
