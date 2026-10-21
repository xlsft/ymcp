<p align="center">
  <img src="docs/banner.svg" alt="ymcp — It’s fun to fetch with the YMCP" width="100%">
</p>

<p align="center">
  <a href="https://github.com/xlsft/ymcp/actions/workflows/release.yml"><img src="https://github.com/xlsft/ymcp/actions/workflows/release.yml/badge.svg" alt="Build"></a>
  <a href="https://github.com/xlsft/ymcp/actions/workflows/e2e.yml"><img src="https://github.com/xlsft/ymcp/actions/workflows/e2e.yml/badge.svg" alt="E2E"></a>
  <a href="https://github.com/xlsft/ymcp/releases/latest"><img src="https://img.shields.io/github/v/release/xlsft/ymcp?color=C62A22" alt="Release"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/xlsft/ymcp?color=0E0E0E" alt="Go">
  <a href="LICENSE"><img src="https://img.shields.io/github/license/xlsft/ymcp?color=0E0E0E" alt="MIT"></a>
</p>

**ymcp** is a local [MCP](https://modelcontextprotocol.io) server for [YouTrack](https://www.jetbrains.com/youtrack/),
including self-hosted instances behind a VPN. It speaks for you with your **login and password**, ships every tool of
YouTrack’s built-in MCP server under the same names, and adds the ones it lacks: deleting things, sprints, attachments,
history, time reports, commands and more.

- **56 tools**: the 23 built-in ones, same names, arguments and limits, plus 33 extras
- **Signs in like the web UI**, so there is no admin-issued token or OAuth client; the password lives in your system keychain
- **Works without admin rights**: field schemas, groups and comments fall back to what a regular user can read
- **One-command install** that registers itself in Claude, Cursor, VS Code, Zed, Codex, Gemini and 8 more clients
- **Single static binary** for macOS, Linux and Windows, with `ymcp update` built in

## Install

macOS / Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/xlsft/ymcp/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/xlsft/ymcp/main/install.ps1 | iex
```

The installer downloads the latest release and checks its SHA-256. It then runs `ymcp setup`, which:

1. asks for your YouTrack URL, login and password, and verifies them before saving anything;
2. lists the MCP clients found on your machine, with a checkbox for each;
3. registers ymcp as the `youtrack` stdio server in the clients you pick, and removes it from the ones you uncheck.

Run `ymcp setup` again at any time to add or remove clients.

## Commands

| Command | |
|---|---|
| `ymcp` / `ymcp serve` | Run the MCP server over stdio (what clients launch). `--transport http --addr 127.0.0.1:8090` serves Streamable HTTP |
| `ymcp setup` | Sign in and pick MCP clients. `--clients cursor,zed`, `--all` and `--reauth` skip the questions |
| `ymcp auth` | Set or replace the URL, login and password. `--url`, `--login`, `--proxy`, `--password-stdin` for scripts |
| `ymcp check` | Sign in and print who you are |
| `ymcp call <tool> '<json>'` | Call one tool, exactly as a client would, e.g. `ymcp call count_issues '{"query":"#Unresolved"}'` |
| `ymcp logs` | Show what the server did; `-f` follows the log live, `-n 200` shows more lines |
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
| VCS | `get_issue_vcs_changes`: commits and pull requests from GitHub, GitLab, Bitbucket, Gitea or TeamCity integrations |
| Tags and saved searches | `find_tags`, `create_tag`, `update_tag`, `delete_tag`, `create_saved_search`, `update_saved_search`, `delete_saved_search` |
| Escape hatch | `call_api`: any YouTrack or Hub REST endpoint as you, for whatever the other tools don't cover |

Tools that delete or bulk-change data are marked with `destructiveHint`, so clients ask before running them.

## Configuration

`ymcp auth` stores the URL, login and proxy in `<config dir>/ymcp/config.json`, a file only you can read:

- macOS: `~/Library/Application Support`
- Linux: `~/.config`
- Windows: `%APPDATA%`

The password goes to the system keychain: Keychain on macOS, Secret Service on Linux, Credential Manager on Windows.
It falls back to the config file only where no keychain exists.

Environment variables override the stored values. That's handy for CI or for several instances:

| Variable | |
|---|---|
| `YOUTRACK_URL` | Instance root, e.g. `https://youtrack.example.com` (no `/api`) |
| `YOUTRACK_LOGIN`, `YOUTRACK_PASSWORD` | YouTrack or Hub account, LDAP included |
| `YOUTRACK_PROXY` | Empty to use `HTTP(S)_PROXY` (or direct, when that proxy can't reach YouTrack), `none` to always connect directly, or a proxy URL |
| `YMCP_CONFIG` | Path of the config file |

`--env FILE` loads these variables from a dotenv file (see [`.env.example`](.env.example)).

> **Behind a VPN?** MCP clients often pass an `HTTP(S)_PROXY` meant for the internet. Such a proxy resolves an
> internal YouTrack through public DNS and never reaches it. When the proxy comes from the environment and doesn't
> lead to YouTrack, ymcp connects directly instead and logs why. An explicitly configured proxy is always used.

### Logs

The server logs its start, every client that connects and every tool call. Each line has the arguments, how long the
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

To log at debug level from a client, add `"env": {"YMCP_LOG_LEVEL": "debug"}` to its ymcp entry.

### Supported clients

Claude Code · Claude Desktop · Cursor · VS Code (Copilot) · Windsurf · Devin Desktop · Zed · Codex · Gemini CLI ·
Antigravity · JetBrains Junie · Cline · LM Studio · opencode

ymcp edits each client's config in place and keeps your other servers, keys and comments. It leaves a
`.ymcp.bak` copy next to the original. For any other client, register a stdio server named `youtrack` that runs
the `ymcp` binary with no arguments:

```json
{ "mcpServers": { "youtrack": { "command": "/path/to/ymcp", "args": [] } } }
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

E2E tests ([`e2e/`](e2e)) start the built binary over stdio against a real YouTrack. They call all 56 tools on a
throwaway project and delete everything they create. They run in CI on every push and nightly.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the commit and release flow.

## License

[MIT](LICENSE). Not affiliated with JetBrains; YouTrack is a trademark of JetBrains s.r.o.
