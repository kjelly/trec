# trec

Record, drive, play, and inspect terminal sessions.

## Development

Install [mise](https://mise.jdx.dev/getting-started.html), then install the
project's pinned Go toolchain:

```sh
mise install
mise run check
```

`mise.toml` pins Go 1.27.4. CI uses the same configuration and `check` task.
Dependencies are managed with Go modules.

```sh
mise run build       # build ./trec
mise run test        # run all tests
mise run test:race   # run tests with the race detector
mise run vet         # run static analysis
mise exec -- go run . --help
```

You can also [activate mise in your shell](https://mise.jdx.dev/getting-started.html#activate-mise)
to run `go` commands directly. When upgrading Go, update `mise.toml`, `go.mod`,
`.golangci.yml`, and the version documented here and in `AGENTS.md` together.
