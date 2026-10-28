<p align="center">
  <img src="docs/banner.svg" alt="ymcp — It’s fun to fetch with the YMCP" width="100%">
</p>

<div align="center">

  <a href="https://github.com/xlsft/ymcp/actions/workflows/release.yml"><img src="https://github.com/xlsft/ymcp/actions/workflows/release.yml/badge.svg" alt="Build"></a>
  <a href="https://github.com/xlsft/ymcp/actions/workflows/e2e.yml"><img src="https://github.com/xlsft/ymcp/actions/workflows/e2e.yml/badge.svg" alt="E2E"></a>
  <a href="https://github.com/xlsft/ymcp/releases/latest"><img src="https://img.shields.io/github/v/release/xlsft/ymcp?color=C62A22" alt="Release"></a>
  <a href="https://hub.docker.com/r/xlsoftware/ymcp"><img src="https://img.shields.io/docker/pulls/xlsoftware/ymcp?label=docker%20pulls&color=0E0E0E" alt="Docker pulls"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/xlsft/ymcp?color=0E0E0E" alt="Go">
  <a href="LICENSE"><img src="https://img.shields.io/github/license/xlsft/ymcp?color=0E0E0E" alt="MIT"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/xlsft/ymcp"><img src="https://api.scorecard.dev/projects/github.com/xlsft/ymcp/badge" alt="OpenSSF Scorecard"></a>
