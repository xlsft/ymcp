# Security

## Reporting a vulnerability

Report it privately through [GitHub's vulnerability reporting](https://github.com/xlsft/ymcp/security/advisories/new),
not in a public issue. Say what you found, how to reproduce it and which version you ran (`ymcp version`). You get an
answer within a few days; the fix ships in the next release, and the advisory is published with it.

## Supported versions

Only the latest release gets fixes. `ymcp update`, or your package manager, moves you to it.

## What counts

ymcp holds YouTrack passwords and tokens, so these matter most:

- a password or token reaching a log, an error message, a tool result or anywhere outside the system keychain
  (or the config file where there is no keychain);
- the HTTP server answering without the token it was started with;
- a pipeline's loopback MCP server reachable by anything but its own agent;
- release archives, checksums or the Docker image that don't verify as described in the README.

A YouTrack account doing what its own permissions allow is not a vulnerability in ymcp.
