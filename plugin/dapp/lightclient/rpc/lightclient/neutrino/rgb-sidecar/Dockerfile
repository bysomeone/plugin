# RGB 侧车（watch-only、单脚本 wpkh(TSS 公钥)）+ issue_usdt 助手。
# 在 Linux rust 镜像内构建（宿主 macOS 二进制无法进 Linux 容器）。
# 缓存策略：先拷 Cargo 清单 + 引用全部直接依赖的占位 lib → 预编译依赖层（Docker 层缓存复用），
# 再拷全量源码增量编译，CI 重跑只重编侧车 crate 本身（10-30min → 秒级）。
ARG DEBIAN_MIRROR=mirrors.aliyun.com

FROM rust:1.98-bookworm AS builder
ARG DEBIAN_MIRROR
RUN set -eux; \
    sed -i "s|deb.debian.org|${DEBIAN_MIRROR}|g" /etc/apt/sources.list.d/debian.sources; \
    for i in 1 2 3 4 5 6 7 8; do \
        if apt-get update && apt-get install -y --no-install-recommends protobuf-compiler; then break; fi; \
        echo "apt retry $i"; sleep 15; \
    done; \
    apt-get update && apt-get install -y --no-install-recommends protobuf-compiler; \
    rm -rf /var/lib/apt/lists/*
WORKDIR /src
# ---- 依赖层：只拷 Cargo 清单 + build.rs + proto，占位 lib 引用全部直接依赖 ----
# 这里只编 lib 目标（--lib）：本层还没有 src/main.rs 与 src/bin/*（bins 的源码下一层才拷进来），
# 而 Cargo.toml 没有 [[bin]] 段（bin 靠 src/ 自动发现），拿 `--bin rgb-sidecar` 去解析目标只会
# 得到 "no bin target" 并在编译前退出 —— 加上 `|| true` 就变成"这一层什么都不编、每轮从零编全部
# 依赖"。lib 目标把 [dependencies] 整份编掉，正是本层要的东西（bins 用的 crate 也在同一份依赖表
# 里，一并覆盖）。
# 注意占位 lib 引用的是各依赖的 **lib 名**，不是包名：rgb-ops → rgbstd、rgb-consensus → rgbcore、
# rgb-schemas → schemata、rgb-invoicing → rgbinvoice、rgb-psbt-utils → psrgbt、
# rgb-strict-encoding → strict_encoding、rgb-strict-types → strict_types（见 Cargo.toml 里的注释）。
COPY Cargo.toml Cargo.lock build.rs ./
COPY proto ./proto
RUN mkdir -p src && cat > src/lib.rs <<'EOF'
pub use bdk_wallet;
pub use bdk_electrum;
pub use rgbstd;
pub use rgbcore;
pub use schemata;
pub use rgbinvoice;
pub use psrgbt;
pub use strict_encoding;
pub use strict_types;
pub use serde;
pub use serde_json;
pub use bitcoin;
pub use anyhow;
pub use amplify;
pub use tonic;
pub use tonic_prost;
pub use prost;
pub use tokio;
pub use tokio_stream;
pub use tower;
pub use rand;
pub use electrum_client;
pub use bdk_chain;
pub use ureq;
pub use rustls;
pub use base64;
EOF
RUN cargo build --release --lib
# ---- 源码层：拷全量源码，增量编译 ----
COPY src ./src
RUN touch build.rs && cargo build --release --bin rgb-sidecar --bin issue_usdt

FROM ubuntu:22.04
WORKDIR /sidecar
COPY --from=builder /src/target/release/rgb-sidecar /sidecar/rgb-sidecar
COPY --from=builder /src/target/release/issue_usdt /sidecar/issue_usdt
ENV RGB_SIDECAR_NETWORK=regtest \
    RGB_SIDECAR_LISTEN=0.0.0.0:50061 \
    RGB_SIDECAR_TEST_LISTEN=0.0.0.0:50064
EXPOSE 50061 50064
CMD ["/sidecar/rgb-sidecar"]
