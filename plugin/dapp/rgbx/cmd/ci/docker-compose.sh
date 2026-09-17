#!/usr/bin/env bash

set -e
set -o pipefail

ROOT_DIR=$(cd "$(dirname "$0")" && pwd)
export PATH="${ROOT_DIR}:${PATH}"

# shellcheck disable=SC1091
source "${ROOT_DIR}/scripts/assertions.sh"
# shellcheck disable=SC1091
source "${ROOT_DIR}/scripts/bootstrap.sh"

ACTION="run"
PROJECT=""
if [ "$#" -gt 0 ]; then
    case "${1}" in
    run | up | down | reset | init | config | test)
        ACTION="${1}"
        PROJECT="${2:-rgbx-ci}"
        ;;
    *)
        PROJECT="${1}"
        ;;
    esac
fi
if [ -z "${PROJECT}" ]; then
    PROJECT="rgbx-ci"
fi

export COMPOSE_PROJECT_NAME="${PROJECT}"

# Phase 5：RGB20 服务/override 独立 compose 文件，与基础文件合并。
# 基础 docker-compose.yml 保持纯净；main healthcheck + para 等待 main healthy + RGB 链服务
# 都放在 docker-compose-rgb20.yml。这样基础文件改动最小，避免与其他分支冲突。
export COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml:docker-compose-rgb20.yml}"

if command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_BIN="docker-compose"
elif docker compose version >/dev/null 2>&1; then
    COMPOSE_BIN="docker compose"
else
    fail "docker compose is required"
fi

function compose_cmd() {
    ${COMPOSE_BIN} "$@"
}

MAIN_CLI="compose_cmd exec -T main /root/chain33-cli --conf=chain33.test.toml"
PARA1_CLI="compose_cmd exec -T para1 /root/chain33-cli --conf=chain33.para1.toml --paraName=user.p.rgbx."
PARA2_CLI="compose_cmd exec -T para2 /root/chain33-cli --conf=chain33.para2.toml --paraName=user.p.rgbx."
PARA3_CLI="compose_cmd exec -T para3 /root/chain33-cli --conf=chain33.para3.toml --paraName=user.p.rgbx."
PARA4_CLI="compose_cmd exec -T para4 /root/chain33-cli --conf=chain33.para4.toml --paraName=user.p.rgbx."
BTCD_RPC_USER="${BTCD_RPC_USER:-root}"
BTCD_RPC_PASS="${BTCD_RPC_PASS:-1314}"
BTC_CTL="compose_cmd exec -T btcd /usr/local/bin/btcctl --configfile=/tmp/btcctl.conf --rpcserver=127.0.0.1:18443 --rpcuser=${BTCD_RPC_USER} --rpcpass=${BTCD_RPC_PASS}"

BTC_NETWORK="${BTC_NETWORK:-regtest}"
# 单 btcd 收敛（Phase 5 收尾）：BTC 桥 + RGB 链共用同一个 btcd 全节点（BIP157 committed
# filters，neutrino 能同步头/过滤器；bitcoind 不服务 BIP157 是原 SPV EOF 根因）。para 的
# neutrino connectPeers 指 btcd:18444（P2P），btcRPC 指 btcd:18443（TLS + /btcd/rpc.cert）。
BTC_P2P_ADDR="${BTC_P2P_ADDR:-btcd:18444}"
BTC_RPC_ADDR="${BTC_RPC_ADDR:-btcd:18443}"
HOST_BTC_RPC_ADDR="${HOST_BTC_RPC_ADDR:-127.0.0.1:18443}"
PARA_TITLE="${PARA_TITLE:-user.p.rgbx.}"
TSS_THRESHOLD="${TSS_THRESHOLD:-3}"
AUTO_DISCOVER_TSS_PEERS="${AUTO_DISCOVER_TSS_PEERS:-true}"

# 14KEKbYtKKQm4wMthSK9J4La4nAiidGozt
GENESIS_KEY="CC38546E9E659D15E6B4893F0AB32A06D103931A8230B0BDE71459D2B27D6944"

# 4 para validators: one official TSS node + three validating nodes.
AUTH_ADDR1="1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4"
AUTH_ADDR2="1JRNjdEqp4LJ5fqycUBm9ayCKSeeskgMKR"
AUTH_ADDR3="1NLHPEcbTWWxxU3dGUZBhayjrCHD3psX7k"
AUTH_ADDR4="1MCftFynyvG2F4ED5mdHYgziDxx6vDrScs"
AUTH_KEY1="0x6da92a632ab7deb67d38c0f6560bcfed28167998f6496db64c258d5e8393a81b"
AUTH_KEY2="0x19c069234f9d3e61135fefbeb7791b149cdf6af536f26bebb310d4cd22c3fee4"
AUTH_KEY3="0x7a80a1f75d7360c6123c32a78ecf978c1ac55636f87892df38d8b85a9aeff115"
AUTH_KEY4="0xcacb1f5d51700aea07fca2246ab43b0917d70405c65edea9b5063d72eb5c6b71"
TSS_PEERS="${TSS_PEERS:-${AUTH_ADDR1},${AUTH_ADDR2},${AUTH_ADDR3},${AUTH_ADDR4}}"

MINT_SYMBOL="${MINT_SYMBOL:-BTC}"
WITHDRAW_DEST_ADDR="${WITHDRAW_DEST_ADDR:-bcrt1qnnwpfpljh5n8m3a8xtf3x5ayvhjjplxmhuexyh}"
BTC_FUNDING_PRIV_HEX="${BTC_FUNDING_PRIV_HEX:-0000000000000000000000000000000000000000000000000000000000000001}"
BTC_DEPOSIT_AMOUNT_SATS="${BTC_DEPOSIT_AMOUNT_SATS:-20000000}"
BTC_WITHDRAW_AMOUNT_SATS="${BTC_WITHDRAW_AMOUNT_SATS:-500000}"

