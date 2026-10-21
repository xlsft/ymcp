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

## E2E tests

`e2e/` runs the real binary as an MCP server over stdio against a real YouTrack and calls every tool. The test
account must be an admin there, because the suite creates its own project and agile board. The E2E workflow runs on
pushes to `main`, on pull requests from this repository, nightly and on demand. It uses the
`YOUTRACK_E2E_ENDPOINT`, `YOUTRACK_E2E_LOGIN` and `YOUTRACK_E2E_PASSWORD` repository secrets. To run it locally
against an instance of your own:

```sh
YOUTRACK_E2E_ENDPOINT=youtrack.example.com YOUTRACK_E2E_LOGIN=admin YOUTRACK_E2E_PASSWORD=… \
  go test -tags e2e -count=1 -v ./e2e
```

Without those variables the suite skips. Everything a test creates is named with the `ymcp-e2e` prefix and
cleaned up:

- `t.Cleanup` deletes each object, even when the test fails;
- a sweep removes marked leftovers before and after the run;
- the run fails if anything marked is still there at the end.

Keep it that way when adding scenarios: create through the fixture project or with the marker in the name, and
delete what you create.

One account is the exception: `ymcp-e2e-regular`, the member without admin rights that runs the non-admin scenarios.
The license limits users and YouTrack keeps deleted ones, so it is created on the first run and kept. Its password
derives from the admin's and must never reach a log, since CI logs of this repository are public; Hub can't reset it.
Never create a user per run.

## Adding a tool

1. Put the handler in `internal/tools`, next to related tools, and register it in `Register` (`tools.go`).
   Input structs use `json`/`jsonschema` tags; fields without `omitempty` are required.
2. Mark read-only tools with `readOnly(...)` and writes with `mutating(..., destructive)`.
3. Test it in `tools_test.go`. `connect` runs the tool through an in-memory MCP client against the fake
   YouTrack in `internal/youtrack/youtracktest`; assert on the REST requests it sends.
4. Add a scenario to `e2e/scenarios_test.go` that calls it on the live instance.
5. Add it to the tool tables in the README.

### Trying changes on a live instance

```sh
cp .env.example .env    # fill in URL, login and password; .env is gitignored
go build -o bin/ymcp ./cmd/ymcp && ./bin/ymcp --env .env call <tool> '<json>'
```

Keep test data recognisable (e.g. a `[ymcp test]` prefix) and delete it afterwards.

## Releasing

Releases are automatic; nobody tags by hand. Every push to `main` runs the Release workflow: CI and the E2E suite
first, then [`.github/scripts/next-version.sh`](.github/scripts/next-version.sh) picks the next
[SemVer](https://semver.org) version from the commit messages since the last tag:

| Commits | Release |
|---|---|
| `feat` | minor |
| `fix`, `perf` | patch |
| `type!:` or `BREAKING CHANGE` | major (minor while on 0.x) |
| only `docs`, `test`, `ci`, `build`, `chore`, `refactor` | none |

The workflow then tags the version and uses GoReleaser to build macOS, Linux and Windows binaries for amd64 and
arm64. It publishes them with checksums and a changelog. `install.sh`, `install.ps1` and `ymcp update`
pick up the new release automatically.
