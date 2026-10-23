FROM ubuntu:22.04

# E2E needs an HTTP client inside the node containers: the test-only sign-psbt endpoint binds
# 127.0.0.1 (the bridge exposes no inbound API), so the harness reaches it with `compose exec`.
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /root

COPY chain33 chain33
COPY chain33-cli chain33-cli
COPY *.toml ./

CMD /root/chain33 -f /root/$ChainConfig