# ===== RGB20 (Phase 5) =====
# RGB20 链全走 compose 内部网络：rgb-bitcoind(rgb 链) + rgb-electrs(索引) + rgb-sidecar(gRPC 50061 + test-sim 50064)。
# 侧车镜像在 Docker 内构建（源码目录里的 Dockerfile，宿主 macOS 二进制无法进 Linux 容器）。
RGB20_SYMBOL="${RGB20_SYMBOL:-RGB20_USDT}"
RGB20_SIDECAR_SYMBOL="${RGB20_SIDECAR_SYMBOL:-USDT}"
RGB20_SIDECAR_ADDR="${RGB20_SIDECAR_ADDR:-rgb-sidecar:50061}"
RGB20_SIDECAR_TEST_ADDR="${RGB20_SIDECAR_TEST_ADDR:-127.0.0.1:50064}"
RGB20_PRECISION="${RGB20_PRECISION:-6}"
# RGB 链收敛到单 btcd：sidecar/issue_usdt/test-sim 的 RPC 指向 btcd（TLS，凭据 root/1314，
# cert=/btcd/rpc.cert）。RGB_BITCOIND_RPC 为 host:port（无 scheme），scheme 由 cert 是否设置决定。
RGB20_BITCOIND_RPC="${RGB20_BITCOIND_RPC:-btcd:18443}"
RGB20_BITCOIND_USER="${RGB20_BITCOIND_USER:-${BTCD_RPC_USER:-root}}"
RGB20_BITCOIND_PASS="${RGB20_BITCOIND_PASS:-${BTCD_RPC_PASS:-1314}}"
RGB20_BITCOIND_CERT="${RGB20_BITCOIND_CERT:-/btcd/rpc.cert}"
RGB20_DEPOSIT_AMOUNT="${RGB20_DEPOSIT_AMOUNT:-1000000}"   # 1.0 USDT (min units)
RGB20_WITHDRAW_AMOUNT="${RGB20_WITHDRAW_AMOUNT:-500000}"  # 0.5 USDT
# 两连提现场景每笔金额（0.2 USDT）：两笔合计不超过上一场景提现后的剩余额（默认 0.5），
# 第二笔必须花掉第一笔的 change seal。
RGB20_TWO_WITHDRAW_AMOUNT="${RGB20_TWO_WITHDRAW_AMOUNT:-200000}"
# 易截断金额的提现（60000 min units = 0.0006 的 -a 口径）：1..2,000,000 最小单位中有 7.99%
# 在旧的浮点换算（int64(float64 相乘)）下会少 1（0.0006*1e8 = 59999.99999999999 -> 59999）。
# 其余场景的金额（500000/200000）恰好精确，覆盖不到这个雷区。
RGB20_TRUNCATION_WITHDRAW_AMOUNT="${RGB20_TRUNCATION_WITHDRAW_AMOUNT:-60000}"
BTC_WITHDRAW_FEE_RATE="${BTC_WITHDRAW_FEE_RATE:-20}"
XBTC_TRANSFER_AMOUNT="${XBTC_TRANSFER_AMOUNT:-500000}"

BTCD_RPC_CERT_IN_CONTAINER="${BTCD_RPC_CERT_IN_CONTAINER:-/btcd/rpc.cert}"
USER_B_KEY="${USER_B_KEY:-${AUTH_KEY2}}"
USER_B_ADDR="${USER_B_ADDR:-${AUTH_ADDR2}}"
# Fixed regtest mining identity (privHex=...0001 -> mrCDr...).
BTCD_MINING_ADDR="mrCDrCybB6J1vRfbwM5hemdJz73FwDBC8r"
BTC_FUNDING_WIF="${BTC_FUNDING_WIF:-}"
USER_MAIN_ADDR="${USER_MAIN_ADDR:-14KEKbYtKKQm4wMthSK9J4La4nAiidGozt}"

