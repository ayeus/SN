# Builds the Linux host agent without a local Rust toolchain and writes the
# binary out of the build (see `make dist-agent-linux`). Context: repo root.
#
# Built against Debian bookworm's glibc (2.36), which covers Ubuntu 22.04+ hosts.

FROM rust:1-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler libprotobuf-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY VERSION ./VERSION
COPY proto ./proto
COPY agent ./agent
RUN --mount=type=cache,target=/usr/local/cargo/registry --mount=type=cache,target=/src/agent/target \
    cd agent && cargo build --release --locked && cp target/release/ayeusann-agent /ayeusann-agent

FROM scratch
ARG TARGETARCH
COPY --from=build /ayeusann-agent /ayeusann-agent-linux-${TARGETARCH}
