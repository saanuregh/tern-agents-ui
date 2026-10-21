# Development

## Build and test

```sh
./build.sh                            # bin/tclaude and bin/tcodex
go test -race ./...
bin/tclaude -demo chat                # sample screens without an agent; -help lists them
bin/tclaude -log /tmp/tclaude.log     # diagnostics; add -debug for the agent CLI's own output
```

`-debug` logs can contain conversation text.

## How it fits together

- `tclaude` and `tcodex` speak the
  [Tern Surface Protocol](https://docs.stencil.so/tern/protocol/index.html)
  through the [Tern Go SDK](https://github.com/stencil-hq/tern-sdk/tree/main/go/tern).
  Each draws an inline surface: the transcript above, with the composer and
  status bar docked below.
- [agent-cli-connector](https://github.com/saanuregh/agent-cli-connector)
  drives the `claude` or `codex` CLI from the same process.
- Both binaries share one chat in `internal/chat`. What differs between
  the agents lives in `internal/chat/claude.go` and `internal/chat/codex.go`
  (plus `claude_extras.go` and `codex_extras.go` for agent-only commands).
- `cmd/tclaude` and `cmd/tcodex` are one-line entry points.
- `plugin/` is the Tern plugin that adds the palette commands. It runs the
  binaries from `../bin`.

## Styling

The look comes from Tern's own chat skin. The transcript and composer use
the roles Tern styles for its built-in agent (`omp.user`, `omp.assistant`,
`omp.editor`, `omp.composer.*`, `omp.working`, `omp.welcome`). These roles
are not a documented API, so a Tern update can change how they look. The
stylesheet in `internal/chat/main.go` adds the rest.

Tern confines a program's stylesheet to its own regions and matches only
descendants of a region. Style elements by role (for example
`.sf-row[data-role='agent.task']`) rather than by position in the dock.

## Releases

Pushing a `v*` tag runs `.github/workflows/release.yml`. It runs the tests,
then [GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) builds both
binaries for Linux and macOS and publishes a GitHub release. Each archive
holds `bin/`, `plugin/`, the docs and the license in one `tern-agents-ui`
folder.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

To build the archives locally without publishing:

```sh
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish
```
