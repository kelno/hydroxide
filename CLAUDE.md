# hydroxide (kelno's fork)

This fork is AI-managed by its owner, kelno, under kelno's own rules, which don't apply to anyone else's agents.

Upstream is https://codeberg.org/emersion/hydroxide (remote `upstream`); the GitHub repo this fork was created from is archived.

## Build and checks

There are no tests in the repo. Go may not be installed on the machine you work from; the Dockerfile's build stage (`golang:latest`) is a way to build and vet: `go vet ./... && go build ./...`, with `GOFLAGS=-buildvcs=false` when the source is mounted into a container.