[![M8ven Score](https://m8ven.ai/badge/mcp/xlsft-ymcp-fbkn16?v=2e1db4e584becf064c6d5bf20227fcc6)](https://m8ven.ai/mcp/xlsft-ymcp-fbkn16?s=readme)

</div>

**ymcp** is a local toolchain for project work in [YouTrack](https://www.jetbrains.com/youtrack/), including
self-hosted instances behind a VPN. It has two parts: an MCP server for any AI client, and pipelines that hand
issues to coding agents.

The [MCP](https://modelcontextprotocol.io) **server** speaks for you with your **login and password**, ships every tool
of YouTrack’s built-in MCP server under the same names, and adds the ones it lacks: deleting things, sprints,
attachments, history, time reports, commands and more.

- **58 tools**: the 23 built-in ones, same names, arguments and limits, plus 35 extras
- **Signs in like the web UI**, so there is no admin-issued token or OAuth client; the password lives in your system keychain
- **Works without admin rights**: field schemas, groups and comments fall back to what a regular user can read
- **One-command install** that registers itself in Claude, Cursor, VS Code, Zed, Codex, Gemini and 8 more clients
- **Single static binary** for macOS, Linux and Windows, with `ymcp update` built in

**Pipelines** take the issues a YouTrack search finds and give each one to a coding agent on its own branch.

- **Claude Code, Kimi, opencode or any ACP agent**, with the same YouTrack tools as the server
- **Branches, GitHub pull requests or GitLab merge requests**, and the agent answers review comments
- **Needs input** when the agent has a question or wants a permission, with a system notification
- **A TUI** with a board per pipeline, or plain commands for scripts

## Install

macOS / Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/xlsft/ymcp/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/xlsft/ymcp/main/install.ps1 | iex
```

Or with a package manager, then run `ymcp setup`:

```sh
brew install xlsft/tap/ymcp                                                  # macOS, Linux
scoop bucket add xlsft https://github.com/xlsft/scoop-bucket; scoop install ymcp   # Windows
winget install xlsft.ymcp                                                    # Windows, once Microsoft accepts it
```

Package-managed copies update through their manager (`brew upgrade ymcp` and so on); `ymcp update` says so.

The installer downloads the latest release and checks its SHA-256. It then runs `ymcp setup`, which:

1. asks for a profile name, `youtrack` by default;
2. asks for your YouTrack URL, login and password, and verifies them before saving anything;
3. lists the MCP clients found on your machine, with a checkbox for each;
4. registers ymcp as a stdio server in the clients you pick, and removes it from the ones you uncheck.

Run `ymcp setup` again at any time to edit a profile, add or remove clients, or add another YouTrack.

### Several YouTrack instances

Each YouTrack instance is a profile, and each profile is its own MCP server. The profile's display name gives the
server name: `YouTrack (work)` becomes `youtrack-work`, `youtrack-acme` stays as it is. A repeated `ymcp setup`
lists your profiles: pick one to edit it, rename it (its client entries move along), or add or remove a profile.
Clients run `ymcp --profile <key>`, so every instance shows up next to the others with its own tools.

### Verify a release

Releases are signed keylessly with [Sigstore](https://www.sigstore.dev) by this repository's release workflow:
`checksums.txt` has a `checksums.txt.sigstore.json` bundle, and each archive an SPDX SBOM (`*.sbom.json`).

```sh
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/xlsft/ymcp/\.github/workflows/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
```

The Docker image is signed the same way and carries SBOM and provenance attestations:

```sh
cosign verify xlsoftware/ymcp:latest \
  --certificate-identity-regexp '^https://github.com/xlsft/ymcp/\.github/workflows/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Commands

| Command | |
|---|---|
| `ymcp` / `ymcp serve` | Run the MCP server over stdio (what clients launch). `--transport http` serves Streamable HTTP, see [HTTP](#http) |
| `ymcp setup` | Add or edit a profile, sign in, pick its tools and MCP clients. `--name`, `--tools`, `--clients cursor,zed`, `--all`, `--reauth` and `--remove` skip the questions |
| `ymcp auth` | Set or replace the URL, login and password of a profile. `--url`, `--login`, `--proxy`, `--password-stdin` for scripts |
| `ymcp check` | Sign in and print who you are |
| `ymcp call <tool> '<json>'` | Call one tool, exactly as a client would, e.g. `ymcp call count_issues '{"query":"#Unresolved"}'` |
| `ymcp health` | Check that a running HTTP server answers (`--url`, default `http://127.0.0.1:8090`) |
| `ymcp logs` | Show what the server did; `-f` follows the log live, `-n 200` shows more lines |
| `ymcp pipeline` | Open the pipelines TUI; `ymcp pipeline --help` lists `new`, `run`, `check`, `answer` and the rest, see [Pipelines](#pipelines) |
| `ymcp update` | Replace the binary with the latest release (`--check` only reports) |
| `ymcp version` | Print the version |

## Tools

### Built-in YouTrack MCP set

| Area | Tools |
|---|---|
| Issues | `search_issues`, `get_issue`, `create_issue`, `create_draft_issue`, `update_issue`, `change_issue_assignee`, `get_issue_fields_schema` |
| Comments, tags, links, time | `add_issue_comment`, `get_issue_comments`, `manage_issue_tags`, `link_issues`, `log_work`, `get_saved_issue_searches` |
| Knowledge base | `search_articles`, `get_article`, `create_article`, `update_article` |
| Projects and people | `find_projects`, `get_project`, `find_user`, `get_current_user`, `find_user_groups`, `get_user_group_members` |

Custom fields are set by their display name, e.g. `{"State": "In Progress", "Fix versions": ["2.1"]}`. ymcp looks up
the field type and checks the value against the project before sending it. That includes the allowed assignees, so a
wrong value comes back with the list of valid ones.

### Extras

| Area | Tools |
|---|---|
| Delete and undo | `delete_issue`, `delete_issue_comment`, `restore_issue_comment`, `delete_article`, `delete_work_item`, `unlink_issues` |
| Agile boards and sprints | `find_agile_boards`, `get_agile_board`, `get_sprint`, `create_sprint`, `update_sprint`, `add_issue_to_sprint`, `remove_issue_from_sprint`, `delete_sprint` |
| History and reports | `get_issue_activities`, `count_issues`, `get_time_report`, `get_issue_work_items` |
| Bulk changes | `apply_command`: any command from the YouTrack command dialog, on issues or a query (up to 100), validated first, with `dryRun` |
| Attachments | Of issues and knowledge base articles: `get_issue_attachments`, `get_attachment` (text inline, images as images, other files saved locally), `add_attachment` (several files at once), `delete_attachment` |
| Comments | `update_issue_comment` |
| Partial text edits | `edit_issue_description`, `edit_article_content`: find-and-replace in a description or article, exact or by regular expression with capture groups, so a small change doesn't resend the whole text |
| VCS | `get_issue_vcs_changes`: commits and pull requests from GitHub, GitLab, Bitbucket, Gitea or TeamCity integrations |
| Tags and saved searches | `find_tags`, `create_tag`, `update_tag`, `delete_tag`, `create_saved_search`, `update_saved_search`, `delete_saved_search` |
| Escape hatch | `call_api`: any YouTrack or Hub REST endpoint as you, for whatever the other tools don't cover |

Every command takes `--profile <key>` (or `YMCP_PROFILE`) to pick the YouTrack instance, e.g.
`ymcp --profile youtrack-acme check`. Without it ymcp uses the `youtrack` profile, or the only one there is.

Tools that delete or bulk-change data are marked with `destructiveHint`, so clients ask before running them.

Errors tell the model what to do next. ymcp's own checks list what is allowed (`value "X" is not allowed; allowed:
Critical, Major, …`), and errors from YouTrack get a hint by status: which tool finds the right ID after a 404,
that a 403 needs a permission rather than a retry, that a 401 needs `ymcp auth`. HTML error pages from proxies are
cut down to their title.

### Prompts

Clients that support MCP prompts list these as slash commands. Each one tells the model which tools to use and in
what order, and they never change YouTrack without asking:

| Prompt | |
|---|---|
| `standup` | What I did since the last working day (Friday on a Monday), what's next, what blocks me. `days` looks further back |
| `triage` | Untriaged issues of a `project` with proposed type, priority, assignee and duplicates; applies only what you approve |
| `sprint_report` | A sprint of a `board`: done and open work, who has what, scope changes, time logged, risks |
| `issue_brief` | Catch up on an `issue`: TL;DR, state, decisions, open questions, next steps |

A prompt is offered only when the tools it needs are served (see below).

### Choosing tools

All 58 tools take room in the model's context. A profile can serve only some groups of them: pick them in
`ymcp setup`, or pass `--tools` (or `YMCP_TOOLS`) to `serve`, which wins over the profile.

| Group | Tools |
|---|---|
| `issues` | search, count, read, create, edit, drafts, delete, commands, links, history, VCS changes |
| `comments` | read, add, edit, delete, restore |
| `attachments` | files on issues and articles |
| `articles` | knowledge base |
| `time` | work items and time reports |
| `agile` | boards and sprints |
| `projects` | find and describe projects |
| `people` | users and groups |
| `tags` | tags and tagging issues |
| `searches` | saved searches |
| `api` | `call_api` |

A selection mixes groups and tool names, and `-` removes one: `issues,comments,articles,-delete_issue`. Removals
alone start from everything, e.g. `-api,-agile`. The start-up log shows what is served, e.g.
`tools="19 of 58: issues, comments, -delete_issue"`.

## Pipelines

A pipeline watches a YouTrack search and gives each issue it finds to a coding agent. The agent works in its own git
worktree on a new branch. ymcp pushes the branch, opens a pull or merge request and brings the agent the review
comments until the change is merged. Pipelines need a local install: they are not in the Docker image. The agent works
on this machine or on an SSH host (`workspace.host`, an alias from `~/.ssh/config`).

<p>
  <img src="https://raw.githubusercontent.com/xlsft/ymcp/site/screens/board.png" alt="A pipeline's board: backlog, working, needs input, review and done" width="49%">
  <img src="https://raw.githubusercontent.com/xlsft/ymcp/site/screens/run.png" alt="A run card: the issue, its comments and the agent's question" width="49%">
  <img src="https://raw.githubusercontent.com/xlsft/ymcp/site/screens/permission.png" alt="The agent asks for a permission; /approve or /reject answers" width="49%">
  <img src="https://raw.githubusercontent.com/xlsft/ymcp/site/screens/fields.png" alt="Changing the issue's fields from the card" width="49%">
</p>

The [Site workflow](.github/workflows/site.yml) draws these from the TUI's own code on every change to `main`.

Run `ymcp pipeline` to open the TUI: your profiles, their pipelines as cards, a board per pipeline and an inbox of
runs that wait for you. Press `n` to create a pipeline with the wizard. `enter` on a card of the board, a run or an
issue still in the backlog, opens it like a YouTrack issue: the description and comments, the pull request thread
(`[` and `]`) and the agent's log, with a comment box (`enter`) whose `/` completes the commands below; `r` reloads the
board or the card. `t` opens the run's transcript over the card: what the agent thinks and says, every tool it runs
with the file, command or pattern it looks at, and the replies it gets, followed live. Every field of the issue that
has a value shows at the top of the card, four to a row, beside the title or under it when it doesn't fit. `e` lists the project's fields to change one: pick a value,
add or remove one of several, or type a number. A backlog issue the source query no longer finds after the change
leaves the board. Shortcuts work on Cyrillic keyboard layouts too. In kitty and Ghostty the card shows pictures in place, pasted
screenshots and attachments included, and the agent's working loader plays; other terminals, and tmux, get links to
them. `YMCP_IMAGES=kitty` or `YMCP_IMAGES=off` overrides the choice. Or from the command line:

```sh
ymcp pipeline new backend --profile youtrack --dir ~/src/backend --query 'project: BE #Unresolved tag: ai'
ymcp pipeline backend edit     # adjust the stage commands to your workflow
ymcp pipeline backend check    # profile, query, stages, git and agent
ymcp pipeline backend run -d   # run in the background; `stop` stops it
```

`ymcp pipeline --help` lists every command, including `status`, `runs`, `logs`, `answer`, `cancel` and `delete`. Each
pipeline is a JSON file in `<config dir>/ymcp/pipelines/`, and runs are kept in `<config dir>/ymcp/state.db`.

### Stages

The source query uses the same syntax as a saved search. Each issue it finds moves along the pipeline's board:

| Stage | |
|---|---|
| Backlog | Issues the source query finds and no run has taken yet |
| Working | The agent works on the issue. ymcp applies the stage's YouTrack command, `State In Progress` by default |
| Needs input | The agent asked a question, wants a permission, timed out or failed. ymcp says why in a comment and applies `tag needs-input` |
| Review | The agent is done and the branch is pushed. Default command `State Fixed` |
| Done | Off by default. Reached when the change is merged or the issue matches the stage's query |
| Closed | The change was closed without a merge, or the issue matches the stage's query. Its optional command also runs on a `/cancel` reply |

Each stage has an `enter` command, as typed in YouTrack's command dialog, and a `match` query that recognizes issues
in it. The working command should take the issue out of the source query: moving it back there cancels the run. A
canceled run (also `ymcp pipeline NAME cancel N`) closes its pull or merge request with a comment, keeps its branch and
removes its worktree unless `workspace.keepWorktree` is set; a later run of the issue starts on a new branch. An issue
moved back to the backlog to cancel its run is not taken again until it leaves the source; one canceled otherwise is
taken again once it is back in the source. A change closed without a merge ends the run as closed.
`workspace.parallel` sets how many issues run at once (1).

A run that has ended can be deleted from the local store with `d` on the board or the run's card, or with
`ymcp pipeline NAME delete N`: its log goes, the issue, branch and change stay, and an issue the backlog held is taken
again. A canceled run can be deleted once a runner has closed its change and removed its worktree.

The agent commits as the repository's `user.name` and `user.email`. With `workspace.identity.enabled` it gets an author
of its own in each worktree instead, written to the worktree's config only. `workspace.identity.name` is a template of
the name (`{{agent.name}} · {{pipeline.name}} #{{run.number}}`) and `workspace.identity.tag` of what goes after `+` in
your address (`agent_{{pipeline.name}}_{{run.number}}`, so `dev@example.com` becomes
`dev+agent_backend_3@example.com`). Both take the issue's keys too; the tag keeps lowercase letters, digits and `_`.

Every commit message starts with `workspace.commitPrefix` (`{{issue.id}}`). Set it to `""` for none, e.g. when the
repository runs commitlint: the agent then writes its commit messages the repository's way. `strategy.title` is the
template of the PR/MR title (`{{issue.key}} {{issue.summary}}`); `{{commit.subject}}` is the subject of the branch's
first commit, so `"title": "{{commit.subject}}"` gives a conventional title, which a squash merge keeps.

### Agents

| `agent.provider` | |
|---|---|
| `claude` | Claude Code through its CLI, `claude -p` with stream-json, one process per turn and `--resume` in between |
| `kimi` | Kimi Code over [ACP](https://agentclientprotocol.com), `kimi acp` |
| `opencode` | opencode over ACP, `opencode acp` |
| `acp` | Any other agent that speaks ACP, started by `agent.command` |

Each run gets its own MCP server on a loopback port with its own token, and the agent gets the YouTrack tools
through it, as the pipeline's profile. The password and the YouTrack token stay in ymcp and never leave your machine.
Two more tools reach the runner: `request_input` asks you a question, `report` posts a short status.

What the agent may do is up to the agent's own settings: its mode (`agent.mode`, e.g. Claude's `acceptEdits`) and
its arguments (`agent.args`, e.g. `--allowedTools`). When it asks for a tool it may not use, the pipeline's
`permissions` decides: `ask` (default) puts the run in needs input, `allow` approves every request, `deny` refuses
every one and the agent goes on without the tool. New Claude pipelines start with `--dangerously-skip-permissions` in
`agent.args`, so Claude runs every tool without asking; replace it with a mode or `--allowedTools` to restrict it.
New Kimi pipelines start in Kimi's `yolo` mode for the same reason; `auto` approves only what Kimi deems safe and
`default` asks about everything.

Before each turn ymcp fetches `origin`, so the agent can rebase onto the base branch without fetching itself.

### Strategies

| `strategy.kind` | |
|---|---|
| `branch` | Push the branch only (default). Feedback comes from YouTrack alone |
| `github-pr` | A pull request |
| `github-issue-pr` | A GitHub issue that mirrors the YouTrack issue, and a pull request that closes it |
| `gitlab-mr` | A merge request on gitlab.com or a self-hosted GitLab, found from the `origin` URL |

`strategy.draft` opens them as drafts. For `gitlab-mr`, `strategy.squash` and `strategy.deleteBranch` tick the merge
request's "Squash commits" and "Delete source branch" options. The token comes from `gh` or `glab`, whichever you are signed in to for that
host, or from the system keychain (service `ymcp`, account `vcs:<host>`).

### Agent messages

Everything the agent writes, in YouTrack or on the pull request, starts with a hidden marker and a header:

```
<!-- ymcp:agent pipeline=backend run=17 msg=3f9a -->
@xlsft's agent #17 (backend)

Done: branch BE-123-fix-login, PR https://github.com/acme/api/pull/42
```

The marker is an HTML comment, so YouTrack, GitHub and GitLab don't show it; ymcp uses it to tell the agent's
messages from yours. The header names whose agent it is, the run and the pipeline. When the agent starts on a
message, ymcp replies right away with an animated ymcp logo, and later edits that comment into the
answer, or into the reason the run stopped.

### Feedback and needs input

The agent reacts to comments from you: the profile's YouTrack account and the `gh` or `glab` account. Add other
logins to `feedback.youtrack`, `feedback.github` and `feedback.gitlab`; comments from anyone else are skipped.
Comments that arrive within `feedback.debounce` (60s) go to the agent together.

A run waits in needs input when the agent asks a question, wants a permission, stays silent for `timeouts.silence`
(10m), runs longer than `timeouts.total` (2h) or fails. The first reply from an allowed author continues it: a
comment in YouTrack or on the pull request, the TUI's inbox, or `ymcp pipeline backend answer 17 "…"`. A permission
request takes `/approve` or `/reject`; without a reply within `timeouts.permission` (30m) it is refused.
For Claude, `/approve` allows exactly the calls that were denied for the rest of the run (`Bash(<command>)`,
`Edit(//<path>)`, …), never the whole tool; a compound command gets a rule for each of its commands, and a call that
can't be spelled as such a rule (a wildcard, a subshell) stays denied.

Three more replies are carried out by ymcp itself, the agent never sees them:

| Reply | |
|---|---|
| `/clear` | Hides every comment of the run, yours and the agent's, in YouTrack and on the pull or merge request. They stay in the history: YouTrack marks them deleted and can restore them, GitHub folds them as outdated, GitLab resolves their threads (a plain comment outside a thread stays visible) |
| `/compact` | Has the agent sum the run up in the pipeline's `language`, then clears it and leaves the summary, with the links to the change and the branch, as the one comment |
| `/cancel [why]` | Cancels the run as `ymcp pipeline NAME cancel N` does, and closes the issue with the closed stage's command |

`"autoCompact": true` compacts every run that ends as done or closed. A command runs once: restoring its comment does
not run it again.

### Notifications

By default, when a run needs input or fails, ymcp shows a system notification with its icon. A click opens the
question, the pull request or the issue. It works on macOS, Windows and Linux (with a notification daemon on D-Bus).

```sh
ymcp pipeline notify                                   # what is on and whether it works here
ymcp pipeline notify test                              # send a test notification
ymcp pipeline notify set toast.events=needs_input,run_failed,review_ready
```

The events are `needs_input`, `run_failed`, `review_ready`, `run_done` and `runner_stopped`. A pipeline can narrow
them with `"notifications": {"events": [...]}` in its JSON.

### Prompt presets

The agent's prompts come from a preset: `fix` (default), `bugfix-test-first`, `feature`, `refactor`, `docs` or
`investigate`. `ymcp pipeline presets` shows what each is for. `prompts.start`, `prompts.feedback` and
`prompts.answer` replace a preset's texts, with placeholders such as `{{issue.id}}` and `{{branch}}`.
`language` (`English` by default) is the language of everything the agent writes in public: comments, pull requests
and commit messages.

The built-in presets are read-only; your own live in `presets/` next to the config file.
`ymcp pipeline presets copy fix mine` makes one to change with `presets edit mine`, and
`presets save NAME --from PIPELINE` keeps the prompts you tuned in a pipeline. `rename` and `rm` do what they say.
A pipeline copies a preset's texts, so changing or removing a preset never changes a pipeline.

## Configuration

`ymcp setup` and `ymcp auth` store each profile's name, URL, login and proxy in `<config dir>/ymcp/config.json`,
a file only you can read:

- macOS: `~/Library/Application Support`
- Linux: `~/.config`
- Windows: `%APPDATA%`

The password goes to the system keychain: Keychain on macOS, Secret Service on Linux, Credential Manager on Windows.
It falls back to the config file only where no keychain exists.

Environment variables override the stored values of the profile in use. That's handy for CI:

| Variable | |
|---|---|
| `YOUTRACK_URL` | Instance root, e.g. `https://youtrack.example.com` (no `/api`) |
| `YOUTRACK_LOGIN`, `YOUTRACK_PASSWORD` | YouTrack or Hub account, LDAP included |
| `YOUTRACK_PROXY` | Empty to use `HTTP(S)_PROXY` (or direct, when that proxy can't reach YouTrack), `none` to always connect directly, or a proxy URL |
| `YMCP_PROFILE` | Profile to use, like `--profile` |
| `YMCP_TOOLS` | Tools to serve, like `serve --tools` |
| `YMCP_CONFIG` | Path of the config file |

`--env FILE` loads these variables from a dotenv file (see [`.env.example`](.env.example)).

> **Behind a VPN?** MCP clients often pass an `HTTP(S)_PROXY` meant for the internet. Such a proxy resolves an
> internal YouTrack through public DNS and never reaches it. When the proxy comes from the environment and doesn't
> lead to YouTrack, ymcp connects directly instead and logs why. An explicitly configured proxy is always used.

### HTTP

`ymcp serve --transport http` serves every profile from one process, each at its own path, over Streamable HTTP:

```sh
YMCP_HTTP_TOKEN=$(openssl rand -hex 32) ymcp serve --transport http --addr 127.0.0.1:8090
```

The start-up log lists each profile with its URL, e.g. `http://127.0.0.1:8090/youtrack-acme`. The default profile
is also served at `/`. `--profiles a,b` serves only those, and `--profile a` serves just one. A profile that can't sign
in is skipped with a warning.

`GET /healthz` answers `{"status":"ok","version":"…"}` without the token, for load balancers and monitoring, and
`ymcp health` checks it from the command line. The Docker image uses it as its `HEALTHCHECK`.

With `--token` or `YMCP_HTTP_TOKEN`, every request must carry `Authorization: Bearer <token>`; set that header in
the client's entry for the URL. Without a token the server is open to anyone who can reach it, so keep it on
`127.0.0.1` (the default) or set a token; ymcp warns when it listens elsewhere without one.

### Docker

The [`xlsoftware/ymcp`](https://hub.docker.com/r/xlsoftware/ymcp) image runs the HTTP server for linux/amd64 and linux/arm64.
Profiles live in the `/config` volume. There is no keychain in a container, so passwords are kept in that volume's
config file (mode 0600); treat the volume as a secret. The image has the MCP server only; [pipelines](#pipelines)
need a local install.

Add a profile. `auth` asks for the URL, login and password and checks them before saving:

```sh
docker run -it --rm -v ymcp:/config xlsoftware/ymcp auth
docker run -it --rm -v ymcp:/config xlsoftware/ymcp --profile youtrack-acme auth   # another instance
```

Run the server on port 8090 with a token:

```sh
docker run -d --name ymcp --restart unless-stopped \
  -p 127.0.0.1:8090:8090 -v ymcp:/config \
  -e YMCP_HTTP_TOKEN="$(openssl rand -hex 32)" \
  xlsoftware/ymcp
docker logs ymcp   # lists each profile and its URL
```

For a single instance without a volume, pass the connection instead:
`-e YOUTRACK_URL=https://youtrack.example.com -e YOUTRACK_LOGIN=… -e YOUTRACK_PASSWORD=…`.

Then point clients at a profile's URL with the token, e.g. in Claude Code:

```sh
claude mcp add --transport http youtrack http://127.0.0.1:8090/youtrack --header "Authorization: Bearer <token>"
```

or in a JSON config:

```json
{ "mcpServers": { "youtrack": { "type": "http", "url": "http://127.0.0.1:8090/youtrack",
  "headers": { "Authorization": "Bearer <token>" } } } }
```

The image serves stdio too, one instance per client, and that is how the
[MCP Registry](https://registry.modelcontextprotocol.io) lists it, as `io.github.xlsft/ymcp`. Clients that install from
the registry run:

```sh
docker run -i --rm -e YOUTRACK_URL -e YOUTRACK_LOGIN -e YOUTRACK_PASSWORD xlsoftware/ymcp serve
```

### Retries

A busy or restarting YouTrack doesn't fail a tool call right away. ymcp retries up to three times, waiting 0.5,
1 and 2 seconds or as long as `Retry-After` asks (up to 10 s): on 429 and 503 for any request, and on 502, 504 and
dropped connections only for requests that are safe to repeat. A POST that may already have created an issue or
comment is never sent twice. Each retry is logged.

### Logs

The server logs its start, every client that connects and every tool call. At start it also checks GitHub in the
background and, when a newer release is out, logs `update available current=0.4.0 latest=0.5.0 run="ymcp update"`. Each line has the arguments, how long the
call took and any error. Sign-ins are logged too. The log goes to stderr, which clients like Claude Desktop keep in
their own MCP logs. It is also appended to a file, so you can watch it while a client runs ymcp:

```sh
ymcp logs -f
```

The file is `~/Library/Logs/ymcp/ymcp.log` on macOS, `~/.local/state/ymcp/ymcp.log` on Linux and
`%LOCALAPPDATA%\ymcp\logs\ymcp.log` on Windows. It rotates at 5 MB.

| Setting | |
|---|---|
| `ymcp -v serve` or `YMCP_LOG_LEVEL=debug` | Also log every YouTrack and Hub request with status and timing |
| `YMCP_LOG_LEVEL` | `debug`, `info` (default), `warn` or `error` |
| `YMCP_LOG_FILE` | Another path for the file, or `off` |
| `YMCP_UPDATE_CHECK=off` | Don't look for a newer release at start |

To log at debug level from a client, add `"env": {"YMCP_LOG_LEVEL": "debug"}` to its ymcp entry.

### Supported clients

Claude Code · Claude Desktop · Cursor · VS Code (Copilot) · Windsurf · Devin Desktop · Zed · Codex · Gemini CLI ·
Antigravity · JetBrains Junie · Cline · LM Studio · opencode · Kimi Code

Claude Desktop writes its config back from memory while it runs, which drops edits made meanwhile, so `ymcp setup`
offers to quit it during the change and reopens it afterwards.

ymcp edits each client's config in place and keeps your other servers, keys and comments. It leaves a
`.ymcp.bak` copy next to the original. For any other client, register a stdio server per profile that runs the
`ymcp` binary with `--profile <key>`:

```json
{ "mcpServers": { "youtrack": { "command": "/path/to/ymcp", "args": ["--profile", "youtrack"] } } }
```

## How sign-in works

YouTrack’s OAuth password grant needs a service secret, and regular users don't have one. So ymcp signs in the
same way the YouTrack web app does:

1. Read the Hub location and YouTrack service ID from the public `/api/config`.
2. Submit the Hub login form and keep the session cookie.
3. Run the implicit OAuth flow of the YouTrack web client and take the bearer token from the redirect.

Expired tokens are renewed from the session without resending the password. The password is sent again only when the
session itself has expired. A separate Hub-scoped token is fetched lazily for the Hub API, which group members need.
Two-factor authentication is not supported yet.

## Development

```sh
go build -o bin/ymcp ./cmd/ymcp
go test -race ./...
./bin/ymcp --env .env call get_current_user
```

Unit tests never touch the network. [`internal/youtrack/youtracktest`](internal/youtrack/youtracktest) emulates
YouTrack and Hub, including the login form, the OAuth redirects and token revocation. The tool tests call tools through
an in-memory MCP client and check the REST requests they send.

E2E tests ([`e2e/`](e2e)) start the built binary over stdio against a real YouTrack. They call all 58 tools on a
throwaway project and delete everything they create. They run in CI on every push and nightly.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the commit and release flow.

## License

[MIT](LICENSE). Not affiliated with JetBrains; YouTrack is a trademark of JetBrains s.r.o.
