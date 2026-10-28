## What and why

<!-- One or two sentences. Link the issue if there is one. -->

## Checklist

- [ ] The title is a Conventional Commit: it becomes the squash commit, and `feat`, `fix` and `perf` release
- [ ] `go test -race ./...`, `go vet ./...` and `gofmt -l .` are clean
- [ ] New or changed tools are covered in `internal/tools/tools_test.go` and listed in the README
- [ ] Tried against a live YouTrack, or explained why not
