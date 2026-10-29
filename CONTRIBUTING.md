# Contributing

Questions and ideas go to [Discussions](https://github.com/xlsft/ymcp/discussions), vulnerabilities to
[SECURITY.md](SECURITY.md). Everyone here follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Flow

- `main` is always releasable, and CI must be green on it.
- Make **small, atomic commits**. Each one does one thing and builds and passes tests on its own.
  Push after every commit.
- Use [Conventional Commits](https://www.conventionalcommits.org): `feat`, `fix`, `docs`, `test`, `refactor`,
  `build`, `ci`, `chore`, with an optional scope (`feat(tools): …`). Add `!` for breaking changes.
- A `feat`, `fix` or `perf` commit ends with a `Changelog:` trailer: one sentence on what it changes for someone
  using ymcp, not how. The release notes and [the changelog](https://ymcp.su/changelog) are made of these
  sentences, and a pull request that releases fails its `conventional` check without one.

  ```
  fix(pipeline): carry out a reply command once

  A command was marked consumed without checking that this call consumed it, …

  Changelog: A /compact sent twice while the first is still running is carried out once.
  ```

  It goes under Added (`feat`), Fixed (`fix`), Changed (`perf`) or Breaking changes (`!`); start the sentence
  with `Changed: `, `Removed: `, `Deprecated: ` or `Security: ` to put it there instead.
- Outside contributions go through a pull request from a branch. CI runs on every PR.

## Checks

Run what CI runs before you push:

```sh
gofmt -l .                     # must print nothing
go mod tidy -diff
go vet ./...
go tool staticcheck ./...
go tool govulncheck ./...
go test -race ./...
```

[`docs/reference.md`](docs/reference.md), the reference of the command line, the tools, the prompts and the pipeline
file, is generated from the code, and a test fails when it is out of date. After changing any of those, rewrite it:

```sh
YMCP_REFERENCE=write go test ./cmd/ymcp -run TestReference
```

## E2E tests

`e2e/` runs the real binary as an MCP server over stdio against a real YouTrack and calls every tool. By default it
starts a YouTrack of its own in Docker: the image pinned in `e2e/youtrack/Dockerfile` (Dependabot keeps it current),
with the configuration wizard skipped, signed in as the `admin` that a clean install creates. It comes up in about a
minute and is removed at the end, so a run shares nothing with any other. That is what pull requests, pushes to `main`
and releases run:

```sh
go test -tags e2e -count=1 -v ./e2e
```

To test an instance of your own instead, give its address and an admin account; the suite creates a project and an
agile board of its own there:

```sh
YOUTRACK_E2E_ENDPOINT=youtrack.example.com YOUTRACK_E2E_LOGIN=admin YOUTRACK_E2E_PASSWORD=… \
  go test -tags e2e -count=1 -v ./e2e
```

The nightly run and manual ones (`gh workflow run e2e.yml`) also test the shared instance from the `YOUTRACK_E2E_ENDPOINT`,
`YOUTRACK_E2E_LOGIN` and `YOUTRACK_E2E_PASSWORD` repository secrets, a hosted YouTrack that may get API changes before
the image does. An instance that is down or read-only isn't a failure of ymcp: the run waits up to
`YOUTRACK_E2E_WAIT` (10m) for it to take a write, and if it stays unavailable, or goes away during the run, the test
binary exits with 3, which the workflow reports as a warning that nothing was tested.

Without an endpoint and without Docker the suite skips. Everything a test creates is named with the `ymcp-e2e` prefix
and the run's ID (`runID`, six digits) and cleaned up:

- `t.Cleanup` deletes each object, even when the test fails;
- a sweep removes this run's leftovers at the end, and those of runs over two hours old at both ends;
- the run fails if any of its own marked objects is still there at the end.

Runs may overlap, a pull request's with a release's say, since each keeps to its own objects. Keep it that way
when adding scenarios: create through the fixture project or with the marker and `runID` in the name, keep queries
to the fixture project, and delete what you create. Tests share the run's project but nothing in it: each makes its
own issues (`newIssue`) and other objects, so any one runs on its own (`-run TestComments`) and a failure in one
doesn't spill into the next.

Besides the stdio server, the suite runs `ymcp serve --transport http` and a container built from the Dockerfile,
each serving the admin and the member behind a token; the Docker test skips where Docker isn't running, and against a
YouTrack of the run's own outside Linux, where a container can't reach the host's loopback.

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
5. Add it to the tool tables in the README and rewrite `docs/reference.md` (see [Checks](#checks)).

### Trying changes on a live instance

```sh
cp .env.example .env    # fill in URL, login and password; .env is gitignored
go build -o bin/ymcp ./cmd/ymcp && ./bin/ymcp --env .env call <tool> '<json>'
```

Keep test data recognisable (e.g. a `[ymcp test]` prefix) and delete it afterwards.

## Pipeline packages

`ymcp pipeline` lives in `cmd/ymcp/pipeline*.go` and these packages:

| Package | |
|---|---|
| `internal/pipeline/spec` | The pipeline JSON: fields, defaults, validation, prompt presets |
| `internal/pipeline/engine` | The runner: polling, stages, turns, feedback, needs input |
| `internal/pipeline/agent` | Claude Code (CLI) and ACP agents |
| `internal/pipeline/vcs` | Strategies: branch, GitHub PR and issue, GitLab MR; tokens |
| `internal/pipeline/ytwatch` | YouTrack searches, comments and commands for the runner |
| `internal/pipeline/mcpserver` | The per-run MCP server the agent talks to |
| `internal/pipeline/workspace` | Git worktrees and branches |
| `internal/pipeline/store` | Runs, messages and events in `state.db` (SQLite) |
| `internal/pipeline/text` | Templates, the agent marker and header, feedback lists |
| `internal/pipeline/tui` | The TUI and the setup wizard |
| `internal/notify` | Notifications; `internal/notify/toast` is the system notification provider |

Unit tests use fakes and never touch the network: `internal/pipeline/agent/agenttest` plays a Claude Code or ACP
agent from a script (and recorded Claude transcripts), the engine runs against the fake YouTrack of
`internal/youtrack/youtracktest`, a fake agent and an in-memory code host, and `vcs` tests answer from an `httptest` GitHub and
GitLab.

Live tests are opt-in and never run in CI. `YMCP_PIPELINE_LIVE=1` sends a tiny prompt to each agent installed, in
a throwaway repository (it bills your accounts), and reads YouTrack and GitHub without writing anything: the
YouTrack test talks only to the host in `YMCP_PIPELINE_LIVE_HOST`, through the profile in `YMCP_PIPELINE_PROFILE`, and
reads the project in `YMCP_PIPELINE_PROJECT`; the GitHub one reads github.com/xlsft/ymcp with your `gh` login. `YMCP_NOTIFY_LIVE=1` shows one real notification. Try
whole pipelines on your own instance and a sandbox repository, never on a shared one.

## Releasing

Releases are automatic; nobody tags by hand. Each also updates the Homebrew cask in
[xlsft/homebrew-tap](https://github.com/xlsft/homebrew-tap) and the Scoop manifest in
[xlsft/scoop-bucket](https://github.com/xlsft/scoop-bucket). That needs the `YMCP_PACKAGES_TOKEN` secret, a token that
can push to those repositories; without it the release skips them. Every push to `main` runs the Release workflow: CI and the E2E suite
first, then [`.github/scripts/next-version.sh`](.github/scripts/next-version.sh) picks the next
[SemVer](https://semver.org) version from the commit messages since the last tag:

| Commits | Release |
|---|---|
| `feat` | minor |
| `fix`, `perf` | patch |
| `type!:` or `BREAKING CHANGE` | major (minor while on 0.x) |
| only `docs`, `test`, `ci`, `build`, `chore`, `refactor` | none |

A change that touches only documentation (Markdown, `docs/`, `site/`, `LICENSE`, the issue and pull request templates)
runs neither CI nor E2E, on a pull request or on `main`, and releases nothing: their jobs skip, which the required
checks count as passing. [`.github/scripts/code-changed.sh`](.github/scripts/code-changed.sh) decides.

The workflow then tags the version and uses GoReleaser to build macOS, Linux and Windows binaries for amd64 and
arm64. It publishes them with checksums and a changelog. `install.sh`, `install.ps1` and `ymcp update`
pick up the new release automatically. The release notes start with What's new, the `Changelog:` trailers since the last
release (see [`.github/scripts/changelog.py`](.github/scripts/changelog.py)), above the list of its commits;
[`CHANGELOG.md`](CHANGELOG.md) points to every release's.
