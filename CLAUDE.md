# hydroxide (kelno's fork)

This fork is AI-managed by its owner, kelno, under kelno's own rules, which don't apply to anyone else's agents.

Upstream is https://codeberg.org/emersion/hydroxide (remote `upstream`); the GitHub repo this fork was created from is archived.

## Build and checks

The only tests are in `imap/database`. Go may not be installed on the machine you work from; the Dockerfile's build stage (`golang:latest`) is a way to build, vet and test: `go vet ./... && go test ./... && go build ./...`, with `GOFLAGS=-buildvcs=false` when the source is mounted into a container. `go vet` already reports unkeyed struct literals in `exports/messages.go`, upstream code.

The fork's differences from upstream are listed at the top of `README.md`; a change that adds or ends one updates that list.
