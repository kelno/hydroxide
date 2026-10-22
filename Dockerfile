FROM golang:latest AS build

ARG COMMIT=unknown

WORKDIR /opt/hydroxide
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN echo "$COMMIT" | tee COMMIT \
	&& CGO_ENABLED=0 go build ./cmd/hydroxide

FROM debian:trixie-slim

ARG VERSION=dev
LABEL org.opencontainers.image.version="$VERSION"

RUN apt-get update && apt-get install -y ca-certificates \
	&& rm -rf /var/lib/apt/lists/*

COPY --from=build /opt/hydroxide/hydroxide /usr/local/bin/hydroxide
COPY --from=build /opt/hydroxide/COMMIT /usr/local/share/hydroxide/COMMIT
RUN chmod +x /usr/local/bin/hydroxide

ENTRYPOINT ["/usr/local/bin/hydroxide"]
