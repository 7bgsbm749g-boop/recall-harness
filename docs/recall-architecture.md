# Recall architecture

Recall adds an ACP executable and a memory adapter to Unreal Agent. T3 remains the owner
of its conversations, workspace bindings, approvals, checkpoints, and orchestration tools.
Recall does not edit T3's database. Its installer adds one named local ACP provider through
the normal settings API.

## Evidence and views

Each project identity selects a separate archive. The origin URL identifies Git projects,
including their worktrees. An explicit `RECALL_PROJECT_KEY` can bind non-Git fleet directories.
URLs are stripped of embedded credentials before they become project identities.

An original event contains its thread, ordinal, timestamp, and complete normalized model
item. Its SHA-256 reference is immutable, for example `recall://PROJECT/event/HASH`. Summaries
have their own content references, `recall://PROJECT/memory/HASH`. A revised summary is a
new object. Its source list links to original events or earlier summary layers.

Each document contains a brief, cited facts, and retrieval hints. Fact records distinguish
instructions, decisions, observations, proposals, and unfinished tasks. Scope and state are
explicit. An instruction must quote a cited user message. A higher summary cannot silently
drop an inherited standing instruction; a replacement must cite the old instruction and
have the same scope. Project curation excludes turn and thread instruction records.

These checks validate provenance and structure. Models still decide what a statement means,
whether it is durable, and whether a correction applies. A recorded claim can be wrong.
Retrieval hints and original sources let the foreground agent check those decisions.

## Background publication

The memory adapter archives full foreground input and output. A separate model call prepares
thread views while foreground work continues. A completed turn also schedules publication,
so short conversations can contribute to a new conversation's project seed. A persistent
worker recovers pending updates when the ACP process exits.

Each thread has a short queue that coalesces newer snapshots. A thread lock prevents two
workers from summarizing that thread concurrently. Project locks cover file publication,
not model calls. Project curation uses an optimistic check against the previous logical
roots; a conflicting publisher retries against the new roots.

Four leaves normally roll into one parent. The same process applies recursively to thread
and project layers. A bounded overview gives the next model call its project seed. Old
evidence remains searchable through an on-disk term index, even when no current summary
mentions it. Search uses term matching; this release has no embedding service.

## Context changes

A prepared checkpoint stores an exact source-prefix digest and event count. Recall uses
that checkpoint only when it still matches the live transcript, ends outside an open tool
exchange, and leaves the latest user request verbatim. A worker snapshot taken during a
tool call remains available for retrieval but cannot split that call from its result.

Prepared memory normally replaces the old prefix at the configured compaction threshold.
Recent history and the current system instructions remain in the model request. If no
usable checkpoint is ready at the context limit, Recall waits and prepares a safe prefix.
A failed summary, invalid citation, oversized retained tail, or repeated publication conflict
returns an explicit error with original evidence retained. Invalid memory JSON can receive
up to three bounded repair attempts. Nothing silently discards unprocessed history.

Token budgets include tool definitions. `o200k_base` estimates and framing reserves are
conservative planning inputs, not a provider-independent tokenizer guarantee.

## Recovery and fleet transport

Canonical session logs use Unreal Agent's durable operation protocol. Mutating tool calls
write a start receipt before execution. A saved result can be replayed without repeating
the mutation. An interrupted operation with an unknown outcome requires inspection before
retrying. Plan mode refuses mutating tools; Ask mode uses ACP permission requests.

The background worker uses transcript coverage as its durable queue. It records errors and
retry times in the manifest. A crash cannot lose an update between a separate queue and
archive write. Idle workers make no model requests.

Fleet sync streams compressed, integrity-checked objects over the existing private SSH
aliases. Inventories avoid resending objects. Heads change only after a complete transfer
passes validation. Divergent histories are retained and reported. Stable fingerprint caching
skips completed exchanges; a concurrent append forces another pass. Project views are
rebuilt locally. Login files, tool receipts, and approvals are outside the transfer format.

Sync is eventual, and offline hosts keep their local data. Legacy T3 history is accessible
through T3 tools but is not automatically backfilled into Recall's archive. Audio
prompts remain unsupported. Image prompts are supported from version 0.1.1: native
user image parts are saved in the durable inbox and immutable event archive. Session replay
re-emits those image parts. Fleet sync includes the original bytes. Search and summary
projections show MIME type, dimensions, and retrieval instructions instead of base64.
`memory_read` verifies an archived event, then materializes private, hash-checked local files
for `ViewImage`; these files are disposable caches. Context estimates count pixel allowances
for user and tool images rather than treating encoded bytes as prose.
