# Installing ymcp: notes for AI agents

These are the steps for an agent (Cline, Claude Code and the like) that sets ymcp up for its user. ymcp is a single
binary that serves YouTrack over MCP on stdio, signed in with the user's own login and password.

## 1. Install the binary

macOS / Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/xlsft/ymcp/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/xlsft/ymcp/main/install.ps1 | iex
```

Or `brew install xlsft/tap/ymcp`, or `scoop bucket add xlsft https://github.com/xlsft/scoop-bucket; scoop install ymcp`.

The installer runs `ymcp setup` when it finishes, if it has a terminal to ask in; set `YMCP_NO_SETUP=1` to skip that
and leave it to step 2. Then find the binary's full path with `command -v ymcp` (`where ymcp` on Windows): MCP
clients don't always have the same `PATH` as a shell.

## 2. Sign in

Ask the user to run this in their own terminal:

```sh
ymcp setup
```

It asks for the YouTrack URL, login and password, checks them, and keeps the password in the system keychain. It
then offers to register ymcp in the MCP clients it finds, Cline included; the user can tick yours there and skip
step 3.

Don't ask the user to type their password into the chat. If they still want ymcp configured through environment
variables, put them in your MCP config's `env` (step 3) and nowhere else.

## 3. Register the server

If `ymcp setup` didn't register it, add a stdio server to your MCP settings, with the path from step 1:

```json
{
  "mcpServers": {
    "youtrack": {
      "command": "/full/path/to/ymcp",
      "args": ["--profile", "youtrack"]
    }
  }
}
```

`youtrack` is the profile `ymcp setup` makes by default; a user with several YouTrack instances picks the profile there.

Without `ymcp setup`, the server reads its connection from the environment instead:

```json
{
  "mcpServers": {
    "youtrack": {
      "command": "/full/path/to/ymcp",
      "args": ["serve"],
      "env": {
        "YOUTRACK_URL": "https://youtrack.example.com",
        "YOUTRACK_LOGIN": "jane.doe",
        "YOUTRACK_PASSWORD": "..."
      }
    }
  }
}
```

`YOUTRACK_URL` is the instance root, without `/api`. `YMCP_TOOLS` (for example `issues,articles`) limits the tools
it serves.

## 4. Check it

`ymcp check` (with `--profile KEY` for another profile) signs in and prints the current user, and so does the
`get_current_user` tool once the client has started the server. A sign-in error means the URL, login or password is
wrong; `ymcp setup --reauth` asks for them again.
