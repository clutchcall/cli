# `clutch` — ClutchCall SDK Developer CLI

A single static Go binary that scaffolds, migrates, and (eventually) documents
ClutchCall projects. Modeled on LiveKit's `lk` CLI: the goal is to give both
humans and coding agents (Claude Code, Cursor, Codex, …) a stable entry point
for spinning up a working ClutchCall app and keeping it on a current schema.

> Named `clutch` to avoid a collision with the system C compiler (`cc` is
> typically a symlink to `gcc` or `clang`).

## Build

```bash
cd cli
go build -o bin/clutch ./cmd/clutch
```

The binary embeds all starter templates via `embed.FS` — it has no runtime
dependency on this repo and can be shipped as a single artifact.

## Commands

| Command                                    | Status   | Notes |
|--------------------------------------------|----------|-------|
| `clutch init <lang> <name>`                | working  | `lang` ∈ `go`, `typescript`, `python` |
| `clutch migrate`                           | partial  | reads `.clutchcall.json`, flags schema drift; full diff against `apirpc_compiler` is TODO |
| `clutch docs <overview\|search\|get-page>` | stub     | wiring waits on the `clutchcall-docs` corpus shoring-up work |
| `clutch version`                           | working  | |

## Adding a Language

1. Drop a directory under `internal/scaffold/templates/<lang>/`.
2. Files ending in `.tmpl` are rendered with `text/template` against `{Name, Endpoint}`; everything else is copied verbatim.
3. Register the alias in `supported` (in `internal/scaffold/scaffold.go`).

## Roadmap

- [ ] Wire `clutch docs` to `docs.clutchcall.io` and a local `clutchcall-docs` fallback.
- [ ] Wire `clutch migrate` to the generated schema in `clutchcall/tools/apirpc_compiler` so it can show added/removed/renamed RPC fields per version.
- [ ] Add `clutch deploy` once the agent-runtime Cloud target is stable.
- [ ] Publish a hosted MCP server exposing `get_pages` / `code_search` / `changelog` (mirrors LiveKit's `https://docs.livekit.io/mcp`).
