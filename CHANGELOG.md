# Changelog

What each release of ymcp changed, newest first. The versions follow [Semantic Versioning](https://semver.org); every release, with its binaries and the list of its commits, is on [GitHub](https://github.com/xlsft/ymcp/releases).

## [0.24.0] - 2026-10-29

### Changed

- The pipeline wizard's discard question offers s to save the draft instead.

## [0.23.0] - 2026-10-29

### Added

- `ymcp update` lists what each release between your version and the new one changed, and links the changelog at ymcp.su/changelog.

## [0.22.0] - 2026-10-29

### Changed

- An answer, a reply command or a cancel given in the TUI or with the CLI, and a comment the TUI posts on a run's issue, reach the run within a second instead of waiting for the runner's next poll of YouTrack.

### Fixed

- A second `/compact` sent while the first is still writing its summary is skipped, instead of making two summaries and hiding the comments twice.

## [0.21.0] - 2026-10-29

### Added

- `ymcp setup` in a terminal opens a TUI of your profiles, in the look of `ymcp pipeline`, showing each one's sign-in, allowed tools, confirmations and clients; enter edits a profile, n adds one and d removes one.
- A profile wizard edits a profile in three steps: its name and connection (signed in as you type), the allowed tools chosen one by one in a tree of groups, and the MCP clients it is registered in.
- Destructive tools (deletes, unlinks, `apply_command` with a removing command, `call_api` with DELETE) can ask for your confirmation: called without `confirm` they change nothing and return a one-time secret, valid for ten minutes, that the model must pass back after asking you.

### Changed

- Confirmation of destructive tools is on by default for every profile; turn it off per profile with `ymcp setup --confirm`, or per server with `ymcp serve --confirm` or `YMCP_CONFIRM`, while `ymcp call` and pipeline agents never ask.

## [0.20.3] - 2026-10-29

### Changed

- Release provenance is now made by GitHub's build attestations and can be verified with `gh attestation verify`; the release still carries it as `ymcp.intoto.jsonl`.

### Fixed

- A leading `~` in `workspace.worktrees` now means the home directory on the host, instead of git creating a directory named `~` inside the repository on ssh hosts.

## [0.20.2] - 2026-10-28

### Added

- Each release comes with SLSA build provenance, `ymcp.intoto.jsonl`, for every file listed in `checksums.txt`.

### Fixed

- The note in the sign-in form now says plainly that only your login and password are needed, with nothing to set up in YouTrack.

## [0.20.1] - 2026-10-28

### Changed

- ymcp is no longer published to winget, and no longer suggests updating through it; Homebrew, Scoop, the install scripts and Docker remain.

### Fixed

- Binaries are built with an updated `golang.org/x/text`, fixing two known vulnerabilities on crafted input.
- A stray escape byte from a CI job log with broken UTF-8 no longer reaches the pipeline agent's prompt.

## [0.20.0] - 2026-10-28

### Changed

- After a push, the pipeline agent writes the PR/MR title and description in a turn of its own and hands them over with the new `set_change` tool, instead of its whole work-turn answer becoming the description; later pushes update them unless someone edited them on the host, and `strategy.describe: false` keeps the old way.

## [0.19.2] - 2026-10-28

### Fixed

- A remote agent now starts in a `workspace.dir` written with `~` on an ssh host, which used to fail with "can't cd"; the advice to fix the PATH is given only when the shell actually reported a missing command.

## [0.19.1] - 2026-10-28

### Fixed

- Two pipeline runs captured at once no longer fail on a "cannot lock ref" error: fetches of one repository go one at a time, and a fetch that raced another process is retried up to three times.

## [0.19.0] - 2026-10-28

### Added

- `strategy.title` sets the PR/MR title template, `{{issue.key}} {{issue.summary}}` by default, and `{{commit.subject}}` gives the branch's first commit for a conventional title that a squash merge keeps.

### Changed

- ymcp's own commits in pipeline branches no longer end in a full stop.

### Fixed

- An empty `workspace.commitPrefix` now means no prefix instead of falling back to the issue ID, so repositories with commitlint accept the agent's commits; the agent is told to write commit messages the repository's way.

## [0.18.0] - 2026-10-28

### Added

- Pipeline agents may now use `gh` and `glab` to fix the PR/MR title or description and to run and read CI; ymcp still pushes and opens the change, and the agent merges or closes it only when a person asks.
- `strategy.squash` and `strategy.deleteBranch` open GitLab merge requests with squash and delete-branch ticked, for projects that require them.
- The runner watches the CI of every push on GitHub and GitLab and gives failures to the agent with the failed jobs and the end of their logs; after `ci.fixes` failed attempts in a row (3 by default) the run goes to needs input, and `ci.watch: false` turns this off.

### Changed

- Pipeline agents are told never to credit themselves or any other agent, model or tool in commits or changes.

## [0.17.0] - 2026-10-28

### Added

- t on a run card opens a live transcript of the run, turn by turn: what the agent thinks and says, each tool it runs with the file, command or pattern, and the replies it gets; the pull request thread moves to [ and ].

### Changed

- The agent's text and thoughts are recorded every second while it streams, so long answers show up on the run card as they are written.

## [0.16.0] - 2026-10-28

### Added

- d on the board or a run card, or `ymcp pipeline NAME delete RUN`, deletes a run that has ended from the local store after asking (`-y` skips the question); the issue, branch and pull request stay.

### Fixed

- An issue canceled with `/cancel` or from the CLI and then put back in the source is captured again once the runner restarts, instead of staying in the backlog for good.
- A new run no longer continues a branch left by a run that is no longer in the store; branch names that git already has, locally or on origin, are skipped.
- When an agent on an ssh host is not found, the run's error and `ymcp pipeline check` say that the command is not in the PATH of the agent's shell and that `~/.zshenv`, the top of `~/.bashrc` or `agent.command` fixes it.

## [0.15.2] - 2026-10-28

### Fixed

- The run card keeps an issue's fields beside the title when they take three rows, instead of moving them under the title.

## [0.15.1] - 2026-10-28

### Added

- Every release is published to the MCP Registry, which lists the Docker image as a stdio server configured by `YOUTRACK_*` variables.

### Fixed

- Binaries are built with Go 1.26.10, fixing known vulnerabilities in the standard library (net/http, crypto/tls and others) that earlier builds shipped.

## [0.15.0] - 2026-10-28

### Added

- In kitty and Ghostty the run card shows pictures in place: images in the description and comments, screenshots pasted into comments and the issue's picture attachments; other terminals and tmux or screen get links, and `YMCP_IMAGES=kitty` or `off` overrides the choice.

## [0.14.0] - 2026-10-28

### Added

- The run card shows every field of the issue that has a value, and e opens a menu of the project's fields to change them; a backlog issue that no longer matches the source query after a change leaves the board.
- r reloads the board and the card, enter opens the comment box, and s on the board starts or stops the runner.

## [0.13.0] - 2026-10-28

### Added

- Typing @ in the run card's comment box lists matching YouTrack users, and tab inserts their @login.

### Changed

- The pipeline agent now commits as the repository's own `user.name` and `user.email`; a separate author is the opt-in `workspace.identity`, with `identity.name` and `identity.tag` templates set in the wizard under Advanced.
- The run card's frame, help and status lines are laid out more cleanly, and the help names enter and r.

## [0.12.2] - 2026-10-28

### Fixed

- The pipeline wizard's previews of the branch template and commit prefix now fill in issue fields such as `{{issue.fields.Type}}` instead of showing none.

## [0.12.1] - 2026-10-28

### Fixed

- A stage search naming a tag that does not exist yet now matches nothing instead of making the runner skip every stage check, so issues moved to done, closed or back to the backlog are noticed; the wizard shows it as a warning.

## [0.12.0] - 2026-10-27

### Added

- `ymcp pipeline` hands YouTrack issues to a coding agent (Claude Code, Kimi, opencode or any ACP agent), opens a GitHub PR, a GitHub issue with a PR or a GitLab MR, moves the issue through its stages and answers feedback from YouTrack and the change.
- Pipelines come with a TUI, agents on SSH hosts, reply commands (`/approve`, `/reject`, `/clear`, `/compact`, `/cancel`), prompt presets and system notifications.

## [0.11.0] - 2026-10-26

### Added

- Every tool declares all four annotation hints (read-only, destructive, idempotent, open-world) as explicit values, so hosts that require them accept ymcp.

## [0.10.1] - 2026-10-24

### Fixed

- `ymcp --version` and `-h`/`--help` now print the version or usage and exit successfully without a configured profile.

## [0.10.0] - 2026-10-24

### Added

- `edit_article_content` and `edit_issue_description` apply exact or regexp find-and-replace edits, all or nothing, so a small change does not resend the whole text.

## [0.9.2] - 2026-10-21

### Fixed

- Publishing to Homebrew, Scoop and winget no longer fails once the packages token is configured.

## [0.9.1] - 2026-10-21

### Fixed

- Release archives are reproducible: every build of a commit gives the same archives and checksums.

## [0.9.0] - 2026-10-21

### Added

- ymcp is published to Homebrew (macOS and Linux), Scoop and winget with every release.

### Changed

- `ymcp update` no longer replaces a binary installed by Homebrew, Scoop or winget; it and the start-up notice name the package manager's upgrade command instead.

## [0.8.0] - 2026-10-21

### Added

- Four MCP prompts, shown by clients as slash commands, guide the model through common jobs: `standup`, `triage` (changes applied only after approval), `sprint_report` and `issue_brief`; a prompt is offered only when the tools it needs are served.
- The HTTP server answers `GET /healthz` with its version and no token, `ymcp health` checks it, and the Docker image uses it as its health check.
- YouTrack errors reach the model with a hint on what to do next, such as which tool finds a valid ID after a 404 or that a 401 needs `ymcp auth`; HTML error pages are cut down to their title.
- Requests to YouTrack endpoints that require a Hub-scoped token, such as deleting a user, are repeated once with one.

### Fixed

- Error messages in the Docker image name the `docker run ... auth` command instead of `ymcp setup`, and leave `--profile` out for the default profile.

## [0.7.1] - 2026-10-21

### Added

- Release archives and the Docker image are signed with Sigstore and come with SBOMs; the README shows how to verify them.

### Changed

- `create_issue` and `update_issue` reuse a project's field schema for five minutes instead of reading it on every call, and read it again when a cached one rejects a field or value.

## [0.7.0] - 2026-10-21

### Added

- A profile can serve only chosen tool groups (issues, comments, attachments, articles, time, agile, projects, people, tags, searches, api) or single tools, set in `ymcp setup` or with `setup --tools` and overridden by `serve --tools` or `YMCP_TOOLS`.
- Calls to a busy or unreachable YouTrack are retried up to three times with short waits or what Retry-After asks for; requests that may already have taken effect are never sent twice.

### Fixed

- `find_user_groups` for a non-admin user on YouTrack 2026 now explains that only admins can list groups there, instead of returning an empty list.

## [0.6.1] - 2026-10-21

### Fixed

- Entries added to JSONC client configs such as Zed's are indented like their neighbours instead of glued to the previous entry.

## [0.6.0] - 2026-10-21

### Added

- The server logs at start when a newer release is out, with the command to update; `YMCP_UPDATE_CHECK=off` turns this off.

## [0.5.0] - 2026-10-21

### Added

- `ymcp setup` can register ymcp in Kimi Code.
- `--transport http` serves every profile on one server at `/<key>`, `--profiles` narrows the set, and `--token` or `YMCP_HTTP_TOKEN` requires a bearer token on every request.
- A Docker image on Docker Hub runs the HTTP server for every profile on port 8090, with profiles kept in the `/config` volume.

### Fixed

- Passwords, tokens and other secrets in logged tool arguments, such as `call_api` bodies, are masked as `***`.
- The HTTP address is logged as given, so 0.0.0.0 no longer shows up as [::].

## [0.4.1] - 2026-10-21

### Fixed

- `ymcp setup` offers to quit Claude Desktop while it edits its config and reopens it afterwards, because Claude Desktop otherwise wrote the old config back and the new server disappeared.

## [0.4.0] - 2026-10-21

### Added

- Attachment tools work on knowledge base articles through `articleId`, `add_attachment` uploads several files at once with `filePaths`, and `get_article` lists the article's attachments.
- Several YouTrack instances can be kept as named profiles, picked with `--profile` or `YMCP_PROFILE`, and each profile is registered in MCP clients as its own server.
- A repeated `ymcp setup` lets you edit, add or remove profiles, with `--name`, `--remove` and `--profile` for scripts.

### Fixed

- An instance URL without a scheme, such as `youtrack.example.com`, is accepted as `https://`.
- Terminal logs no longer prefix attributes with a group name they don't belong to.

## [0.3.1] - 2026-10-21

### Fixed

- Clients on the stateless 2026-07-28 MCP protocol, Claude Code among them, are now logged when they connect.

## [0.3.0] - 2026-10-21

### Added

- `call_api` sends raw GET, POST or DELETE requests to any YouTrack or Hub REST path as the current user, for anything the other tools don't cover.

## [0.2.1] - 2026-10-21

### Fixed

- When a proxy from the environment cannot reach YouTrack, ymcp retries directly and stays direct if that works; an explicitly configured proxy is still used as is.

## [0.2.0] - 2026-10-21

### Added

- `serve` and `call` log startup, connecting clients and every tool call to stderr and to a rotated log file, `ymcp logs [-f] [-n N]` shows it, and `-v` or `YMCP_LOG_LEVEL=debug` adds sign-in and request details.

### Fixed

- The first connection no longer takes 5 seconds behind DNS servers that ignore AAAA queries: ymcp tries IPv4 first and falls back to dual stack.
- Late terminal replies to colour queries no longer land at the shell prompt or in the MCP stream of a hand-run `ymcp serve`.
- The installer's badge uses the same red as ymcp.

## [0.1.2] - 2026-10-21

### Fixed

- Interactive forms in `auth`, `setup` and `update` no longer receive stray terminal replies such as "]11;rgb:…" in their first field.

## [0.1.1] - 2026-10-21

### Added

- ymcp is licensed under the MIT licence.

## [0.1.0] - 2026-10-21

### Added

- An MCP server for YouTrack over stdio or HTTP that signs in with your login and password, with no token or YouTrack setup, and renews its session on its own.
- Tools that mirror YouTrack's built-in MCP server (issue search, details, comments, create and update with custom fields by name, projects, users, saved searches), and work for users without admin rights.
- Extra tools beyond the built-in set: deleting issues, comments and articles, links, drafts, tags, work items and time reports, agile boards and sprints, user groups, attachments, articles, issue history, issue counts, VCS changes, saved searches and `apply_command`.
- `ymcp auth` saves the connection with the password in the system keychain, and `ymcp setup` registers ymcp in MCP clients such as Claude Code, Claude Desktop, Codex, Cursor, VS Code, Windsurf, Zed, Gemini CLI and others.
- `ymcp check`, `ymcp call` and `ymcp update`, prebuilt binaries for macOS, Linux and Windows with checksums, and install scripts for each platform.

[0.24.0]: https://github.com/xlsft/ymcp/releases/tag/v0.24.0
[0.23.0]: https://github.com/xlsft/ymcp/releases/tag/v0.23.0
[0.22.0]: https://github.com/xlsft/ymcp/releases/tag/v0.22.0
[0.21.0]: https://github.com/xlsft/ymcp/releases/tag/v0.21.0
[0.20.3]: https://github.com/xlsft/ymcp/releases/tag/v0.20.3
[0.20.2]: https://github.com/xlsft/ymcp/releases/tag/v0.20.2
[0.20.1]: https://github.com/xlsft/ymcp/releases/tag/v0.20.1
[0.20.0]: https://github.com/xlsft/ymcp/releases/tag/v0.20.0
[0.19.2]: https://github.com/xlsft/ymcp/releases/tag/v0.19.2
[0.19.1]: https://github.com/xlsft/ymcp/releases/tag/v0.19.1
[0.19.0]: https://github.com/xlsft/ymcp/releases/tag/v0.19.0
[0.18.0]: https://github.com/xlsft/ymcp/releases/tag/v0.18.0
[0.17.0]: https://github.com/xlsft/ymcp/releases/tag/v0.17.0
[0.16.0]: https://github.com/xlsft/ymcp/releases/tag/v0.16.0
[0.15.2]: https://github.com/xlsft/ymcp/releases/tag/v0.15.2
[0.15.1]: https://github.com/xlsft/ymcp/releases/tag/v0.15.1
[0.15.0]: https://github.com/xlsft/ymcp/releases/tag/v0.15.0
[0.14.0]: https://github.com/xlsft/ymcp/releases/tag/v0.14.0
[0.13.0]: https://github.com/xlsft/ymcp/releases/tag/v0.13.0
[0.12.2]: https://github.com/xlsft/ymcp/releases/tag/v0.12.2
[0.12.1]: https://github.com/xlsft/ymcp/releases/tag/v0.12.1
[0.12.0]: https://github.com/xlsft/ymcp/releases/tag/v0.12.0
[0.11.0]: https://github.com/xlsft/ymcp/releases/tag/v0.11.0
[0.10.1]: https://github.com/xlsft/ymcp/releases/tag/v0.10.1
[0.10.0]: https://github.com/xlsft/ymcp/releases/tag/v0.10.0
[0.9.2]: https://github.com/xlsft/ymcp/releases/tag/v0.9.2
[0.9.1]: https://github.com/xlsft/ymcp/releases/tag/v0.9.1
[0.9.0]: https://github.com/xlsft/ymcp/releases/tag/v0.9.0
[0.8.0]: https://github.com/xlsft/ymcp/releases/tag/v0.8.0
[0.7.1]: https://github.com/xlsft/ymcp/releases/tag/v0.7.1
[0.7.0]: https://github.com/xlsft/ymcp/releases/tag/v0.7.0
[0.6.1]: https://github.com/xlsft/ymcp/releases/tag/v0.6.1
[0.6.0]: https://github.com/xlsft/ymcp/releases/tag/v0.6.0
[0.5.0]: https://github.com/xlsft/ymcp/releases/tag/v0.5.0
[0.4.1]: https://github.com/xlsft/ymcp/releases/tag/v0.4.1
[0.4.0]: https://github.com/xlsft/ymcp/releases/tag/v0.4.0
[0.3.1]: https://github.com/xlsft/ymcp/releases/tag/v0.3.1
[0.3.0]: https://github.com/xlsft/ymcp/releases/tag/v0.3.0
[0.2.1]: https://github.com/xlsft/ymcp/releases/tag/v0.2.1
[0.2.0]: https://github.com/xlsft/ymcp/releases/tag/v0.2.0
[0.1.2]: https://github.com/xlsft/ymcp/releases/tag/v0.1.2
[0.1.1]: https://github.com/xlsft/ymcp/releases/tag/v0.1.1
[0.1.0]: https://github.com/xlsft/ymcp/releases/tag/v0.1.0
