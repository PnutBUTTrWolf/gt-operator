# Operator image — runs the gt-operator binary and gt daemon with tmux shim.
FROM golang:1.24-bookworm AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build build-shim

# --- Runtime ---
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    tmux \
    && rm -rf /var/lib/apt/lists/*

# Install kubectl
RUN curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
    && install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl \
    && rm kubectl

# Install gt binary (built separately or pulled from release)
# This is a placeholder — in CI, copy from a gastown build artifact.
# COPY --from=gt-builder /build/gt /usr/local/bin/gt

# Tmux shim: placed ahead of real tmux in PATH
COPY --from=builder /build/bin/tmux-shim /usr/local/bin/tmux
ENV GT_REAL_TMUX=/usr/bin/tmux
ENV GT_RUNTIME=k8s

COPY --from=builder /build/bin/gt-operator /usr/local/bin/gt-operator

RUN mkdir -p /var/run/gt-operator

ENTRYPOINT ["gt-operator"]
