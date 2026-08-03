FROM golang:latest AS build

RUN git clone https://github.com/kelno/hydroxide /opt/hydroxide

WORKDIR /opt/hydroxide
RUN git rev-parse HEAD | tee COMMIT \
	&& GO111MODULE=on go build ./cmd/hydroxide

FROM debian:trixie-slim

RUN apt-get update && apt-get install -y ca-certificates \
	&& rm -rf /var/lib/apt/lists/*

COPY --from=build /opt/hydroxide/hydroxide /usr/local/bin/hydroxide
COPY --from=build /opt/hydroxide/COMMIT /usr/local/share/hydroxide/COMMIT
RUN chmod +x /usr/local/bin/hydroxide

ENTRYPOINT ["/usr/local/bin/hydroxide"]