function join_csv_as_toml_array() {
    local csv="${1}"
    local out=""
    IFS=',' read -r -a items <<<"${csv}"
    local i
    for ((i = 0; i < ${#items[@]}; i++)); do
        local item
        item=$(echo "${items[$i]}" | xargs)
        if [ -n "${item}" ]; then
            if [ -n "${out}" ]; then
                out="${out},"
            fi
            out="${out}\"${item}\""
        fi
    done
    echo "[${out}]"
}

function config_main() {
    log_step "configure chain33.test.toml"
    local main_cfg="${ROOT_DIR}/chain33.test.toml"
    if [ -f "${main_cfg}" ]; then
        chmod u+w "${main_cfg}" 2>/dev/null || true
    fi
    perl -i -pe 's/^Title=.*/Title="local"/' "${main_cfg}"
    perl -i -pe 's/^TestNet=.*/TestNet=true/' "${main_cfg}"
    perl -i -pe 's/^jrpcBindAddr=.*/jrpcBindAddr="0.0.0.0:8801"/' "${main_cfg}"
    perl -i -pe 's/^grpcBindAddr=.*/grpcBindAddr="0.0.0.0:8802"/' "${main_cfg}"
    perl -i -pe 's/^whitelist=.*/whitelist=["*"]/' "${main_cfg}"
    perl -i -pe 's/^isLevelFee=.*/isLevelFee=false/' "${main_cfg}"

    if ! grep -q '^\[exec.sub.rgbx\]' "${main_cfg}" 2>/dev/null; then
        cat >>"${main_cfg}" <<EOF

[exec.sub.lightclient]
btcNetName="regtest"
commitAddress="${AUTH_ADDR1}"
allowRegtestTimeWarp=true

[exec.sub.rgbx]
commitAddress="${AUTH_ADDR1}"
crossChainAssetPrefix="X"
guardianParachainTitle="${PARA_TITLE}"
# CI 显式用 1：本 harness 的 BTC 确认数整体是 1（blockConfirmations=1），E2E 依赖"1 个后续块即确认"
# 的时序；而被屏蔽的 scenario_user_deposit_via_btc_tx 等待循环不再挖块，一旦重新启用，生产默认值 6
# 会让它等不到确认而卡死。生产/主网不要照抄这个值（默认 6）。
minBtcConfirmations=1
EOF
    fi
}

function config_para_file() {
    local file="$1"
    local auth_addr="$2"
    local rank="$3"
    local official="$4"
    local path="${ROOT_DIR}/${file}"

    if [ -f "${path}" ]; then
        chmod u+w "${path}" 2>/dev/null || true
    fi
    cp "${ROOT_DIR}/chain33.para.toml" "${path}"
    chmod u+w "${path}" 2>/dev/null || true
    perl -i -pe 's/^Title=.*/Title="'"${PARA_TITLE}"'"/' "${path}"
    perl -i -pe 's/^TestNet=.*/TestNet=true/' "${path}"
    perl -i -pe 's/^mainChainGrpcAddr=.*/mainChainGrpcAddr="main:8802"/' "${path}"
    perl -i -pe 's/^jrpcBindAddr=.*/jrpcBindAddr="0.0.0.0:8801"/' "${path}"
    perl -i -pe 's/^authAccount=.*/authAccount="'"${auth_addr}"'"/' "${path}"
    perl -i -pe 's/^startHeight=.*/startHeight=1/' "${path}"
    perl -i -pe 's/^enableTSS=.*/enableTSS=true/' "${path}"
    if ! grep -q '^enableTSS=' "${path}" 2>/dev/null; then
        perl -i -0pe 's/\[crypto\]\n/[crypto]\nenableTSS=true\n/' "${path}"
    fi
    # Phase 5 DKG 修复：para 链 paracross fork 高度与主链(local)对齐。
    # 主链 local 下 ForkCommitTx/ForkLoopCheckCommitTxDone 都被特殊处理为 1；
    # para 链若用默认 2270000/4320000，nodegroup apply 无条件写带前缀 key 而 approve
    # 在 fork 未激活时用 raw id 查询，key 不一致导致 para 链 nodegroup 建立失败（getNodegGroupId ErrNotFound）。
    perl -i -pe 's/^mainForkParacrossCommitTx=.*/mainForkParacrossCommitTx=1/' "${path}"
    perl -i -pe 's/^mainLoopCheckCommitTxDoneForkHeight=.*/mainLoopCheckCommitTxDoneForkHeight=1/' "${path}"
    # Phase 5 DKG 修复：RGB20 CI 屏蔽旧 BTC 跨链用例后，主链没有持续的 para 交易，
    # para 链若按默认空块间隔(0:50)推进，会长期停在 nodegroup approve 高度导致 blockSync 追不上主链
    # （syncCaughtUp=false → para is not Sync）。这里收紧到每 1 个主链区块产出一个空 para 区块，让 para 快速追上主链。
    perl -i -pe 's/^emptyBlockInterval=.*/emptyBlockInterval=["0:1"]/' "${path}"
    # Para nodes must enable DHT before peer discovery and TSS peer collection.
    perl -i -pe 's/^types=.*/types=["dht"]/' "${path}"
    perl -i -pe 's/^enable=.*/enable=true/' "${path}"
    perl -i -pe 's/^waitPid=.*/waitPid=false/' "${path}"

    local toml_peers
    toml_peers=$(join_csv_as_toml_array "${TSS_PEERS}")
    cat >>"${path}" <<EOF

[rpc.sub.light]
clients=["neutrino"]
commitAddr="${auth_addr}"

[rpc.sub.light.neutrino]
isOfficialNode=${official}
netName="${BTC_NETWORK}"
connectPeers=["${BTC_P2P_ADDR}"]
btcBlockInterval=2
blockConfirmations=1
maxUtxoRescanTime=60

[rpc.sub.light.neutrino.btcRPC]
host="${BTC_RPC_ADDR}"
user="${BTCD_RPC_USER}"
pass="${BTCD_RPC_PASS}"
mode="http"
disableTLS=false
certFile="${BTCD_RPC_CERT_IN_CONTAINER}"

[rpc.sub.light.neutrino.tss]
peers=${toml_peers}
threshold=${TSS_THRESHOLD}
rank=${rank}

[rpc.sub.light.neutrino.rgb20]
sidecarAddr="${RGB20_SIDECAR_ADDR}"
consignmentListen="0.0.0.0:17000"
precision=${RGB20_PRECISION}
# testSignPsbt 打开 E2E 的 sign-psbt 测试端点（用 TSS 组签任意 PSBT，模拟用户付款）。
# 该能力等于"用组私钥签任意内容"，签名节点无从核对 ⇒ 生产环境必须为 false（默认关闭），
# 这里只为 E2E/regtest 打开。
testSignPsbt=true

[[rpc.sub.light.neutrino.rgb20.contracts]]
symbol="${RGB20_SYMBOL}"
sidecarSymbol="${RGB20_SIDECAR_SYMBOL}"
precision=${RGB20_PRECISION}
EOF
}

function init_env() {
    log_step "init realistic topology config (main + 4 para + btcd)"
    config_main
    config_para_file chain33.para1.toml "${AUTH_ADDR1}" 0 true
    config_para_file chain33.para2.toml "${AUTH_ADDR2}" 1 false
    config_para_file chain33.para3.toml "${AUTH_ADDR3}" 1 false
    config_para_file chain33.para4.toml "${AUTH_ADDR4}" 1 false
}

function btcd_container_id() {
    # 无容器时输出为空；查询本身失败也不应让调用方（set -e/pipefail）中断。
    compose_cmd ps -aq btcd 2>/dev/null | head -1 || true
}

# btcd regtest 链的当前 tip（"<height>:<bestblockhash>"）。btcd 没在跑/查询失败时返回空，
# 调用方据此跳过比对（探测失败不应阻断流程）。
function btcd_chain_tip() {
    local height hash
    height=$(${BTC_CTL} --"${BTC_NETWORK}" getblockcount 2>/dev/null) || return 0
    hash=$(${BTC_CTL} --"${BTC_NETWORK}" getbestblockhash 2>/dev/null) || return 0
    if [ -n "${height}" ] && [ -n "${hash}" ]; then
        echo "${height}:${hash}"
    fi
    return 0
}

function chain33_state_volume_preexists() {
    # 只回答"此刻数据卷是否已存在"。必须在 do_up_only 里、任何 compose 命令之前取快照
    # （compose run main 那一步会顺带把 main-data 卷创建出来），否则干净环境上会误报。
    if [ -n "$(docker volume ls -q --filter "name=^${COMPOSE_PROJECT_NAME}_main-data$" 2>/dev/null)" ]; then
        echo "true"
    else
        echo "false"
    fi
}

# =====================================================================
# start_env：**只 up，不 down** —— btcd 的 regtest 链必须跨 run 存活
# =====================================================================
# 为什么不能再 `down` 全体后 `up`（这是本 harness 的核心约束，别"优化"回去）：
#
#   btcd v0.24.2 在 --regtest 下**每次进程启动都无条件删除区块库**，源码注释即
#   "The regression test is special in that it needs a clean database for each run"
#   （btcd/btcd.go）：
#       func removeRegressionDB(dbPath string) error {
#           if !cfg.RegressionTest { return nil }      // 唯一的门，没有任何 datadir 保护
#           ... os.RemoveAll(dbPath) ...
#       }
#       func loadBlockDB() { ...; removeRegressionDB(blockDbPath(cfg.DbType)); ... }
#   而 dbPath = <datadir>/<net>/blocks_ffldb（config.go 里 cfg.DataDir 末尾已拼上网络名
#   netName(activeNetParams)），所以换 --datadir 也躲不掉 —— docker-compose.yml 里那条
#   --datadir 注释曾经得出的"可原位恢复"结论是错的。
#
#   后果：btcd 一旦被**重建或重启**（进程重新执行 entrypoint），链就回到 genesis；而 chain33
#   的 lightclient 已把**旧链**的 BTC 头提交上链，头链接不上新链 → ErrBtcHeaderDisorder /
#   "invalid btc proof block info" → 充值 SPV 永久失败，且无法原地恢复（只能 reset 重跑
#   DKG，很慢）。这正是"run 第一遍 OK、第二遍必废"的根因。
#
#   所以：`run` 能连续跑的前提是 **btcd 容器/进程不被动**；需要全新环境时用 reset（down -v），
#   那是 reset 的职责，语义不变。
#
# 只 up 的语义（compose 只重建"配置/镜像变了"的服务）：
#   - chain33 各服务（main/para1-4/rgb-sidecar）改了镜像或 toml 时照旧重建，不会跑旧二进制；
#   - btcd 配置/镜像没变时 compose 判定无需重建 → 容器保持运行、regtest 链不丢。
# 但"btcd 变了"本身是致命的，故这里显式探测（容器被重建 / 被原地重启 / 链 tip 变了）并**直接
# 报错要求 reset**，而不是静默沿用旧容器、静默跑到 SPV 上才炸。
function start_env() {
    log_step "start docker-compose services (in-place up: btcd container must stay untouched)"

    local btcd_cid_before="" btcd_status_before="" btcd_tip_before=""
    btcd_cid_before=$(btcd_container_id)
    if [ -n "${btcd_cid_before}" ]; then
        btcd_status_before=$(docker inspect -f '{{.State.Status}}' "${btcd_cid_before}" 2>/dev/null || true)
        if [ "${btcd_status_before}" = "running" ]; then
            btcd_tip_before=$(btcd_chain_tip)
            log_step "existing btcd container ${btcd_cid_before:0:12} is running (tip=${btcd_tip_before:-unknown}); preserving it"
        else
            log_step "existing btcd container ${btcd_cid_before:0:12} is '${btcd_status_before}'"
        fi
    elif [ "${CHAIN33_VOLUME_PREEXISTED:-false}" = "true" ]; then
        # 容器没了但 chain33 链数据还在（例如有人先 `down` 再 `run`）：btcd 会以新容器从
        # genesis 起链，而 chain33 若已提交过旧链的 BTC 头就再也接不上。无法在这里断定
        # chain33 是否已有 BTC 头，故不直接失败，但把话说在前面（真要干净环境请 reset）。
        log_step "WARNING: no btcd container while chain33 volume ${COMPOSE_PROJECT_NAME}_main-data still exists."
        log_step "WARNING: a fresh btcd chain will be created; if chain33 already committed BTC headers from the previous chain, deposit SPV will fail. Prefer './docker-compose.sh reset' for a clean environment."
    fi

    # 只 up（不 down）：见上方注释，btcd --regtest 每次启动清链，down + up 必废头链。
    compose_cmd up --build -d --remove-orphans
    sleep 8
    compose_cmd ps

    local btcd_cid_after=""
    btcd_cid_after=$(btcd_container_id)
    assert_non_empty "${btcd_cid_after}" "btcd container missing after compose up"

    if [ -n "${btcd_cid_before}" ] && [ "${btcd_cid_before}" != "${btcd_cid_after}" ]; then
        fail "btcd container was RECREATED (${btcd_cid_before:0:12} -> ${btcd_cid_after:0:12}) because its config/image changed. btcd v0.24.2 wipes the regtest chain on every start, so the BTC header chain chain33 already committed is stale and deposits would fail with ErrBtcHeaderDisorder. Run './docker-compose.sh reset' for a clean environment."
    fi
    if [ -n "${btcd_cid_before}" ] && [ "${btcd_status_before}" != "running" ]; then
        fail "btcd container ${btcd_cid_before:0:12} existed but was '${btcd_status_before}'; starting it re-runs btcd (which wipes the regtest chain) while chain33 still holds the old chain's BTC headers. Run './docker-compose.sh reset' for a clean environment."
    fi
    if [ -n "${btcd_tip_before}" ]; then
        local btcd_tip_after
        btcd_tip_after=$(btcd_chain_tip)
        if [ -n "${btcd_tip_after}" ] && [ "${btcd_tip_after}" != "${btcd_tip_before}" ]; then
            fail "btcd regtest chain changed across this start (${btcd_tip_before} -> ${btcd_tip_after}): the chain was reset/restarted under us, so chain33's committed BTC headers are stale. Run './docker-compose.sh reset' for a clean environment."
        fi
    fi
}

function wait_cli_ready() {
    local cli="$1"
    local retries=120
    local i
    for ((i = 0; i < retries; i++)); do
        if ${cli} block last_header >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    fail "cli not ready: ${cli}"
}

function wait_btcd_ready() {
    log_step "wait btcd ready"
    # Ensure btcd home/config paths exist under bind-mounted volume.
    compose_cmd exec -T btcd sh -c 'mkdir -p /root/.btcd /tmp && [ -f /root/.btcd/btcd.conf ] || : > /root/.btcd/btcd.conf' >/dev/null 2>&1 || true
    local retries=10
    local i
    local last_err=""
    for ((i = 0; i < retries; i++)); do
        local out
        if out=$(${BTC_CTL} --"${BTC_NETWORK}" getblockcount 2>&1); then
            return 0
        fi
        last_err="${out}"
        sleep 1
    done
    if [ -n "${last_err}" ]; then
        log_step "btcd probe last error: ${last_err}"
    fi
    compose_cmd ps btcd || true
    compose_cmd logs --tail=120 btcd || true
    fail "btcd not ready"
}

function block_wait() {
    local cli="$1"
    local delta="$2"
    local cur
    local expect
    cur=$(${cli} block last_header | jq ".height")
    expect=$((cur + delta))
    local count=300
    while [ "${count}" -gt 0 ]; do
        local now
        now=$(${cli} block last_header | jq ".height")
        if [ "${now}" -ge "${expect}" ]; then
            return 0
        fi
        count=$((count - 1))
        sleep 0.2
    done
    fail "wait block timeout for ${cli}, expect=${expect}"
}

function tx_wait() {
    local cli="$1"
    local tx_hash="$2"
    local retries=20
    local i
    for ((i = 0; i < retries; i++)); do
        if ${cli} tx query_hash -s "${tx_hash}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.5
    done
    fail "tx not found: ${tx_hash}"
}

function prepare_btcd_mining_identity() {
    local raw_output
    local key_info
    raw_output=$(compose_cmd run --rm --no-deps -T main /root/chain33-cli rgbx btcKeyInfo --net "${BTC_NETWORK}" --privHex "${BTC_FUNDING_PRIV_HEX}")
    # docker compose may print progress lines before command output; keep JSON payload only.
    key_info=$(echo "${raw_output}" | awk 'BEGIN{keep=0} /^\{/ {keep=1} keep==1 {print}')
    assert_non_empty "${key_info}" "btcKeyInfo json output empty"
    BTC_FUNDING_WIF=$(echo "${key_info}" | jq -r '.wif')
    local derived_addr
    derived_addr=$(echo "${key_info}" | jq -r '.address')
    assert_non_empty "${BTC_FUNDING_WIF}" "btc funding wif empty"
    assert_non_empty "${derived_addr}" "btcd mining address empty"
    assert_eq "${derived_addr}" "${BTCD_MINING_ADDR}" "btc funding key does not match fixed BTCD_MINING_ADDR"
    log_step "prepared btcd mining identity address=${BTCD_MINING_ADDR}"
}

function get_main_addr_by_label() {
    local label="$1"
    ${MAIN_CLI} account list | jq -r --arg l "${label}" '[.[]? | select(.label == $l) | .addr][0] // empty'
}

function query_xbtc_balance() {
    local addr="$1"
    ${MAIN_CLI} asset balance -a "${addr}" --asset_exec=rgbx --asset_symbol=XBTC | jq -r '.balance // "0"'
}

function wait_xbtc_balance_not_less_than() {
    local addr="$1"
    local expected="$2"
    local retries="${3:-30}"
    local i
    for ((i = 0; i < retries; i++)); do
        local balance
        balance=$(query_xbtc_balance "${addr}")
        if awk "BEGIN{exit !(${balance} >= ${expected})}"; then
            return 0
        fi
        sleep 1
    done
    fail "xbtc balance not reached, addr=${addr}, expected>=${expected}"
}

function mine_btcd_blocks() {
    local count="$1"
    ${BTC_CTL} --"${BTC_NETWORK}" generate "${count}" >/dev/null
}

function build_mature_coinbase_utxo() {
    assert_non_empty "${BTCD_MINING_ADDR}" "BTCD_MINING_ADDR empty"
    local best_height
    best_height=$(${BTC_CTL} --"${BTC_NETWORK}" getblockcount)
    local mature_height=$((best_height - 100))
    if [ "${mature_height}" -lt 1 ]; then
        fail "no mature coinbase yet: bestHeight=${best_height}"
    fi

    local block_hash
    block_hash=$(${BTC_CTL} --"${BTC_NETWORK}" getblockhash "${mature_height}")
    local coinbase_tx
    coinbase_tx=$(${BTC_CTL} --"${BTC_NETWORK}" getblock "${block_hash}" 1 | jq -r '.tx[0] // empty')
    assert_non_empty "${coinbase_tx}" "coinbase tx empty at height=${mature_height}"
    local tx_json
    tx_json=$(${BTC_CTL} --"${BTC_NETWORK}" getrawtransaction "${coinbase_tx}" 1)
    local vout
    vout=$(echo "${tx_json}" | jq -r --arg a "${BTCD_MINING_ADDR}" '.vout[] | select(.scriptPubKey.addresses[]? == $a) | .n' | head -1)
    assert_non_empty "${vout}" "coinbase vout not found for mining address"
    local amount_sats
    amount_sats=$(echo "${tx_json}" | jq -r --argjson v "${vout}" '.vout[] | select(.n == $v) | (.value * 100000000 | floor)')
    local pk_script
    pk_script=$(echo "${tx_json}" | jq -r --argjson v "${vout}" '.vout[] | select(.n == $v) | .scriptPubKey.hex')
    assert_non_empty "${amount_sats}" "coinbase amount sats empty"
    assert_non_empty "${pk_script}" "coinbase pkScript empty"
    echo "${coinbase_tx}:${vout}:${amount_sats}:${pk_script}"
}

# wait_no_withdraw_pending_for_user <from_addr> [tx_hash]
#
# 无 tx_hash：等该地址的提现 pending 全部清空（旧行为，BTC 场景用）。
# 有 tx_hash：等"本轮发出的这一笔"（hex，带 0x 前缀）走完 —— 先等它真的出现在 pending 列表里
# （提现交易落块后 chain33 才登记 pending；不先等它出现的话，紧接着的一次查询会"看不到"它而
# 被误判为已完成，后面的断言就在提现还没被桥处理时跑），再等它从列表里消失（= 桥提交的
# rgbx Confirm 已执行，这是"这笔提现真的走完了"的判据）。
#
# 为什么要按笔判定：chain33 对提现**没有**取消/超时路径（checktx 明确拒绝 Timeout=true 的
# withdraw confirm：ErrWithdrawConfirmTimeoutNotAllowed），上一轮失败遗留的 pending 会永远留在
# 列表里。而每轮 run_rgb20_env 都会重建侧车账本、重新发行资产（见该函数注释：不重发的话桥的
# 可支配余额会随付款耗尽），资产 id 一变，老 pending 里的 invoice 就永远不可能被这一版资产满足
# ——把它算在本轮头上会让环境"一次失败、此后每个 run 必失败"。老 pending 由桥侧停在那里并报
# 明确错误（lightclient neutrino：UNRECOVERABLE 日志 + 落盘状态），本轮只对本轮这一笔负责。
function wait_no_withdraw_pending_for_user() {
    local from_addr="$1"
    local tx_hash="${2:-}"
    local retries=45
    local i
    local cnt=0
    local seen=0
    # pending 列表里的 txHash 与 CLI 返回的提现哈希是同一个 0x 十六进制串（chain33 的 CLI JSON
    # 把 bytes 渲染成 hex），统一小写后直接比。
    local want_hash=""
    if [ -n "${tx_hash}" ]; then
        want_hash=$(echo "${tx_hash}" | tr 'A-Z' 'a-z')
    fi
    for ((i = 0; i < retries; i++)); do
        local pend_hashes
        pend_hashes=$(${MAIN_CLI} rgbx listPendingTxByFrom -f "${from_addr}" |
            jq -r '[.pendingList[]? | select(.actionType == 106) | .txHash | ascii_downcase]')
        cnt=$(echo "${pend_hashes}" | jq 'length')
        if [ -n "${want_hash}" ]; then
            if echo "${pend_hashes}" | jq -e --arg h "${want_hash}" 'index($h) != null' >/dev/null; then
                seen=1
            elif [ "${seen}" -eq 1 ]; then
                if [ "${cnt}" -gt 0 ]; then
                    log_step "  (this run's withdrawal confirmed; ${cnt} pending(s) left by earlier runs stay put)"
                fi
                return 0
            fi
        elif [ "${cnt}" -eq 0 ]; then
            return 0
        fi
        mine_btcd_blocks 1
        sleep 2
    done
    fail "withdraw pending not cleared for ${from_addr}， txHash=${tx_hash:-any}， cnt=${cnt}， seen=${seen}"
}

function query_latest_received_sats() {
    local addr="$1"
    local txs
    if ! txs=$(${BTC_CTL} --"${BTC_NETWORK}" searchrawtransactions "${addr}" 1 0 1 0 true 2>/dev/null); then
        echo 0
        return 0
    fi
    echo "${txs}" | jq -r --arg addr "${addr}" '
        [
            .[0]?.vout[]?
            | select((((.scriptPubKey.addresses? // []) | index($addr)) != null) or (.scriptPubKey.address? == $addr))
            | (.value * 100000000 | floor)
        ] | add // 0'
}

function wait_btc_address_received_not_less_than() {
    local addr="$1"
    local expected_sats="$2"
    local retries=3
    local i
    for ((i = 0; i < retries; i++)); do
        local received_sats
        received_sats=$(query_btc_address_received_sats "${addr}")
        if awk "BEGIN{exit !(${received_sats} >= ${expected_sats})}"; then
            log_step "btc address received: addr=${addr}, sats=${received_sats}"
            return 0
        fi
        mine_btcd_blocks 1
        sleep 1
    done
    fail "btc withdraw destination balance not received, addr=${addr}, expect>=${expected_sats}"
}

function prepare_accounts() {
    log_step "prepare wallets and auth keys"

    save_seed_and_unlock "${MAIN_CLI}"
    import_default_keys "${MAIN_CLI}"
    ${MAIN_CLI} account import_key -k "${USER_B_KEY}" -l rgbxUserB >/dev/null || true
    ${MAIN_CLI} send coins transfer -t "${USER_B_ADDR}" -a 100 -k "${GENESIS_KEY}" >/dev/null

    prepare_para_accounts
}

# Phase 5 DKG 修复（追加）：para 钱包 seed/解锁/AUTH 私钥导入。
# para 重启后钱包需要重新解锁，且 import 的 AUTH 账户需要重新导入（否则 TSS 拿不到 commit 私钥，
# submitMainChainTx 一直失败重试，BTC/RGB20 CrossChainInfo 建不出来）。
function prepare_para_accounts() {
    save_seed_and_unlock "${PARA1_CLI}" || true
    save_seed_and_unlock "${PARA2_CLI}" || true
    save_seed_and_unlock "${PARA3_CLI}" || true
    save_seed_and_unlock "${PARA4_CLI}" || true

    ${PARA1_CLI} account import_key -k "${AUTH_KEY1}" -l paraAuth1 >/dev/null || true
    ${PARA2_CLI} account import_key -k "${AUTH_KEY2}" -l paraAuth2 >/dev/null || true
    ${PARA3_CLI} account import_key -k "${AUTH_KEY3}" -l paraAuth3 >/dev/null || true
    ${PARA4_CLI} account import_key -k "${AUTH_KEY4}" -l paraAuth4 >/dev/null || true
}

function collect_peer_names_from_cli() {
    local cli="$1"
    ${cli} net peer | jq -r '[.. | objects | .name? | strings] | .[]' 2>/dev/null || true
}

function peer_count_from_cli() {
    local cli="$1"
    local peers
    peers=$(collect_peer_names_from_cli "${cli}" | sort | uniq | sed '/^$/d')
    echo "${peers}" | sed '/^$/d' | wc -l | xargs
}

function wait_para_dht_discovery() {
    log_step "wait para dht discovery"
    local retries=30
    local i
    for ((i = 0; i < retries; i++)); do
        local count
        count=$(peer_count_from_cli "${PARA1_CLI}")
        # In a 4-para topology, para1 should discover at least the other 3 peers,
        if [ "${count}" -ge 4 ]; then
            log_step "dht ready: para1=${count}"
            return 0
        fi
        sleep 1
    done
    fail "dht peer discovery timeout; check p2p.dht and container network"
}

function discover_tss_peer_names() {
    local retries=30
    local i
    for ((i = 0; i < retries; i++)); do
        # All para nodes use the same tss peers config. Query from one node is enough.
        local peers
        peers=$(collect_peer_names_from_cli "${PARA1_CLI}" | sort | uniq | sed '/^$/d')
        local count
        count=$(echo "${peers}" | sed '/^$/d' | wc -l | xargs)
        if [ "${count}" -ge 4 ]; then
            echo "${peers}" | head -4 | paste -sd, -
            return 0
        fi
        sleep 1
    done
    return 1
}

function rewrite_tss_peers_only() {
    local toml_peers
    toml_peers=$(join_csv_as_toml_array "${TSS_PEERS}")
    local file
    for file in chain33.para1.toml chain33.para2.toml chain33.para3.toml chain33.para4.toml; do
        perl -i -pe "s/^peers=.*/peers=${toml_peers}/" "${ROOT_DIR}/${file}"
    done
}

function copy_para_toml_into_container() {
    local svc="$1"
    local f="chain33.${svc}.toml"
    local cid
    cid=$(compose_cmd ps -q "${svc}" 2>/dev/null | head -1)
    assert_non_empty "${cid}" "no running container for ${svc}; copy ${f} skipped"
    docker cp "${ROOT_DIR}/${f}" "${cid}:/root/${f}"
}

function restart_para_nodes_with_new_toml() {
    log_step "push updated para toml into para1-4 and restart those services (main/btcd unchanged)"
    local svc
    for svc in para1 para2 para3 para4; do
        copy_para_toml_into_container "${svc}"
    done
    compose_cmd restart para1 para2 para3 para4
    sleep 3
}

function apply_dynamic_tss_peers_and_restart() {
    if [ "${AUTO_DISCOVER_TSS_PEERS}" != "true" ]; then
        log_step "skip dynamic peer discovery; use static TSS_PEERS=${TSS_PEERS}"
        return 0
    fi

    wait_para_dht_discovery

    local discovered
    discovered=$(discover_tss_peer_names) || fail "failed to discover 4 peer names from net peer"
    TSS_PEERS="${discovered}"
    log_step "discovered TSS_PEERS=${TSS_PEERS}"

    log_step "rewrite TSS peers in para toml, copy into containers, restart para only"
    rewrite_tss_peers_only
    restart_para_nodes_with_new_toml
    wait_cli_ready "${PARA1_CLI}"
    wait_cli_ready "${PARA2_CLI}"
    wait_cli_ready "${PARA3_CLI}"
    wait_cli_ready "${PARA4_CLI}"
}

# Phase 5 DKG 修复：把"发现 TSS peers 写入 toml"与"重启 para"解耦。
# nodegroup approve 前重启 para 会让 TSS init 因 guardian 未就绪而挂起；
# 故先只把 DHT 发现的 peers 写进 para toml，等 nodegroup 就绪后再统一重启 para。
function prepare_para_tss_peers() {
    if [ "${AUTO_DISCOVER_TSS_PEERS}" != "true" ]; then
        log_step "skip dynamic peer discovery; use static TSS_PEERS=${TSS_PEERS}"
        return 0
    fi

    wait_para_dht_discovery

    local discovered
    discovered=$(discover_tss_peer_names) || fail "failed to discover 4 peer names from net peer"
    TSS_PEERS="${discovered}"
    log_step "discovered TSS_PEERS=${TSS_PEERS}"

    log_step "rewrite TSS peers in para toml (no restart yet)"
    rewrite_tss_peers_only
}

# Phase 5 DKG 修复（核心）：把环境初始化（含 DKG）独立成一个阶段，与测试用例分离。
#   1. 主链 setup para nodegroup（apply+approve）——para 链 paracross fork 高度已与主链(local)对齐，
#      nodegroup apply/approve 在 para 链重放成功（见 config_para_file 的 fork 修复）；
#   2. 重启 para 节点——让 TSS init 重跑：para 初次启动时 nodegroup 尚未 approve，
#      CommitDKG 提交被 para 链 rgbx 拒绝（ErrGetGuardianNodeAddress）后无限重试挂起；
#      重启后 nodegroup 已就绪，DKG keygen 在 4 节点间重开并提交成功；
#   3. 重启后 para 钱包重新上锁，须再次解锁（save_seed_and_unlock），否则 TSS 拿不到 commit 私钥，
#      submitMainChainTx 一直失败重试（这是"init 完成但 BTC getCrossChainInfo 为空"的又一阻塞点）；
#   4. 等主链 BTC CrossChainInfo.tssAddress 落地（4 个 guardian 均提交 CommitDKG 后创建）。
# 这样 compose up 即"环境就绪"，测试阶段只跑用例、不碰初始化时序。
function init_chain33_dkg() {
    log_step "chain33 DKG init phase: nodegroup -> restart para(TSS re-init) -> unlock -> wait DKG commit"

    wait_cli_ready "${MAIN_CLI}"
    setup_para_nodegroup_on_main

    # 等 para RPC ready 再准备账户（wallet seed/import key 依赖 para 已启动，过早执行会静默失败）。
    wait_cli_ready "${PARA1_CLI}"
    wait_cli_ready "${PARA2_CLI}"
    wait_cli_ready "${PARA3_CLI}"
    wait_cli_ready "${PARA4_CLI}"
    # 钱包/账户就绪：TSS 提交 CommitDKG 需要钱包私钥（AUTH 地址），para 共识 fetchPriKey 也依赖解锁钱包。
    prepare_accounts

    # 重启 para（push 最新 toml + restart）：TSS init 重跑，nodegroup 已就绪可提交 DKG。
    restart_para_nodes_with_new_toml
    wait_cli_ready "${PARA1_CLI}"
    wait_cli_ready "${PARA2_CLI}"
    wait_cli_ready "${PARA3_CLI}"
    wait_cli_ready "${PARA4_CLI}"

    # 重启后 para 钱包重新上锁：再次解锁 para1-4（TSS 提交 CommitDKG / para 共识 fetchPriKey 都需要钱包私钥）。
    save_seed_and_unlock "${PARA1_CLI}" || true
    save_seed_and_unlock "${PARA2_CLI}" || true
    save_seed_and_unlock "${PARA3_CLI}" || true
    save_seed_and_unlock "${PARA4_CLI}" || true

    # 等主链 DKG 落地（4 个 guardian 均提交 CommitDKG 后创建 CrossChainInfo）。
    wait_auto_dkg_commit
    log_step "chain33 DKG init done"
}

function setup_para_nodegroup_on_main() {
    log_step "setup para nodegroup on main chain"
    ${MAIN_CLI} send coins transfer -t "${AUTH_ADDR1}" -a 100 -k "${GENESIS_KEY}" >/dev/null
    ${MAIN_CLI} send coins transfer -t "${AUTH_ADDR2}" -a 100 -k "${GENESIS_KEY}" >/dev/null
    ${MAIN_CLI} send coins transfer -t "${AUTH_ADDR3}" -a 100 -k "${GENESIS_KEY}" >/dev/null
    ${MAIN_CLI} send coins transfer -t "${AUTH_ADDR4}" -a 100 -k "${GENESIS_KEY}" >/dev/null

    ${MAIN_CLI} send coins send_exec -e paracross -a 20 -k "${AUTH_KEY1}" >/dev/null
    ${MAIN_CLI} send coins send_exec -e paracross -a 20 -k "${AUTH_KEY2}" >/dev/null
    ${MAIN_CLI} send coins send_exec -e paracross -a 20 -k "${AUTH_KEY3}" >/dev/null
    ${MAIN_CLI} send coins send_exec -e paracross -a 20 -k "${AUTH_KEY4}" >/dev/null

    local addrs="${AUTH_ADDR1},${AUTH_ADDR2},${AUTH_ADDR3},${AUTH_ADDR4}"
    local apply_hash
    apply_hash=$(${MAIN_CLI} send para nodegroup apply --paraName="${PARA_TITLE}" -a "${addrs}" -c 5 -k "${AUTH_KEY1}")
    assert_length "${apply_hash}" 66
    tx_wait "${MAIN_CLI}" "${apply_hash}"

    local approve_hash
    approve_hash=$(${MAIN_CLI} send para nodegroup approve --paraName="${PARA_TITLE}" -i "${apply_hash}" -c 5 -k "${AUTH_KEY1}")
    assert_length "${approve_hash}" 66
    tx_wait "${MAIN_CLI}" "${approve_hash}"
    ${MAIN_CLI} para nodegroup addrs --paraName="${PARA_TITLE}"
}

function wait_para_nodegroup_ready() {
    # Phase 5 DKG 修复：para 侧 nodegroup 需同步主链后才可查。
    # para 初次 TSS init 在 nodegroup approve 前已失败并挂起重试，此时 para 链可能停在旧高度，
    # 重启 para 重新同步后此轮询才能通过（作为 DKG 提交前置校验）。
    log_step "wait para nodegroup visible on para chain (para1)"
    local retries=120
    local i
    for ((i = 0; i < retries; i++)); do
        local addrs
        addrs=$(${PARA1_CLI} para nodegroup addrs --paraName="${PARA_TITLE}" 2>/dev/null | jq -r '.value // empty')
        local cnt
        cnt=$(echo "${addrs}" | tr ',' '\n' | sed '/^$/d' | wc -l | xargs)
        if [ "${cnt}" -ge 4 ]; then
            log_step "para nodegroup ready on para chain: ${addrs}"
            return 0
        fi
        sleep 2
    done
    fail "para nodegroup not visible on para chain within timeout"
}

function ensure_btc_crosschain_prerequisite() {
    log_step "check BTC cross-chain prerequisite only (no mint bootstrap)"
    set +e
    local info
    info=$(${MAIN_CLI} rgbx getCrossChainInfo -s "${MINT_SYMBOL}" 2>/dev/null)
    local rc=$?
    set -e
    if [ "${rc}" -ne 0 ]; then
        fail "BTC cross-chain info not ready; please pre-configure rgbx BTC asset and cross-chain metadata before running this CI"
    fi
    local symbol
    symbol=$(echo "${info}" | jq -r '.assetSymbol // empty')
    if [ -z "${symbol}" ]; then
        fail "BTC cross-chain info missing; this test does not create asset via mint"
    fi
}

function wait_auto_dkg_commit() {
    log_step "wait auto DKG commit by neutrino+tss"
    local retries=180
    local i
    for ((i = 0; i < retries; i++)); do
        set +e
        local info
        info=$(${MAIN_CLI} rgbx getCrossChainInfo -s "${MINT_SYMBOL}" 2>/dev/null)
        local rc=$?
        set -e
        if [ "${rc}" -eq 0 ]; then
            local tss_addr
            tss_addr=$(echo "${info}" | jq -r '.tssAddress // empty')
            if [ -n "${tss_addr}" ]; then
                log_step "auto DKG done, tssAddress=${tss_addr}"
                return 0
            fi
        fi
        sleep 1
    done
    fail "auto DKG commit timeout"
}

function scenario_para_health() {
    log_step "scenario: 4 para nodes sync and network health"
    ${PARA1_CLI} net is_sync >/dev/null
    ${PARA2_CLI} net is_sync >/dev/null
    ${PARA3_CLI} net is_sync >/dev/null
    ${PARA4_CLI} net is_sync >/dev/null
}

function ensure_btcd_network_consistency() {
    if [ "${BTC_NETWORK}" != "regtest" ]; then
        fail "unsupported BTC_NETWORK=${BTC_NETWORK}; this CI uses btcd --regtest only"
    fi
}

# =====================================================================
# RGB20 E2E helpers (Phase 5)
# =====================================================================


function run_tests() {
    # ===== 环境初始化（保留）：chain33 主链 + 4 para TSS + btcd + DKG =====
    ensure_btcd_network_consistency
    prepare_btcd_mining_identity
    wait_btcd_ready
    scenario_para_health
    setup_para_nodegroup_on_main

    # Phase 5 DKG 修复（追加，不改原有流程）：nodegroup 建立后 para 需要重启一次，
    # 让 TSS init 重跑（para 初次启动时 nodegroup 尚未 approve，CommitDKG 提交被拒后无限重试挂起）；
    # 重启后 nodegroup 已就绪、fork 高度已对齐，DKG 可提交成功。重启后钱包重新上锁，须再解锁。
    restart_para_nodes_with_new_toml
    wait_cli_ready "${PARA1_CLI}"
    wait_cli_ready "${PARA2_CLI}"
    wait_cli_ready "${PARA3_CLI}"
    wait_cli_ready "${PARA4_CLI}"
    # 重启后 para 钱包重新上锁 + AUTH 账户需重新导入，否则 TSS 拿不到 commit 私钥。
    prepare_para_accounts

    wait_auto_dkg_commit

    # ===== 测试入口（testcase.sh）：屏蔽旧 BTC + RGB20 全部（env/充值/提现/smoke）=====
    # 旧 BTC 功能用例已屏蔽（函数定义在 scripts/btc_test.sh，入口见 testcase.sh）。
    source "${ROOT_DIR}/testcase.sh"
    testcase_entry
}

function print_logs_hint() {
    log_step "collect logs with: ${COMPOSE_BIN} logs --tail=200"
    log_step "neutrino peer endpoint: ${BTC_P2P_ADDR}"
    log_step "neutrino rpc endpoint: ${BTC_RPC_ADDR}"
    log_step "for neutrino config: netName=${BTC_NETWORK}, connectPeers=[\"${BTC_P2P_ADDR}\"], btcRPC.host=\"${BTC_RPC_ADDR}\", btcRPC.disableTLS=false"
    log_step "TSS roles: para1 official(rank=0), para2-4 validators(rank=1), threshold=${TSS_THRESHOLD}"
}

function do_up_only() {
    ensure_btcd_network_consistency
    mkdir -p "${ROOT_DIR}/btcd-data"
    init_env
    # 快照（必须在下面任何 compose 命令之前）：留到 start_env 判"btcd 容器没了但链数据还在"。
    CHAIN33_VOLUME_PREEXISTED=$(chain33_state_volume_preexists)
    prepare_btcd_mining_identity
    start_env
    wait_btcd_ready
    apply_dynamic_tss_peers_and_restart
    prepare_accounts
}

function do_run_all() {
    do_up_only
    run_tests
    print_logs_hint
}

function do_down() {
    compose_cmd down --remove-orphans
}

# 一次性全量清空：删容器 + 删卷（main/para/DKG、btcd 链），下次 up 从零重建。
# 用于 btcd 清链等导致 chain33 头链与 BTC 链错位、无法原地恢复的场景 —— 只要 btcd 被重建或
# 重启（含 down 后 up、容器 stop 后 start、镜像/配置变更导致的重建），regtest 链就回到 genesis，
# chain33 已提交的旧链 BTC 头必然错位，这是**唯一**的恢复路径。
# 反过来：想连续跑 run 就不要碰 btcd 容器（见 start_env 的说明）。
function do_reset() {
    log_step "reset: remove containers AND volumes (DKG/chain state will be rebuilt on next up)"
    compose_cmd down -v --remove-orphans
}

case "${ACTION}" in
run)
    do_run_all
    ;;
up)
    do_up_only
    ;;
init)
    init_env
    ;;
config)
    prepare_accounts
    ;;
test)
    run_tests
    ;;
down)
    do_down
    ;;
reset)
    do_reset
    ;;
*)
    fail "unknown action: ${ACTION}"
    ;;
esac
