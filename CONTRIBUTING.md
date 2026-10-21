# Contributing

## Flow

- `main` is always releasable, and CI must be green on it.
- Make **small, atomic commits**. Each one does one thing and builds and passes tests on its own.
  Push after every commit.
- Use [Conventional Commits](https://www.conventionalcommits.org): `feat`, `fix`, `docs`, `test`, `refactor`,
  `build`, `ci`, `chore`, with an optional scope (`feat(tools): …`). Add `!` for breaking changes.
  Release notes are grouped from these prefixes.
- Outside contributions go through a pull request from a branch. CI runs on every PR.

## Checks

Run what CI runs before you push:

```sh
gofmt -l .                     # must print nothing
go mod tidy -diff
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go test -race ./...
```

## Adding a tool

1. Put the handler in `internal/tools`, next to related tools, and register it in `Register` (`tools.go`).
   Input structs use `json`/`jsonschema` tags; fields without `omitempty` are required.
2. Mark read-only tools with `readOnly(...)` and writes with `mutating(..., destructive)`.
3. Test it end to end in `tools_test.go`. `connect` runs the tool through an in-memory MCP client against the fake
   YouTrack in `internal/youtrack/youtracktest`; assert on the REST requests it sends.
4. Add it to the tool tables in the README.

### Trying changes on a live instance

```sh
cp .env.example .env    # fill in URL, login and password; .env is gitignored
go build -o bin/ymcp ./cmd/ymcp && ./bin/ymcp --env .env call <tool> '<json>'
```

Keep test data recognisable (e.g. a `[ymcp test]` prefix) and delete it afterwards.

## Releasing

Versions follow [SemVer](https://semver.org). To release, tag `main` and push the tag:

```sh
git tag -a v0.2.0 -m v0.2.0
git push origin v0.2.0
```

The Release workflow runs the tests and builds macOS, Linux and Windows binaries for amd64 and arm64 with
GoReleaser. It then publishes them with checksums and a changelog. `install.sh`, `install.ps1` and `ymcp update`
pick up the new release automatically.
