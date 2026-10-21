<p align="center">
  <img src="https://raw.githubusercontent.com/xlsft/ymcp/main/docs/banner.svg" alt="ymcp" width="720">
</p>

# ymcp: YouTrack MCP server

[ymcp](https://github.com/xlsft/ymcp) gives AI clients the full YouTrack MCP toolset, plus deletes, sprints,
attachments, history, time reports and a raw API tool. This image runs it as a
[Streamable HTTP](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports) server, serving every
configured YouTrack instance (profile) on one port. It signs in with a login and password, like the YouTrack web UI.

## Quick start

Add a profile. `auth` asks for the URL, login and password, and checks them before saving:

```sh
docker run -it --rm -v ymcp:/config xlsoftware/ymcp auth
docker run -it --rm -v ymcp:/config xlsoftware/ymcp --profile youtrack-beget auth   # another instance
```

Run the server with a bearer token:

```sh
docker run -d --name ymcp --restart unless-stopped \
  -p 127.0.0.1:8090:8090 -v ymcp:/config \
  -e YMCP_HTTP_TOKEN="$(openssl rand -hex 32)" \
  xlsoftware/ymcp
docker logs ymcp   # lists each profile and its URL
```

Each profile is served at `http://<host>:8090/<profile>`, and the default one at `/` too. Point a client at it with
the token, e.g. Claude Code:

```sh
claude mcp add --transport http youtrack http://127.0.0.1:8090/youtrack --header "Authorization: Bearer <token>"
```

For a single instance without a volume, pass the connection instead:
`-e YOUTRACK_URL=https://youtrack.example.com -e YOUTRACK_LOGIN=… -e YOUTRACK_PASSWORD=…`.

## Settings

| Variable | |
|---|---|
| `YMCP_HTTP_TOKEN` | Bearer token every request must carry. Without one, keep the port on loopback |
| `YMCP_TOOLS` | Tool groups to serve, e.g. `issues,comments,articles,-delete_issue` |
| `YOUTRACK_PROXY` | `none` to connect directly, or a proxy URL |
| `YMCP_LOG_LEVEL` | `debug` logs every YouTrack request |

Arguments after the image name replace the default command, `serve --transport http --addr 0.0.0.0:8090`, e.g.
`xlsoftware/ymcp serve --transport http --addr 0.0.0.0:8090 --profiles youtrack,youtrack-beget`.

The image has a `HEALTHCHECK` on `GET /healthz`, which answers without the token.

There is no keychain in a container, so passwords are kept in the `/config` volume's config file (mode 0600);
treat the volume as a secret.

## Tags and verification

`latest`, `<major>.<minor>` and `<version>`, for linux/amd64 and linux/arm64. Images are signed keylessly with
Sigstore by the repository's release workflow and carry SBOM and provenance attestations:

```sh
cosign verify xlsoftware/ymcp:latest \
  --certificate-identity-regexp '^https://github.com/xlsft/ymcp/\.github/workflows/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Documentation, the tool list and the native installer: [github.com/xlsft/ymcp](https://github.com/xlsft/ymcp).
MIT licensed.
