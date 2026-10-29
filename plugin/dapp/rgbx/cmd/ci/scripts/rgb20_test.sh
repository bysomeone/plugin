#!/usr/bin/env bash
# RGB20 全部测试（Phase 5）：环境初始化 + 充值 + 提现 + 两连提现 + sidecar smoke，一个文件按函数组织。
# 依赖 docker-compose.sh 已定义的辅助（MAIN_CLI/PARA1_CLI/compose_cmd/assert_*/log_step 等）。
# 由 testcase.sh 入口 source 本文件后调用 run_rgb20_* 系列函数。

# =====================================================================
# RGB20 环境初始化：等 DKG → GG18 公钥 → 发行 USDT → 起侧车
# =====================================================================

function query_rgb20_balance() {
    local addr="$1"
    ${MAIN_CLI} asset balance -a "${addr}" --asset_exec=rgbx --asset_symbol=X"${RGB20_SYMBOL}" | jq -r '.balance // "0"'
}

# Raw balance in min units (the exact integer from consensus state).
#
# `asset balance` formats through types.FormatAmount2FloatDisplay, which truncates anything
# beyond ShowPrecisionNum (4) decimals. An amount like 64000 min units (0.00064) therefore
# prints as 0.0006, so a display-based `>=` check can never reach an 8-decimal expectation.
# Compare min units whenever the expected amount is not representable in 4 decimals.
function query_rgb20_balance_units() {
    local addr="$1"
    curl -s -X POST "http://127.0.0.1:8801" \
        -H 'Content-Type: application/json' \
        -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"Chain33.GetBalance\",\"params\":[{\"addresses\":[\"${addr}\"],\"execer\":\"rgbx\",\"asset_exec\":\"rgbx\",\"asset_symbol\":\"X${RGB20_SYMBOL}\"}]}" |
        jq -r '.result[0].balance // "0"'
}

# rgbx_query <funcName> <payload-json>: call a read-only rgbx query through the main node's
# JSON-RPC (port 8801 is published on the host). The generic Chain33.Query entry dispatches on the
# executor's Query_<funcName> methods, so no CLI subcommand is needed for observation helpers.
function rgbx_query() {
    local func_name="$1" payload="$2"
    curl -s -X POST "http://127.0.0.1:8801" \
        -H 'Content-Type: application/json' \
        -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"Chain33.Query\",\"params\":[{\"execer\":\"rgbx\",\"funcName\":\"${func_name}\",\"payload\":${payload}}]}"
}

# normalize_tx_hash：去掉 0x 前缀并转小写。CLI/日志用带 0x 的哈希，节点日志与查询载荷用裸
# 64-hex；比较前统一成同一份拼写（与 #70 场景同一口径）。
function normalize_tx_hash() {
    echo "$1" | tr 'A-Z' 'a-z' | sed 's/^0x//'
}

# send_main_rgbx_action <actionName> <payload-json> <privkey> → 打印主链交易哈希（0x...）。
#
# rgbx 的 RegisterDepositScript / Abandon 只在执行器里注册了 action，没有 chain33-cli 子命令。
# 这里走通用的 Chain33.CreateTransaction（execer=rgbx + actionName + JSON payload）→
# wallet sign（用给定私钥）→ wallet send。payload 的 bytes 字段按 chain33 jsonpb 的 hex 约定传。
function send_main_rgbx_action() {
    local action="$1" payload="$2" key="$3"
    local created raw signed sent tx_hash
    created=$(curl -s -X POST "http://127.0.0.1:8801" \
        -H 'Content-Type: application/json' \
        -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"Chain33.CreateTransaction\",\"params\":[{\"execer\":\"rgbx\",\"actionName\":\"${action}\",\"payload\":${payload}}]}")
    raw=$(echo "${created}" | jq -r '.result // empty')
    if [ -z "${raw}" ]; then
        # This helper runs inside $( ), so a stdout `fail` would be captured into the caller's
        # variable and the message would vanish; send it to stderr (the run log) instead.
        fail "Chain33.CreateTransaction ${action} failed: $(echo "${created}" | head -c 500)" 1>&2
    fi
    if ! signed=$(${MAIN_CLI} wallet sign -d "${raw}" -k "${key}" 2>&1); then
        fail "wallet sign ${action} failed: ${signed}" 1>&2
    fi
    signed=$(echo "${signed}" | tr -d '\r' | tail -1)
    if ! sent=$(${MAIN_CLI} wallet send -d "${signed}" 2>&1); then
        fail "wallet send ${action} failed: ${sent}" 1>&2
    fi
    tx_hash=$(echo "${sent}" | tr -d '\r' | tail -1)
    if [ "${#tx_hash}" -ne 66 ]; then
        fail "${action}: wallet send did not return a 0x transaction hash (got '${tx_hash}')" 1>&2
    fi
    echo "${tx_hash}"
}

# wait_main_tx_committed <tx hash> [retries]：等主链交易落块（height > 0），打印 "<height> <index>"。
# wallet send 返回时交易只进了 mempool（solo 矿工通常立刻出块，但不保证），receipt 查询必须以
# 落块后的详情为准。
function wait_main_tx_committed() {
    local bare="$1" retries="${2:-60}" i details height
    for ((i = 0; i < retries; i++)); do
        details=$(${MAIN_CLI} tx query -s "${bare}" 2>/dev/null) || details=""
        height=$(echo "${details}" | jq -r '.height // 0' 2>/dev/null) || height=0
        if [[ "${height}" =~ ^[0-9]+$ ]] && [ "${height}" -gt 0 ]; then
            echo "${height} $(echo "${details}" | jq -r '.index // 0')"
            return 0
        fi
        sleep 1
    done
    fail "main-chain tx not committed after ${retries}s: ${bare}" 1>&2
}

# main_tx_receipt_log <bare tx hash> <log name>：取回执里指定 tyName 的第一条日志（JSON，无则空）。
function main_tx_receipt_log() {
    local bare="$1" name="$2"
    ${MAIN_CLI} tx query -s "${bare}" 2>/dev/null |
        jq -c --arg n "${name}" '[.receipt.logs[]? | select(.tyName == $n) | .log] | first // empty' || true
}

# main_tx_receipt_ty <bare tx hash>：回执类型名（ExecOk / ExecErr）。
function main_tx_receipt_ty() {
    local bare="$1"
    ${MAIN_CLI} tx query -s "${bare}" 2>/dev/null | jq -r '.receipt.tyName // empty' || true
}

# query_withdraw_final_state <burn tx hash>: "" (still locked) | "settled" | "released".
# This reads the consensus-state terminal key, i.e. the same value both Confirm and Unlock write.
function query_withdraw_final_state() {
    local burn_hash="$1"
    rgbx_query "GetWithdrawFinalState" "{\"data\":\"${burn_hash}\"}" | jq -r '.result.data // empty'
}

# query_rgb20_lock_address: the shared address that holds unsettled withdrawal amounts.
function query_rgb20_lock_address() {
    rgbx_query "GetCrossChainLockAddress" "{\"data\":\"${RGB20_SYMBOL}\"}" | jq -r '.result.data // empty'
}

# Wait until an address's balance equals want (numeric comparison: the CLI prints 4 decimals).
function wait_rgb20_balance_equal() {
    local addr="$1" want="$2" retries="${3:-120}"
    local i balance
    for ((i = 0; i < retries; i++)); do
        balance=$(query_rgb20_balance "${addr}")
        if awk -v a="${balance}" -v b="${want}" 'BEGIN{exit !(a == b)}'; then
            echo "${balance}"
            return 0
        fi
        sleep 1
    done
    echo "${balance}"
    return 1
}

# Wait until an address holds at least expected_units min units (compared in raw units).
function wait_rgb20_balance_not_less_than() {
    local addr="$1"
    local expected_units="$2"
    local retries="${3:-60}"
    local i
    for ((i = 0; i < retries; i++)); do
        local balance_units
        balance_units=$(query_rgb20_balance_units "${addr}")
        if [ "${balance_units:-0}" -ge "${expected_units}" ] 2>/dev/null; then
            log_step "rgb20 balance reached: addr=${addr}, balance=${balance_units} min units >= ${expected_units}"
            return 0
        fi
        # 主链铸币依赖签名节点的 VerifyDepositSpv —— 它查主链 lightclient 的 BTC 头。
        # 充值付款交易通常落在 btcd 的 tip 上，而 para1 提交 BTC 头受 blockConfirmations(=1)
        # 限制只能提交到 tip-1；若此时链条不再前进，该高度的头永远提交不上去，spv verify 一直
        # ErrNotFound、充值永不铸币（全新重建后必现）。这里等待期间持续挖块，让头链跟上。
        mine_btcd_blocks 1
        sleep 2
    done
    fail "rgb20 balance not reached, addr=${addr}, expected>=${expected_units} min units"
}

# para1（官方节点，跑 Go 桥）在 since_epoch（unix 秒）之后新产生的日志。
# 用于断言"本轮没有出现某个错误"——例如提现覆盖校验失败 / 每秒重试刷屏。
function para1_logs_since() {
    local since_epoch="$1"
    local elapsed=$(( $(date +%s) - since_epoch ))
    if [ "${elapsed}" -lt 1 ]; then
        elapsed=1
    fi
    compose_cmd logs --no-color --since "${elapsed}s" para1 2>&1 || true
}

# para_logs_since <service> <since_epoch>：任意 para 节点在该时刻之后的日志。
# 跨节点 watch 集传播（para2-4 从侧车并集补齐）只能从**各自**的日志观察，不能只看官方节点。
function para_logs_since() {
    local svc="$1" since_epoch="$2"
    local elapsed=$(( $(date +%s) - since_epoch ))
    if [ "${elapsed}" -lt 1 ]; then
        elapsed=1
    fi
    compose_cmd logs --no-color --since "${elapsed}s" "${svc}" 2>&1 || true
}

# main（主链）节点在 since_epoch（unix 秒）之后新产生的日志。
# 用途与 para1_logs_since 相同，但**判定发生在主链**的那些护栏要看这里：rgbx 的 CheckTx
# （充值/提现/确认的共识判据）在主链上跑，护栏的诊断日志因此写在 main 而不是 para1。
function main_logs_since() {
    local since_epoch="$1"
    local elapsed=$(( $(date +%s) - since_epoch ))
    if [ "${elapsed}" -lt 1 ]; then
        elapsed=1
    fi
    compose_cmd logs --no-color --since "${elapsed}s" main 2>&1 || true
}

# 主链节点**日志文件**的候选路径（相对 chain33 进程的 CWD；CI 镜像的 WORKDIR=/root，
# 日志文件名来自 chain33.test.toml 的 logFile="logs/chain33.log"）。
#
# 为什么取证要读文件而不是 docker logs：CI 的日志配置是 loglevel=debug 但
# logConsoleLevel=info —— Debug 行只进文件、不上 stdout，而 docker logs 抓的是 stdout。
# 护栏的"进入"证据（窗口外查询的诊断）正是 Debug 级别，只在文件里。
#
# 已实机确认（2026-09-19 主会话跑的 E2E，非仅代码推导）：`/root/logs/chain33.log` 存在且可读，
# 场景从它里面读到了护栏的窗口外诊断（scenario_btc_header_guard_armed 全绿）。
# 保留候选列表只是兜底，不影响已验证的路径。
MAIN_LOG_FILE_CANDIDATES="${MAIN_LOG_FILE_CANDIDATES:-/root/logs/chain33.log logs/chain33.log}"

# 选中第一个可读且非空的日志文件路径（都读不到时输出空串，调用方据此判失败）。
function main_log_file_path() {
    local path n
    for path in ${MAIN_LOG_FILE_CANDIDATES}; do
        n=$(compose_cmd exec -T main sh -c "wc -l < '${path}'" 2>/dev/null | tr -d '[:space:]' || true)
        if [[ "${n}" =~ ^[0-9]+$ ]] && [ "${n}" -gt 0 ]; then
            echo "${path}"
            return 0
        fi
    done
    echo ""
}

# 主链日志文件的当前行数（读不到回 0）。
function main_log_line_count() {
    local path="$1" n
    n=$(compose_cmd exec -T main sh -c "wc -l < '${path}'" 2>/dev/null | tr -d '[:space:]' || true)
    if [[ "${n}" =~ ^[0-9]+$ ]]; then
        echo "${n}"
        return 0
    fi
    echo 0
}

# 打印主链日志文件自 start_line（含）之后的新行。
function main_log_file_since_line() {
    local path="$1" start_line="$2" from
    from="${start_line}"
    if ! [[ "${from}" =~ ^[0-9]+$ ]] || [ "${from}" -lt 1 ]; then
        from=1
    fi
    compose_cmd exec -T main sh -c "tail -n +${from} '${path}'" 2>&1 || true
}

# 本轮这笔提现在链上的 pending 金额（= CLI 换算出的最小单位数，= 实际 burn 额）。
function query_pending_withdraw_amount() {
    local from_addr="$1"
    local tx_hash="$2"
    ${MAIN_CLI} rgbx listPendingTxByFrom -f "${from_addr}" |
        jq -r --arg h "$(echo "${tx_hash}" | tr 'A-Z' 'a-z')" \
        '[.pendingList[]? | select(.actionType == 106) | select((.txHash | ascii_downcase) == $h) | .amount] | first // empty'
}

# 等 pending 落链后读回金额（CLI 返回 tx hash 与交易执行之间有竞态）。
function wait_pending_withdraw_amount() {
    local from_addr="$1"
    local tx_hash="$2"
    local retries="${3:-30}"
    local i amount
    for ((i = 0; i < retries; i++)); do
        amount=$(query_pending_withdraw_amount "${from_addr}" "${tx_hash}")
        if [ -n "${amount}" ] && [ "${amount}" != "null" ]; then
            echo "${amount}"
            return 0
        fi
        mine_btcd_blocks 1
        sleep 1
    done
    echo ""
}

function wait_rgb20_dkg_commit() {
    log_step "wait RGB20 DKG commit (RGB20 CrossChainInfo pubkey)"
    local retries=90
    local i
    for ((i = 0; i < retries; i++)); do
        set +e
        local info
        info=$(${MAIN_CLI} rgbx getCrossChainInfo -s "${RGB20_SYMBOL}" 2>/dev/null)
        local rc=$?
        set -e
        if [ "${rc}" -eq 0 ]; then
            local pub
            pub=$(echo "${info}" | jq -r '.pubkey // empty')
            if [ -n "${pub}" ]; then
                log_step "RGB20 DKG done, pubkey=${pub}"
                return 0
            fi
        fi
        sleep 1
    done
    fail "RGB20 DKG commit timeout"
}

function rgb20_tss_pubkey() {
    ${MAIN_CLI} rgbx getCrossChainInfo -s "${RGB20_SYMBOL}" | jq -r '.pubkey // empty'
}

# 等侧车 gRPC 端口就绪（50061，host 映射）。
function wait_rgb20_sidecar_grpc() {
    local retries=60
    local i
    for ((i = 0; i < retries; i++)); do
        if nc -z 127.0.0.1 50061 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    fail "rgb-sidecar gRPC 50061 not ready"
}

# btcd regtest 的 segwit 靠 BIP9 激活（MinerConfirmationWindow=144 / RuleChangeActivationThreshold=108）：
# 全新重建后链上只有 warm-up 挖出的 ~105 块时，segwit 仍是 lockedin，任何 P2WPKH（TSS 单脚本
# 地址）花费都会被 btcd 拒绝：
#   TX rejected: ... has witness data, but segwit isn't active yet
# 充值付款交易因此广播失败、E2E 卡在第一步。实测 h=290 仍 lockedin、h=490 active，故先把链挖到
# 500 以上再进 E2E（regtest 挖块是秒级的，代价可忽略）。
function ensure_btcd_segwit_active() {
    local target_height="${1:-500}"
    local height status
    height=$(${BTC_CTL} --"${BTC_NETWORK}" getblockcount)
    while [ "${height}" -lt "${target_height}" ]; do
        mine_btcd_blocks 100
        height=$(${BTC_CTL} --"${BTC_NETWORK}" getblockcount)
    done
    local i
    for ((i = 0; i < 20; i++)); do
        status=$(${BTC_CTL} --"${BTC_NETWORK}" getblockchaininfo | jq -r '.bip9_softforks.segwit.status // empty')
        if [ "${status}" = "active" ]; then
            log_step "btcd segwit active (height=${height}, status=${status})"
            return 0
        fi
        mine_btcd_blocks 50
        height=$(${BTC_CTL} --"${BTC_NETWORK}" getblockcount)
    done
    fail "btcd segwit not active (height=${height}, status=${status})"
}

function run_rgb20_env() {
    log_step "RGB20 env: wait DKG -> fund TSS on btcd -> issue USDT at GG18 script -> start sidecar"

    # RGB 链已收敛到 btcd：给 TSS 地址注资需要挖矿私钥（WIF，--miningaddr=mrCDr）。
    # 未提前 prepare 时现场推导（docker-compose.sh prepare_btcd_mining_identity 会设置 BTC_FUNDING_WIF）。
    if [ -z "${BTC_FUNDING_WIF:-}" ]; then
        prepare_btcd_mining_identity
    fi

    # 必须早于任何 P2WPKH 花费（充值付款 / 提现广播）：全新重建时链太短，segwit 未激活。
    ensure_btcd_segwit_active

    wait_rgb20_dkg_commit
    local pubkey
    pubkey=$(rgb20_tss_pubkey)
    assert_non_empty "${pubkey}" "rgb20 GG18 pubkey empty"
    log_step "GG18 pubkey=${pubkey}"
    export RGB20_TSS_PUBKEY="${pubkey}"

    # 停占位 pubkey 的侧车，清空其数据目录，用 GG18 公钥发行 USDT，再启动侧车。
    #
    # 每轮必须重建账本并重新发行（而不是保留既有资产）：账户从 genesis 领到 100 USDT 后，充值
    # 的"付款方"由侧车自己用它的 seal 模拟（simulate_user_pay 花掉桥的 seal、找零回到 TSS 脚本），
    # 而账本只登记"收到的 seal"，桥的可支配余额会随每次付款变少 —— 保留账本时下一次 run 连
    # 充值发票都付不出来（实测 "insufficient USDT: need 1000000, have 0 (across 0 minted seals)"）。
    # 重发会换掉 asset id，从而让上一轮遗留的 pending 变成"永远不可能成功"—— 这部分由桥的
    # 不可恢复提现处理（停止重试 + 落盘状态 + 明确报错，见 lightclient neutrino）与下面
    # wait_no_withdraw_pending_for_user 的"只认本轮这一笔"共同消化，不再让环境永久不可重跑。
    compose_cmd stop rgb-sidecar >/dev/null 2>&1 || true
    compose_cmd run --rm --no-deps rgb-sidecar sh -c 'rm -rf /data/* /data/.[!.]* 2>/dev/null || true' >/dev/null 2>&1 || true

    local issue_out
    issue_out=$(compose_cmd run --rm --no-deps \
        -e RGB_SIDECAR_TSS_PUBKEY="${pubkey}" \
        -e RGB_BITCOIND_RPC="${RGB20_BITCOIND_RPC:-btcd:18443}" \
        -e RGB_BITCOIND_USER="${RGB20_BITCOIND_USER:-root}" \
        -e RGB_BITCOIND_PASS="${RGB20_BITCOIND_PASS:-1314}" \
        -e RGB_BITCOIND_CERT="${RGB20_BITCOIND_CERT:-/btcd/rpc.cert}" \
        -e BTC_FUNDING_WIF="${BTC_FUNDING_WIF}" \
        rgb-sidecar /sidecar/issue_usdt 2>&1)
    echo "${issue_out}" | tail -15
    echo "${issue_out}" | grep -q "ISSUE-DONE" || fail "issue_usdt did not complete"

    # 这个驱动必须由 docker-compose.sh 那套 harness 提供辅助函数（本函数依赖 config_para_file 渲染出的
    # `assetId=` 占位行 + apply_rgb20_asset_id_and_restart）。若辅助来自别的（旧）文件——例如
    # build/ci 下几个 source ./harness_lib.sh 的临时驱动——这里明确报出来，而不是让下一行的
    # `apply_rgb20_asset_id_and_restart: command not found` 去误导排查。
    if ! declare -F apply_rgb20_asset_id_and_restart >/dev/null; then
        fail "apply_rgb20_asset_id_and_restart undefined: this driver did not source the harness that injects contracts.assetId (use './docker-compose.sh run'; a scratch driver sourcing a stale harness_lib.sh must be retargeted)"
    fi

    # 合约身份（#58）：把**刚发行出来的**合约 id 回填进 4 个 para 的 `contracts.assetId` 并重启光客户端。
    #
    # 取值只认侧车产物：issue_usdt 打印的 `issued <SYM> asset_id=rgb:...`（engine.issue_asset 的返回值）。
    # 不要在这里拼字符串 —— 合约 id 的来源只能是侧车自己算出来的那个（配置串了/字节过期了都要当场失败）。
    #
    # 不做这一步的症状（供反查）：桥的充值路径强校验合约身份且 **fail-closed**，配不出 assetId 时
    # **每一笔充值都被拒**（提现不受影响）—— 充值场景卡在余额不涨，para1 日志里是
    #   deposit asset contract mismatch: contract assetId not configured: symbol=RGB20_USDT declares no assetId ...
    # 或（配了但配错）... configuredAssetId="..." sidecarReportedAssetId="..."。
    local rgb20_asset_id
    rgb20_asset_id=$(echo "${issue_out}" | sed -n 's/^issued [^ ]* asset_id=\(.*\)$/\1/p' | head -1 | tr -d '\r')
    assert_non_empty "${rgb20_asset_id}" \
        "cannot read the issued asset id from the issue_usdt output ('issued <SYM> asset_id='); the bridge rejects every deposit until contracts.assetId is configured"
    log_step "rgb20 issued contract asset_id=${rgb20_asset_id}"
    export RGB20_ASSET_ID="${rgb20_asset_id}"

    compose_cmd up -d rgb-sidecar
    wait_rgb20_sidecar_grpc
    # 侧车先起来，再重启 para：重启后的桥会立刻连侧车（连不上也能自愈重试，但少一个失败窗口）。
    apply_rgb20_asset_id_and_restart "${rgb20_asset_id}"
    wait_bridge_signing_ready
    # 精度上链的端到端断言（本次改动的核心）：BTC 类是协议常量 8（随 CommitDKG），RGB20 类是
    # 合约声明的 8（桥核对后经 crossChainMeta 回填）。回填要等重启后的桥重新连上侧车，故给足重试。
    assert_on_chain_precision "${MINT_SYMBOL}" 0 8
    assert_on_chain_precision "${RGB20_SYMBOL}" 1 "${RGB20_PRECISION}"
    log_step "RGB20 env done: sidecar up (GG18 pubkey), contracts.assetId=${RGB20_ASSET_ID}, " \
        "on-chain precision: ${MINT_SYMBOL}=8 (BTC kind), ${RGB20_SYMBOL}=${RGB20_PRECISION} (RGB20 kind)"
}

# =====================================================================
# 桥"可签名"就绪等待（CGGMP 起必需，不是可选的保险）
# =====================================================================

# 桥的 rgb20 HTTP（充值 CreateReceive / sign-psbt / 扫集协调）是在 **dkgCompleted 之后**才起来的：
# client.Start → waitDKGCompleted → bw.start → rgb20.Start。
#
# GG18 时代 dkgCompleted 紧跟 DKG（亚秒级），所以"链上 CrossChainInfo 出现"（wait_rgb20_dkg_commit）
# 之后桥基本已可用，无需单独等待。**切到 CGGMP 后不再成立**：DKG 之后每个节点还必须跑一次**必经的
# refresh 阶段**（现生成 2048-bit Paillier 安全素数，本机实测数十秒；见 CONFIG.md §4.3.2）才会置
# dkgCompleted。不等它就发充值请求，curl 直接连接失败 ⇒ 断言的 "receive_id empty" 是**假失败**
# （桥还没开始服务），会把"协议启动更慢"误报成"桥坏了"。
#
# 判据必须落在**应用层**的应答上，不能只看"curl 成不成功"：容器端口还没被进程监听时，
# Docker Desktop 的端口转发会在宿主侧先接住连接并回 **502**（实测），curl 仍然退出 0 —— 只看 curl
# 的退出码会立刻"就绪"，等于没等（本 harness 第一版就是这么假通过的）。所以取状态码：
#   - 连接被拒（容器没起/端口没发布）→ 000 → 未就绪；
#   - 502/503（Docker 转发器：容器在、但里面没人监听）→ 未就绪；
#   - 应用自己的应答（`/` 走 ServeMux 默认 → 404）→ **就绪**。
function wait_bridge_signing_ready() {
    log_step "wait bridge rgb20 HTTP ready (CGGMP: the mandatory refresh phase must finish first)"
    local i code
    for ((i = 0; i < 300; i++)); do
        code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:17000/" 2>/dev/null || echo 000)
        if [ "${code}" != "000" ] && [ "${code}" -lt 500 ]; then
            log_step "bridge rgb20 HTTP ready (status=${code}, after ~${i}s)"
            return 0
        fi
        sleep 1
    done
    fail "bridge rgb20 HTTP not ready after 300s (CGGMP refresh stuck? check the para1 log for 'ensureRefresh')"
}

# assert_on_chain_precision 链上资产的**类别 + 精度**断言（本次改动的端到端判据）。
#
# 为什么必须断言链上那份：前端与 CLI 换算金额的唯一依据就是 crossChainInfo 上的这两个字段
# （precision 决定 `-a` 的小数位，assetKind 决定充值/提现走哪条门禁分支）。它们是"搬上链"的
# 产物，只在桥的内存里对不算数 —— 只有 `getCrossChainInfo` 查得到才算真的落链。
#
# 两条来源不同，都要覆盖：
#   - BTC 类：精度是协议常量 8，随 CommitDKG 一起落链；
#   - RGB20 类：精度来自**合约**（issue_usdt 发行时声明），由桥在侧车核对出合约精度后经
#     crossChainMeta 一次性回填 —— 不落链的话，调用方只能手填精度（旧行为，填错静默差 10^n 倍）。
function assert_on_chain_precision() {
    local symbol="$1" want_kind="$2" want_precision="$3" retries="${4:-60}"
    local i info kind precision
    for ((i = 0; i < retries; i++)); do
        info=$(${MAIN_CLI} rgbx getCrossChainInfo -s "${symbol}" 2>/dev/null) || true
        kind=$(echo "${info}" | jq -r '.assetKind // empty' 2>/dev/null) || true
        precision=$(echo "${info}" | jq -r '.precision // empty' 2>/dev/null) || true
        if [ "${kind}" = "${want_kind}" ] && [ "${precision}" = "${want_precision}" ]; then
            log_step "on-chain asset registration OK: symbol=${symbol} assetKind=${kind} precision=${precision}"
            return 0
        fi
        sleep 1
    done
    fail "on-chain asset registration mismatch: symbol=${symbol} want assetKind=${want_kind} precision=${want_precision}, got assetKind=${kind:-<empty>} precision=${precision:-<empty>} (the bridge records a bridged asset's kind at DKG commit and its precision after verifying the contract; the CLI and the frontend read them from here)"
}

# =====================================================================
# RGB20 充值 E2E
# =====================================================================

# gateway_deposit_invoice <request_id> <amount>
#
# Plays the user: sign a deposit request with the user's chain33 key, post it to the gateway, and
# wait for the official node to publish the invoice. Prints the gateway record (JSON) on success.
function gateway_deposit_invoice() {
    local request_id="$1" amount="$2" signed rec
    signed=$(cd "$(repo_root)" && go run ./plugin/dapp/rgbx/cmd/rgb-deposit-sign \
        -priv "${USER_MAIN_KEY}" -chain-title "user.p.rgbx." \
        -request-id "${request_id}" -symbol "${RGB20_SYMBOL}" -amount "${amount}") || return 1
    curl -sf -X POST "http://127.0.0.1:${RGB20_GATEWAY_PORT}/deposit/requests" \
        -H 'Content-Type: application/json' -d "${signed}" >/dev/null || return 1
    for _ in $(seq 1 30); do
        rec=$(curl -s "http://127.0.0.1:${RGB20_GATEWAY_PORT}/deposit/requests/${request_id}")
        if [ -n "$(echo "${rec}" | jq -r '.invoice // empty')" ]; then
            echo "${rec}"
            return 0
        fi
        sleep 1
    done
    return 1
}

# scenario_rgb20_deposit [request_id] [amount]
#
# request_id 默认 rgb20-dep-1；**同一进程里第二次调用必须换一个**——CreateReceive 按 requestId
# 幂等，复用同一个 id 拿到的是上一轮那个 receive（可能已 settled），于是场景看着"跑到了 user_pay"，
# 实际在给一个旧 receive 重付款，失败信息会指向别处（余额/psbt），排查成本很高。
# 冷备演练就是在同一个进程里第二次调它，故显式传 drill 专用的 id（见 backup_test.sh）。
# amount 默认 RGB20_DEPOSIT_AMOUNT；同样给演练留了压额度的口子（付款方是桥自己的 seal，
# 可支配额度受"备份那一刻账本还剩多少 minted seal"限制）。
function scenario_rgb20_deposit() {
    local request_id="${1:-rgb20-dep-1}"
    local deposit_amount="${2:-${RGB20_DEPOSIT_AMOUNT}}"
    log_step "scenario: RGB20 deposit (user pay -> sidecar settle -> TSS deposit sign -> chain33 mint) requestId=${request_id} amount=${deposit_amount}"

    # 1. 用户签名充值请求 → 网关（公开 API）→ SSE 推给官方节点 → 桥出票
    local rec_json invoice receive_id recipient_id
    rec_json=$(gateway_deposit_invoice "${request_id}" "${deposit_amount}") ||
        fail "gateway did not publish an invoice for request ${request_id}"
    invoice=$(echo "${rec_json}" | jq -r '.invoice // empty')
    receive_id=$(echo "${rec_json}" | jq -r '.receive_id // empty')
    recipient_id=$(echo "${rec_json}" | jq -r '.recipient_id // empty')
    assert_non_empty "${receive_id}" "rgb20 receive_id empty (gateway: ${rec_json})"
    assert_non_empty "${invoice}" "rgb20 invoice empty (gateway: ${rec_json})"
    assert_non_empty "${recipient_id}" "rgb20 recipient_id empty (gateway: ${rec_json})"
    log_step "  receive_id=${receive_id}"

    # 2. 侧车 test-sim 构建用户付款（未签 PSBT + consignment）
    local pay_json
    pay_json=$(curl -s -X POST http://127.0.0.1:50064/sim/user_pay \
        -H 'Content-Type: application/json' -d "{\"invoice\":\"${invoice}\"}")
    local psbt_hex cons_hex
    psbt_hex=$(echo "${pay_json}" | jq -r '.psbt // empty')
    cons_hex=$(echo "${pay_json}" | jq -r '.consignment // empty')
    # 失败时把侧车的原始应答一起打出来：/sim/* 的错误体是 {"error": "..."}，只取 .psbt/.status
    # 会让真正的失败原因（余额不足、seal 状态、consignment 校验）全部丢失。
    [ -n "${psbt_hex}" ] ||
        fail "rgb20 user_pay psbt empty; sidecar response: $(echo "${pay_json}" | head -c 600)"
    [ -n "${cons_hex}" ] ||
        fail "rgb20 user_pay consignment empty; sidecar response: $(echo "${pay_json}" | head -c 600)"

    # 3. Go 桥 TSS 组签名 PSBT（test sign-psbt 端点）。桥不对外开放任何端口：测试端点只绑
    #    para1 容器内的 127.0.0.1，所以从容器里 curl。
    local signed_psbt
    signed_psbt=$(compose_cmd exec -T para1 curl -sf -X POST http://127.0.0.1:17000/rgbx/v1/sign-psbt \
        -H 'Content-Type: application/json' -d "{\"psbt\":\"${psbt_hex}\"}" | jq -r '.data.psbt // empty')
    assert_non_empty "${signed_psbt}" "rgb20 sign-psbt empty"

    # 4. 侧车 test-sim 广播已签 PSBT + provide consignment → settle
    local settle_json settle_status
    settle_json=$(curl -s -X POST http://127.0.0.1:50064/sim/user_pay_submit \
        -H 'Content-Type: application/json' \
        -d "{\"psbt\":\"${signed_psbt}\",\"consignment\":\"${cons_hex}\",\"receive_id\":\"${receive_id}\"}")
    settle_status=$(echo "${settle_json}" | jq -r '.status // empty')
    [ "${settle_status}" = "settled" ] ||
        fail "rgb20 settle status expect=settled actual=${settle_status:-<empty>}; sidecar response: $(echo "${settle_json}" | head -c 600)"

    # 本场景的日志窗口起点（下面断言"首次归因没有报 empty txid"）。
    local deposit_started_at
    deposit_started_at=$(date +%s)

    # 4.5 以"用户钱包"身份把 consignment 投递到网关（标准 transport，consignment.post）。
    # 桥自己轮询取件、校验、ACK，并在 receive 记录里保存字节（submitDeposit 需要）。
    local txid cons_file cons_up
    txid=$(echo "${settle_json}" | jq -r '.txid // empty')
    assert_non_empty "${txid}" "rgb20 settle txid empty: ${settle_json}"
    cons_file="$(mktemp "${TMPDIR:-/tmp}/rgb20-consignment.XXXXXX")"
    echo "${cons_hex}" | xxd -r -p >"${cons_file}"
    cons_up=$(curl -s -X POST "http://127.0.0.1:${RGB20_GATEWAY_PORT}/0.2/json-rpc" \
        -F 'jsonrpc=2.0' -F 'id=1' -F 'method=consignment.post' \
        -F "params={\"recipient_id\":\"${recipient_id}\",\"txid\":\"${txid}\",\"vout\":1}" \
        -F "file=@${cons_file}")
    rm -f "${cons_file}"
    echo "${cons_up}" | jq -e '.result != null' >/dev/null ||
        fail "rgb20 consignment.post failed: ${cons_up}"

    # 5. Go 桥 pollTransfers → submitDeposit（TSS 签 deposit）→ chain33 铸造 X.RGB20_USDT
    # 期望增量必须用**实际下单金额**（第 2 个参数），不能用默认值：冷备演练会用压过的额度调用本函数
    # （实测 44000 vs 默认 1000000），沿用默认值会等一个永远不会到的余额。
    # 全程按最小单位比较：deposit_amount 不是 4 位小数（如 44000 = 0.00044）时，
    # `asset balance` 的显示值会被截断（0.00064 -> 0.0006），用它做 >= 判据永远不成立。
    local before_units expected_units delta
    before_units=$(query_rgb20_balance_units "${USER_MAIN_ADDR}")
    [ -n "${before_units}" ] && [ "${before_units}" -ge 0 ] 2>/dev/null ||
        fail "rgb20 balance (min units) unreadable for ${USER_MAIN_ADDR} (got '${before_units}'); cannot compute the expected deposit balance"
    expected_units=$(( before_units + deposit_amount ))
    delta=$(awk "BEGIN{printf \"%.8f\", ${deposit_amount}/100000000}")
    wait_rgb20_balance_not_less_than "${USER_MAIN_ADDR}" "${expected_units}"
    log_step "RGB20 deposit OK: balance ${before_units} -> >= ${expected_units} min units (delta ${delta})"

    # 6. 首次归因必须一次成功。旧实现在首次归因分支里拿着 Settle() 之前的陈旧副本调
    #    BuildSpvProof，必然报 "build spv proof: empty txid"，要等下一轮 30s 轮询拿到
    #    settled 记录才自愈（三轮 run 均复现）。这里用日志窗口钉住"不再发生"。
    local deposit_logs
    deposit_logs=$(para1_logs_since "${deposit_started_at}")
    if echo "${deposit_logs}" | grep -q "build spv proof: empty txid"; then
        fail "deposit first attribution used a stale receive record: $(echo "${deposit_logs}" | grep -m1 'empty txid')"
    fi
    log_step "RGB20 deposit: first attribution succeeded without retry (no empty txid)"

    # 供 E16 负例复用：这笔已 settle 的 consignment 与它对应的 receive。
    # 复用而不是再造一笔，是为了**不额外消耗流动性** —— 付款方是桥自己的 seal（见文件头）。
    RGB20_SETTLED_CONSIGNMENT="${cons_hex}"
    RGB20_SETTLED_RECEIVE_ID="${receive_id}"
}

# E16 · consignment 必须与它被指到的 receive 对应（hint 是**缩小范围**，不是**跳过校验**）
#
# 为什么必须测它：Go 桥**总是**带 receive_id_hint（rgb20/deposit.go:111），所以
# provide_consignment 里"按 asset+amount 搜索"那条分支在生产里从不执行 —— 校验只能落在 hint
# 这条路上。修之前 hint 路径只看 status，于是**任何**一笔合法 consignment 都能结算掉被点名的
# receive，而 mark_settled 会把 consignment **实际交付的** asset/amount 写进那条 receive。
#
# 负例不额外消耗流动性：拿上面那笔已 settle 的 consignment（对应 receive R1 / 金额 A），
# 去结算一条**金额不同**的新 receive R2。修之前会成功并结掉 R2；现在必须被拒。
#
# 走 /sim/provide_consignment 而不是 /sim/user_pay_submit：后者**先广播**再结算
# （test_sim.rs 的 simulate_user_pay_submit），拿同一笔已上链的 psbt 再提交会死在广播上，
# 断言到的就不是这条校验了。
function scenario_rgb20_consignment_hint_mismatch() {
    log_step "scenario: a consignment must correspond to the receive it is hinted at (E16)"
    assert_non_empty "${RGB20_SETTLED_CONSIGNMENT}" \
        "E16 needs the settled consignment from scenario_rgb20_deposit (run it first)"
    assert_non_empty "${RGB20_SETTLED_RECEIVE_ID}" \
        "E16 needs the settled receive id from scenario_rgb20_deposit"

    # R2：资产相同、金额不同（+1 最小单位即足以判别）。
    local other_amount=$(( RGB20_DEPOSIT_AMOUNT + 1 ))
    local rec2 receive2
    rec2=$(gateway_deposit_invoice "rgb20-hint-mismatch" "${other_amount}") ||
        fail "E16 could not create the second receive through the gateway"
    receive2=$(echo "${rec2}" | jq -r '.receive_id // empty')
    assert_non_empty "${receive2}" "E16 could not create the second receive: ${rec2}"
    log_step "  R1=${RGB20_SETTLED_RECEIVE_ID} (settled, amount=${RGB20_DEPOSIT_AMOUNT}) -> R2=${receive2} (waiting, amount=${other_amount})"

    # 拿"对应 R1 的 consignment"去结算 R2。
    local out
    out=$(curl -s -X POST http://127.0.0.1:50064/sim/provide_consignment \
        -H 'Content-Type: application/json' \
        -d "{\"consignment\":\"${RGB20_SETTLED_CONSIGNMENT}\",\"receive_id\":\"${receive2}\"}")

    # 必须是"与 hint 指定的 receive 不对应"，而不是"找不到匹配的 pending receive" ——
    # 后者说明实现把 hint 丢在一边、退回了那条（生产里不执行的）搜索分支，等于校验没生效。
    if echo "${out}" | grep -q 'no pending receive matches'; then
        fail "E16: the hint was ignored and the code fell through to the un-hinted search — the asset/amount check did not run on the hinted receive (response: ${out})"
    fi
    if ! echo "${out}" | grep -q 'does not correspond to hinted receive'; then
        fail "E16: a consignment for ${RGB20_DEPOSIT_AMOUNT} settled a receive asking for ${other_amount} — expected a refusal naming the hinted receive, got: ${out}"
    fi
    echo "${out}" | grep -q "${receive2}" ||
        fail "E16: refusal does not name the hinted receive ${receive2}: ${out}"

    # 正对照：同一笔 consignment 配**它自己的** receive，应当幂等返回 settled。
    # 没有这一条，上面的"被拒"可能只是端点坏了或永远报错 —— 那样断言就没有判别力。
    local ok
    ok=$(curl -s -X POST http://127.0.0.1:50064/sim/provide_consignment \
        -H 'Content-Type: application/json' \
        -d "{\"consignment\":\"${RGB20_SETTLED_CONSIGNMENT}\",\"receive_id\":\"${RGB20_SETTLED_RECEIVE_ID}\"}")
    echo "${ok}" | grep -q '"status":"settled"' ||
        fail "E16 positive control failed: the same consignment with its own receive must settle idempotently, got: ${ok}"

    log_step "PASS: E16 consignment/receive mismatch refused (hint checked, not bypassed); positive control OK"
}

# =====================================================================
# RGB20 提现 E2E
# =====================================================================

# rgb20_withdraw_once <amount_min_units> <label>：跑一遍完整提现（chain33 withdraw → 侧车
# BuildWithdrawal → TSS 组签 → 广播 → 确认 → 链上 burn），断言 pending 清空 + 余额正确递减。
#
# 金额口径（两处**必须**对齐，否则桥的手续费校验会正确地拒掉它）：
#   - chain33 提现（burn）额 = amount；
#   - 用户发票金额 = amount − RGB20_WITHDRAW_FEE（桥核对"实际发给用户 == 链上 pending − 手续费"）。
# 用户实收 = amount − 手续费，链上余额减少 amount（burn ≠ 实收，是这条链路的正常形态）。
function rgb20_withdraw_once() {
    local amount="$1"
    local label="${2:-withdraw}"
    local before after amt withdraw_hash expected payout

    before=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    # 1. 侧车 test-sim 创建用户发票（提现收款方）：金额按手续费扣减后的应发额
    payout=$((amount - RGB20_WITHDRAW_FEE))
    assert_true "$(awk "BEGIN{print (${payout} > 0)?\"true\":\"false\"}")" \
        "rgb20 withdraw amount ${amount} does not cover the configured fee ${RGB20_WITHDRAW_FEE} (${label})"
    local user_invoice
    user_invoice=$(curl -s -X POST http://127.0.0.1:50064/sim/user_invoice \
        -H 'Content-Type: application/json' \
        -d "{\"asset_symbol\":\"${RGB20_SIDECAR_SYMBOL}\",\"amount\":${payout}}" | jq -r '.invoice // empty')
    assert_non_empty "${user_invoice}" "rgb20 user invoice empty (${label})"

    # 2. chain33 发起提现（destinationAddr=invoice）
    #    **没有 -p**：-a 的小数位由 CLI 从链上读（跨链资产读 crossChainInfo.precision —— 桥已经把
    #    核对过的合约精度回填在那，见 resolveWithdrawDecimals）。这里 amt 按 1e8 口径反算，正说明
    #    链上那份精度就是 8（若链上回填错，这里会因金额超精度而失败，而不是静默换一个金额）。
    amt=$(awk "BEGIN{printf \"%.8f\", ${amount}/100000000}")
    withdraw_hash=$(${MAIN_CLI} send rgbx withdraw -a "${amt}" -f 20 -d "${user_invoice}" -s "${RGB20_SYMBOL}" -k "${GENESIS_KEY}")
    assert_length "${withdraw_hash}" 66 "rgb20 withdraw tx hash (${label})"
    # 供调用方在 PASS 行里引用（本函数只打日志、不返回 txid）。
    # shellcheck disable=SC2034  # read by backup_test.sh (single-node restore PASS line)
    RGB20_LAST_WITHDRAW_HASH="${withdraw_hash}"

    # 3. 等桥确认销毁（本轮这笔 pending 清除 = rgbx Confirm 已提交）
    wait_no_withdraw_pending_for_user "${USER_MAIN_ADDR}" "${withdraw_hash}"

    # 4. 断言余额减少（-a 口径 *1e8 = min units，显示口径 /1e8）
    expected=$(awk "BEGIN{printf \"%.8f\", ${before} - ${amt}}")
    after=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    assert_balance "${after}" "${expected}" "rgb20 balance not decreased after withdraw (${label})"
    log_step "RGB20 withdraw OK (${label}): balance ${before} -> ${after} (amount=${amount} min units)"
}

function scenario_rgb20_withdraw() {
    log_step "scenario: RGB20 withdraw (chain33 withdraw -> sidecar BuildWithdrawal -> TSS signPsbt -> broadcast -> confirm -> burn)"
    local before
    before=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    assert_true "$(awk "BEGIN{print (${before} > 0)?\"true\":\"false\"}")" "rgb20 balance is zero before withdraw"
    rgb20_withdraw_once "${RGB20_WITHDRAW_AMOUNT}" "single-withdraw"
}

# =====================================================================
# RGB20 两连提现 E2E（回归 afcb7b934 / e538fc299 修复的性质）
# =====================================================================
#
# 第一笔提现会闭合当前 seal 并产生一个 change seal（余额回到桥的 TSS 地址）；第二笔提现必须
# 花掉这个 change seal。这条链路曾经必失败：
#   - 侧车侧：提现转移没 merge 进 Stock → build_transfer 报 "outpoint <txid>:<vout> has no state"（e538fc299 修）；
#   - Go 侧：change seal 只在 FinalizeWithdrawal 时被登记为 pending-mint，此后无路径提升 →
#     下一笔提现被 HR-5（"closed seal ... is pending-mint"）永久拒绝（afcb7b934 修）。
# 因此本场景同时校验"两笔都成功 burn + 余额递减 + listPendingTxByFrom 清空"与"第二笔的输入确实是
# 第一笔的 change seal"（缺任一修复时第二笔都会卡在 pending 直到超时）。

# 侧车账本视图（seal 生命周期的权威）：engine 每次状态变更后把账本落盘到 /data/ledger.json。
# change seal 的创建（finalize_withdrawal）与最终 Consumed（第二笔把它花掉）都能从这里读到，
# 这也是 Go 侧 refreshSealStatuses 对齐本地 SealIndex 的数据来源。
function rgb20_ledger_json() {
    compose_cmd exec -T rgb-sidecar cat /data/ledger.json 2>/dev/null
}

# 已登记的 seal outpoint 列表（排序，供 comm 求差集）。
function rgb20_ledger_seal_outpoints() {
    rgb20_ledger_json | jq -r '.seals | keys[]' | sort
}

# 读某个 seal 的字段（status / amount / ...）。
function rgb20_ledger_seal_field() {
    local outpoint="$1"
    local field="$2"
    rgb20_ledger_json | jq -r --arg o "${outpoint}" --arg f "${field}" '.seals[$o][$f] // empty'
}

# 侧车账本里未被消费的 seal 面额之和 = 本桥当前持有、可被提现花掉的 RGB 数量。
# 注意含 PendingMint：提现刚产生的 change seal 要等下一次 sync（下一笔提现的 build）才提升为
# Minted，此刻它已经可以参与下一笔提现，因此不能只看 minted（/sim/status 的 total_balance 会漏掉它）。
function query_rgb20_ledger_spendable() {
    rgb20_ledger_json | jq -r '[.seals[]? | select((.status | ascii_downcase) != "consumed") | .amount] | add // 0'
}

# BTC 交易所在高度（未确认返回 0）。
function btc_tx_block_height() {
    local txid="$1"
    local block_hash
    block_hash=$(${BTC_CTL} --"${BTC_NETWORK}" getrawtransaction "${txid}" 1 | jq -r '.blockhash // empty')
    if [ -z "${block_hash}" ]; then
        echo 0
        return 0
    fi
    ${BTC_CTL} --"${BTC_NETWORK}" getblockheader "${block_hash}" | jq -r '.height'
}

# 在 btcd 链上找首个花费 outpoint（"txid:vout"）的交易；找不到返回非 0。
function find_btc_spender_tx() {
    local outpoint="$1"
    local txid="${outpoint%%:*}"
    local vout="${outpoint##*:}"
    local start tip h block_hash t raw
    start=$(btc_tx_block_height "${txid}")
    tip=$(${BTC_CTL} --"${BTC_NETWORK}" getblockcount)
    for ((h = start; h <= tip; h++)); do
        block_hash=$(${BTC_CTL} --"${BTC_NETWORK}" getblockhash "${h}")
        for t in $(${BTC_CTL} --"${BTC_NETWORK}" getblock "${block_hash}" 1 | jq -r '.tx[]'); do
            raw=$(${BTC_CTL} --"${BTC_NETWORK}" getrawtransaction "${t}" 1)
            if echo "${raw}" | jq -e --arg t "${txid}" --argjson v "${vout}" \
                'any(.vin[]?; .txid == $t and .vout == $v)' >/dev/null 2>&1; then
                echo "${t}"
                return 0
            fi
        done
    done
    return 1
}

function scenario_rgb20_two_withdrawals() {
    log_step "scenario: RGB20 two consecutive withdrawals (second must spend the first's change seal)"

    # 余额沿用前序场景（deposit 充值 → withdraw 提现剩下的那部分）：本场景要连提两笔，
    # 合计不超过剩余额。判据用侧车账本的资产余额（= 桥侧可被提现花掉的 RGB 持仓），而不是
    # chain33 余额 —— 后者是用户已铸造的余额，和桥的持仓不是同一本账。
    local sidecar_before need_min_unit seals_before
    sidecar_before=$(query_rgb20_ledger_spendable)
    need_min_unit=$((RGB20_TWO_WITHDRAW_AMOUNT * 2))
    assert_true "$(awk "BEGIN{print (${sidecar_before} >= ${need_min_unit})?\"true\":\"false\"}")" \
        "rgb20 sidecar holdings too low for two withdrawals (deposit/withdraw scenarios must run first): have=${sidecar_before}, need=${need_min_unit}"
    seals_before=$(rgb20_ledger_seal_outpoints)

    # ---- 第一笔提现：产生 change seal ----
    rgb20_withdraw_once "${RGB20_TWO_WITHDRAW_AMOUNT}" "two-withdrawals #1"

    local seals_after1 change_seal
    seals_after1=$(rgb20_ledger_seal_outpoints)
    change_seal=$(comm -13 <(echo "${seals_before}") <(echo "${seals_after1}") | head -1)
    assert_non_empty "${change_seal}" "first withdrawal produced no change seal in sidecar ledger"
    assert_eq "$(comm -13 <(echo "${seals_before}") <(echo "${seals_after1}") | wc -l | tr -d ' ')" "1" \
        "expected exactly one new seal after the first withdrawal"
    # change seal 金额 = 提现前侧车持仓 - **应发额**（RGB 状态守恒：被花 seal 的面额 = 应发额 + 找零）。
    # 注意不是减提现额：提现额是链上 burn 掉的数，而真正离开桥的是发票金额 `提现额 − 手续费`，
    # 差额（手续费）作为找零留在桥手里 —— 找到这里正的差额就是"桥把手续费留下了"的直接证据。
    local expect_change_min_unit=$((sidecar_before - (RGB20_TWO_WITHDRAW_AMOUNT - RGB20_WITHDRAW_FEE)))
    assert_eq "$(rgb20_ledger_seal_field "${change_seal}" amount)" "${expect_change_min_unit}" \
        "change seal amount mismatch"
    log_step "  change seal of withdrawal #1: ${change_seal} (amount=${expect_change_min_unit})"

    # ---- 第二笔提现：必须花掉第一笔的 change seal ----
    rgb20_withdraw_once "${RGB20_TWO_WITHDRAW_AMOUNT}" "two-withdrawals #2"

    # 断言 1：侧车账本中该 change seal 最终为 Consumed（第二笔把它闭合了）
    assert_eq "$(rgb20_ledger_seal_field "${change_seal}" status | tr 'A-Z' 'a-z')" "consumed" \
        "change seal ${change_seal} is not consumed after the second withdrawal"

    # 断言 2：第二笔提现的 BTC 交易输入包含该 change seal，且它在首个输入位
    # （build_transfer 先列 RGB seal、后追加桥自有费输入；PSBT 输入即最终交易的输入）。
    local spender first_in
    if ! spender=$(find_btc_spender_tx "${change_seal}"); then
        fail "no BTC tx spends change seal ${change_seal} (second withdrawal did not use it)"
    fi
    first_in=$(${BTC_CTL} --"${BTC_NETWORK}" getrawtransaction "${spender}" 1 |
        jq -r '.vin[0].txid + ":" + (.vin[0].vout | tostring)')
    assert_eq "${first_in}" "${change_seal}" \
        "second withdrawal tx ${spender} does not spend the change seal as its first (RGB) input"

    log_step "RGB20 two withdrawals OK: ${change_seal} created by #1, spent by #2 (tx ${spender}), ledger=consumed"
}

# =====================================================================
# 易截断金额的提现（CLI 十进制 → 最小单位换算精确性回归）
# =====================================================================
#
# 旧实现用 int64(amount * math.Pow(10, 8)) 换算：float64 表示不了 0.0006，
# 0.0006 * 1e8 = 59999.99999999999 → 截断得 59999（1..2,000,000 最小单位中有 159879 个、
# 即 7.99% 会少 1）。链上 pending 因此比 invoice/放款意图少 1，签名节点的覆盖校验会正确地
# 以 "withdraw payout exceeds pending amount: payout=60000 pending=59999" 拒绝放款，而该
# pending 不属于不可恢复类别 —— 桥每秒重试刷屏，用户资产已 burn 却永不出款。
# 其余场景的金额（500000/200000）恰好落在安全区，覆盖不到这个雷区，故本场景用 60000。

function scenario_rgb20_truncation_withdraw() {
    log_step "scenario: RGB20 withdraw with truncation-prone amount (${RGB20_TRUNCATION_WITHDRAW_AMOUNT} min units)"

    local sidecar_before
    sidecar_before=$(query_rgb20_ledger_spendable)
    assert_true "$(awk "BEGIN{print (${sidecar_before} >= ${RGB20_TRUNCATION_WITHDRAW_AMOUNT})?\"true\":\"false\"}")" \
        "rgb20 sidecar holdings too low for truncation withdrawal: have=${sidecar_before}, need=${RGB20_TRUNCATION_WITHDRAW_AMOUNT}"

    local before after amt user_invoice withdraw_hash burn_amount expected started_at
    before=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    started_at=$(date +%s)

    # 1. 侧车 test-sim 发票：金额用精确的最小单位数（应发额 = burn − 配置手续费，见 rgb20_withdraw_once
    #    的注释：桥核对"实际发给用户 == 链上 pending − 手续费"，发票不扣手续费会被正确拒掉）
    user_invoice=$(curl -s -X POST http://127.0.0.1:50064/sim/user_invoice \
        -H 'Content-Type: application/json' \
        -d "{\"asset_symbol\":\"${RGB20_SIDECAR_SYMBOL}\",\"amount\":$((RGB20_TRUNCATION_WITHDRAW_AMOUNT - RGB20_WITHDRAW_FEE))}" | jq -r '.invoice // empty')
    assert_non_empty "${user_invoice}" "rgb20 truncation user invoice empty"

    # 2. chain33 发起提现（-a 口径 = min units / 1e8；旧实现下 ${RGB20_TRUNCATION_WITHDRAW_AMOUNT} 会少 1）
    #    没有 -p：精度从链上读（见 resolveWithdrawDecimals）。
    amt=$(awk "BEGIN{printf \"%.8f\", ${RGB20_TRUNCATION_WITHDRAW_AMOUNT}/100000000}")
    withdraw_hash=$(${MAIN_CLI} send rgbx withdraw -a "${amt}" -f 20 -d "${user_invoice}" -s "${RGB20_SYMBOL}" -k "${GENESIS_KEY}")
    assert_length "${withdraw_hash}" 66 "rgb20 truncation withdraw tx hash"

    # 3. 断言 burn 额（链上 pending）== invoice 额 + 手续费。invoice 是 burn 扣掉手续费后的应发额，
    #    浮点截断时 burn 会变成 59999（发票随之变成 59999 − 手续费），这里就变红。
    burn_amount=$(wait_pending_withdraw_amount "${USER_MAIN_ADDR}" "${withdraw_hash}")
    assert_non_empty "${burn_amount}" "rgb20 truncation withdraw pending not found (${withdraw_hash})"
    assert_eq "${burn_amount}" "${RGB20_TRUNCATION_WITHDRAW_AMOUNT}" \
        "chain33 burn amount != invoice amount + withdrawFee (CLI decimal->min unit conversion lost units)"

    # 4. 正常完成：pending 清除 + 余额精确递减。金额不一致时桥永远不放款，这里会超时。
    wait_no_withdraw_pending_for_user "${USER_MAIN_ADDR}" "${withdraw_hash}"
    expected=$(awk "BEGIN{printf \"%.8f\", ${before} - ${amt}}")
    after=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    assert_balance "${after}" "${expected}" "rgb20 balance not decreased after truncation withdraw"

    # 5. 本轮日志：不许出现覆盖校验失败，也不许出现每秒重试刷屏
    local logs retries
    logs=$(para1_logs_since "${started_at}")
    if echo "${logs}" | grep -q "payout exceeds pending amount"; then
        fail "rgb20 truncation withdraw rejected by coverage check: $(echo "${logs}" | grep -m1 'payout exceeds pending amount')"
    fi
    retries=$(echo "${logs}" | grep -c "withdrawalProcessor rgb20 retry" || true)
    if [ "${retries}" -ge 10 ]; then
        fail "rgb20 truncation withdraw retried ${retries} times (spam); burn/payout amount mismatch?"
    fi

    log_step "RGB20 truncation withdraw OK: burn == ${RGB20_TRUNCATION_WITHDRAW_AMOUNT} == invoice ($((RGB20_TRUNCATION_WITHDRAW_AMOUNT - RGB20_WITHDRAW_FEE))) + fee (${RGB20_WITHDRAW_FEE}) min units (retry logs=${retries})"
}

# =====================================================================
# 提现手续费：发票金额与「pending − 手续费」对不上必须被拒（E-fee 负例）
# =====================================================================
#
# 桥按资产收一笔固定的提现手续费（桥侧配置 `rgb20.contracts[].withdrawFee`，见 CONFIG.md §4.4.5），
# 判据是「**实际发给用户**的资产精确等于 `链上 pending − 手续费`」。本场景构造**发票没扣手续费**
# 这个真实形态（前端没跟上配置时就是这样，也正是"桥替用户垫 BTC 矿工费"那个 bug 的形态），断言：
#
#   1. 桥给出明确的拒绝，信息里带期望值、实际值与差额（便于用户/前端当场看出该开多少）；
#   2. 没有签名、也没有广播 —— 直接证据是侧车账本**没有新增 seal**（change seal 由
#      FinalizeWithdrawal 创建，那是签名之后的一步）+ chain33 上这笔 pending 一直挂着；
#   3. 于是这笔 burn 停在链上（资产在用户提交提现时就已被 Exec_Withdraw 转进**锁定地址**，桥不做
#      自动退款；怎么处置见 WITHDRAW_LOCK_MODEL.md 的解锁路径）。
#
# ⚠️ 本场景**故意**留下一个永久 pending 并锁住用户的资产，所以它必须排在 `run_rgb20_all` 的**最后**
# （之后的场景要么依赖余额、要么依赖"本轮只认自己那笔 pending"）。这与 harness 的既有前提一致：
# chain33 对提现没有取消/超时路径，失败遗留的 pending 会永远留在列表里，等待函数都按 tx_hash
# 认领自己那一笔（见 wait_no_withdraw_pending_for_user）。
#
# 顺带说明判据的边界（别把它读成"桥什么都没做"）：桥**会**调用一次 BuildWithdrawal —— 侧车不把
# 解析出的发票金额回传给桥（BuildWithdrawalResponse 只有 psbt/input_amounts/consignment），
# 所以金额只能从"构建出来的转移"里核对。拒绝点因此在构建之后、**签名与广播之前**。
function scenario_rgb20_withdraw_fee_mismatch() {
    log_step "scenario: a withdrawal whose invoice does not deduct the fee must be refused (E-fee negative case)"

    # The fee is the on-chain governance value (rgb20WithdrawFee), not a bridge-local knob.
    # This harness does not run the autonomy proposal/vote flow yet, so unless the fee was
    # seeded by other means the chain value is 0 and the bridge deliberately skips the exact
    # payout check. Skip instead of failing on an unseeded precondition.
    local onchain_fee
    onchain_fee=$(rgbx_query "GetBridgeConfig" "{\"data\":\"${RGB20_SYMBOL}\"}" |
        jq -r '[.result.rgb20WithdrawFee[]? | select((.symbol | ascii_upcase) == "'"${RGB20_SYMBOL}"'") | .feeMinUnits] | first // 0' 2>/dev/null)
    if [ -z "${onchain_fee}" ] || [ "${onchain_fee}" -le 0 ] 2>/dev/null; then
        log_step "SKIP: on-chain rgb20WithdrawFee is ${onchain_fee:-0} (governance not seeded); the fee-deduction guard cannot be exercised"
        return 0
    fi

    local amount="${RGB20_FEE_MISMATCH_WITHDRAW_AMOUNT}"
    local expected_payout=$((amount - onchain_fee))

    # 前置条件：桥侧（侧车账本）要拿得出这笔提现，否则失败原因会变成"余额不足"，判据就没走到。
    local sidecar_before before amount_decimal
    sidecar_before=$(query_rgb20_ledger_spendable)
    assert_true "$(awk "BEGIN{print (${sidecar_before} >= ${amount})?\"true\":\"false\"}")" \
        "rgb20 sidecar holdings too low for the fee-mismatch case: have=${sidecar_before}, need=${amount}"
    before=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    amount_decimal=$(awk "BEGIN{printf \"%.8f\", ${amount}/100000000}")
    assert_true "$(awk "BEGIN{print (${before} >= ${amount_decimal})?\"true\":\"false\"}")" \
        "rgb20 user balance too low for the fee-mismatch case: have=${before}, need=${amount_decimal}"

    # 1. 发票按**完整提现额**开（手续费没扣）：桥应发的是 ${expected_payout}，实际会发 ${amount}。
    local user_invoice
    user_invoice=$(curl -s -X POST http://127.0.0.1:50064/sim/user_invoice \
        -H 'Content-Type: application/json' \
        -d "{\"asset_symbol\":\"${RGB20_SIDECAR_SYMBOL}\",\"amount\":${amount}}" | jq -r '.invoice // empty')
    assert_non_empty "${user_invoice}" "rgb20 fee-mismatch user invoice empty"

    # 2. chain33 发起提现（链上 burn = amount，锁定地址在此刻收到资产）
    local seals_before started_at withdraw_hash want_hash
    seals_before=$(rgb20_ledger_seal_outpoints)
    # 读不到账本就无从比对"没新增 seal"（两边都空会让下面那条断言变成空转），故先判非空。
    assert_non_empty "${seals_before}" "rgb20 ledger has no seals before the fee-mismatch case (cannot tell 'no new seal' apart from 'cannot read the ledger')"
    started_at=$(date +%s)
    withdraw_hash=$(${MAIN_CLI} send rgbx withdraw -a "${amount_decimal}" -f 20 -d "${user_invoice}" -s "${RGB20_SYMBOL}" -k "${GENESIS_KEY}")
    assert_length "${withdraw_hash}" 66 "rgb20 fee-mismatch withdraw tx hash"
    want_hash=$(echo "${withdraw_hash}" | tr 'A-Z' 'a-z')

    local burn_amount
    burn_amount=$(wait_pending_withdraw_amount "${USER_MAIN_ADDR}" "${withdraw_hash}")
    assert_eq "${burn_amount}" "${amount}" "chain33 burn amount (fee-mismatch case)"

    # 3. 桥必须给出明确拒绝。等它出现：pending 由桥每 10s 轮询一次，之后每次重试都会打这条
    #    （CI 的重试节奏是秒级），60s 足够；超时判失败并打印日志尾部。
    local i logs reason=""
    for ((i = 0; i < 60; i++)); do
        logs=$(para1_logs_since "${started_at}")
        if echo "${logs}" | grep -q "withdraw payout mismatch"; then
            reason=$(echo "${logs}" | grep -m1 "withdraw payout mismatch")
            break
        fi
        sleep 1
    done
    if [ -z "${reason}" ]; then
        fail "发票没扣手续费的提现没有被桥拒绝（未出现 'withdraw payout mismatch'）：para1 本轮日志尾部=$(para1_logs_since "${started_at}" | tail -5)"
    fi
    log_step "  bridge refused it: ${reason}"

    # 3.1 错误信息必须说清期望多少、实际多少、差在哪（用户/前端据此知道发票该开多少）。
    #     片段按 `WithdrawPayoutMismatchError.Error()` 的实际措辞取：
    #     "this transfer pays the user <实发> but the configured withdrawal fee <手续费> requires
    #      exactly <应发> out of the <链上 pending> chain33 pending amount (off by +<差额>; ...)"
    for want in \
        "pays the user ${amount}" \
        "requires exactly ${expected_payout}" \
        "out of the ${amount} chain33 pending amount" \
        "off by +${onchain_fee}"; do
        echo "${reason}" | grep -qF "${want}" ||
            fail "拒绝信息没有说清金额（缺 '${want}'）：${reason}"
    done

    # 4. 拒绝必须早于签名与广播。直接证据：侧车账本没有新增 seal（change seal 只在 FinalizeWithdrawal
    #    ——即签名之后——被创建），chain33 上这笔 pending 一直挂着（真广播并被确认的话它会像其它场景
    #    那样被清掉），且 para1 日志里没有这笔的广播记录。
    local seals_after pending_now
    sleep 5
    logs=$(para1_logs_since "${started_at}")
    seals_after=$(rgb20_ledger_seal_outpoints)
    assert_eq "${seals_after}" "${seals_before}" \
        "被拒的提现不得产生新 seal（新 seal 意味着走到了 FinalizeWithdrawal，即签名之后）"
    # pending 这条只是佐证（几秒内它本来也来不及走完"广播→BTC 确认→Confirm"三步），
    # 上面两条才是"没签名、没广播"的直接证据。
    pending_now=$(query_pending_withdraw_amount "${USER_MAIN_ADDR}" "${withdraw_hash}")
    assert_eq "${pending_now}" "${amount}" \
        "被拒的提现必须仍在链上 pending（佐证：真广播并确认的话它会像其它场景那样被清掉）"
    if echo "${logs}" | grep "withdraw tracked" | grep -q "${want_hash}"; then
        fail "被拒的提现被广播了：$(echo "${logs}" | grep "${want_hash}" | head -3)"
    fi

    # 5. 用户余额：提交提现时链上就把金额锁走了（余额已经减少），被拒不会让它回来。
    local after
    after=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    assert_balance "${after}" "$(awk "BEGIN{printf \"%.8f\", ${before} - ${amount_decimal}}")" \
        "被拒的提现：资产已锁进锁定地址（余额减少），桥不做自动退款"

    log_step "PASS: E-fee 发票未扣手续费的提现被拒（期望 ${expected_payout}、实际 ${amount}、额差 +${onchain_fee}），未签名未广播；链上那笔 burn 停在锁定地址（解锁路径见 WITHDRAW_LOCK_MODEL.md）"
}

# =====================================================================
# #70 unlock E2E: permanently rejected withdrawal -> TSS group signature -> on-chain release
# =====================================================================
#
# Full chain under test: the bridge classifies a withdrawal as "retry can never succeed", the TSS
# group signs the Unlock message, chain33 refunds the locked amount, the user's balance returns,
# and the same burn can never be settled afterwards (the double-payment guard).
#
# How the permanent failure is constructed (deterministic, no source changes):
#   1. create a user invoice against the current sidecar ledger (contract A1);
#   2. rebuild the sidecar ledger and re-issue the asset (contract A2). The asset id is derived
#      from the genesis seal's random blinding factor, so re-issuing always changes it;
#   3. submit a withdraw using the A1 invoice. BuildWithdrawal parses the invoice, compares it
#      with the ledger's contract (A2) and returns FailedPrecondition
#      "invoice contract ... != asset contract ..." => classifyWithdrawSidecarError =>
#      asset-contract-mismatch => handleUnrecoverableRgb20Withdraw => Unlock.
#
# The sidecar ledger is snapshotted and restored around this scenario: later scenarios expect
# the original contract (and the backup drill would otherwise capture a ledger that no longer
# matches the configured contracts.assetId). This scenario must therefore run BEFORE
# scenario_rgb20_withdraw_fee_mismatch, which is the one that intentionally leaves a pending.
function scenario_rgb20_unlock_after_permanent_rejection() {
    log_step "scenario: permanently rejected withdrawal -> TSS unlock -> on-chain released -> refund (#70)"

    local amount="${RGB20_UNLOCK_WITHDRAW_AMOUNT}"
    local payout=$((amount - RGB20_WITHDRAW_FEE))
    assert_true "$(awk "BEGIN{print (${RGB20_WITHDRAW_FEE} > 0)?\"true\":\"false\"}")" \
        "this scenario needs a non-zero RGB20_WITHDRAW_FEE (the invoice must deduct it)"
    assert_true "$(awk "BEGIN{print (${payout} > 0)?\"true\":\"false\"}")" \
        "rgb20 unlock amount ${amount} does not cover the configured fee ${RGB20_WITHDRAW_FEE}"

    local before amount_decimal lock_addr lock_before
    before=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    amount_decimal=$(awk "BEGIN{printf \"%.8f\", ${amount}/100000000}")
    assert_true "$(awk "BEGIN{print (${before} >= ${amount_decimal})?\"true\":\"false\"}")" \
        "rgb20 user balance too low for the unlock case: have=${before}, need=${amount_decimal}"
    lock_addr=$(query_rgb20_lock_address)
    assert_non_empty "${lock_addr}" \
        "cannot read the cross-chain lock address (Query_GetCrossChainLockAddress)"
    lock_before=$(query_rgb20_balance "${lock_addr}")
    log_step "  pre-state: user=${before}, lockAddr=${lock_addr} locked=${lock_before}, " \
        "withdraw=${amount} (payout=${payout})"

    # 1. Invoice against the CURRENT contract (A1). It will name a contract the rebuilt ledger no
    #    longer has.
    local user_invoice old_asset_id
    user_invoice=$(curl -s -X POST http://127.0.0.1:50064/sim/user_invoice \
        -H 'Content-Type: application/json' \
        -d "{\"asset_symbol\":\"${RGB20_SIDECAR_SYMBOL}\",\"amount\":${payout}}" | jq -r '.invoice // empty')
    assert_non_empty "${user_invoice}" "rgb20 unlock user invoice empty"
    old_asset_id="${RGB20_ASSET_ID}"
    assert_non_empty "${old_asset_id}" \
        "RGB20_ASSET_ID is empty; the mismatch this scenario constructs cannot be verified"

    # 2. Snapshot the sidecar data dir (bind-mounted host dir: the data itself lives in a volume,
    #    so docker cp into a stopped container would be shadowed by the volume mount).
    local backup_dir
    backup_dir=$(mktemp -d)
    compose_cmd run --rm --no-deps -v "${backup_dir}":/backup rgb-sidecar \
        sh -c 'tar -C /data -cf /backup/data.tar .' >/dev/null 2>&1 ||
        fail "cannot snapshot the rgb-sidecar data dir"
    assert_non_empty "$(ls -A "${backup_dir}" 2>/dev/null || true)" \
        "rgb-sidecar data snapshot is empty"

    # 3. Rebuild the ledger with a freshly issued asset (A2 != A1).
    compose_cmd stop rgb-sidecar >/dev/null 2>&1 || true
    compose_cmd run --rm --no-deps rgb-sidecar \
        sh -c 'rm -rf /data/* /data/.[!.]* 2>/dev/null || true' >/dev/null 2>&1 || true
    local issue_out new_asset_id
    issue_out=$(compose_cmd run --rm --no-deps \
        -e RGB_SIDECAR_TSS_PUBKEY="${RGB20_TSS_PUBKEY}" \
        -e RGB_BITCOIND_RPC="${RGB20_BITCOIND_RPC}" \
        -e RGB_BITCOIND_USER="${RGB20_BITCOIND_USER}" \
        -e RGB_BITCOIND_PASS="${RGB20_BITCOIND_PASS}" \
        -e RGB_BITCOIND_CERT="${RGB20_BITCOIND_CERT}" \
        -e BTC_FUNDING_WIF="${BTC_FUNDING_WIF:-}" \
        rgb-sidecar /sidecar/issue_usdt 2>&1)
    echo "${issue_out}" | tail -10
    echo "${issue_out}" | grep -q "ISSUE-DONE" || fail "unlock case: issue_usdt did not complete"
    new_asset_id=$(echo "${issue_out}" | sed -n 's/^issued [^ ]* asset_id=\(.*\)$/\1/p' | head -1 | tr -d '\r')
    assert_non_empty "${new_asset_id}" "unlock case: cannot read the re-issued asset id"
    assert_true "$([ "${new_asset_id}" != "${old_asset_id}" ] && echo true || echo false)" \
        "re-issuing produced the same asset id (${new_asset_id}); the invoice would NOT mismatch and the permanent-failure path would not run"
    log_step "  sidecar ledger rebuilt: asset ${old_asset_id} -> ${new_asset_id}"
    compose_cmd up -d rgb-sidecar >/dev/null
    wait_rgb20_sidecar_grpc

    # 4. Submit the withdraw paying the A1 invoice.
    local started_at withdraw_hash burn_hash burn_amount
    started_at=$(date +%s)
    withdraw_hash=$(${MAIN_CLI} send rgbx withdraw -a "${amount_decimal}" -f 20 -d "${user_invoice}" \
        -s "${RGB20_SYMBOL}" -k "${GENESIS_KEY}")
    assert_length "${withdraw_hash}" 66 "rgb20 unlock withdraw tx hash"
    # The CLI prints "0x"-prefixed hashes; the node logs (hex.EncodeToString) and the query payload
    # use the bare 64-hex form. Normalise once so every comparison uses the same spelling.
    burn_hash=$(echo "${withdraw_hash}" | tr 'A-Z' 'a-z' | sed 's/^0x//')
    burn_amount=$(wait_pending_withdraw_amount "${USER_MAIN_ADDR}" "${withdraw_hash}")
    assert_eq "${burn_amount}" "${amount}" "chain33 burn amount (unlock case)"

    # The withdraw already moved the amount into the shared lock address.
    assert_balance "$(query_rgb20_balance "${USER_MAIN_ADDR}")" \
        "$(awk "BEGIN{printf \"%.8f\", ${before} - ${amount_decimal}}")" \
        "unlock case: the amount must leave the user's balance when the withdraw locks it"
    assert_balance "$(query_rgb20_balance "${lock_addr}")" \
        "$(awk "BEGIN{printf \"%.8f\", ${lock_before} + ${amount_decimal}}")" \
        "unlock case: the amount must arrive at the lock address"

    # 5. The bridge must classify it as unrecoverable and request the unlock (before any payment
    #    signature exists). The sidecar is restarted at this point, so allow time for the bridge's
    #    gRPC connection to come back and for the 10s pending poll.
    # Strip ANSI colour codes: logrus colours field names, so "class=..." would otherwise be seen
    # as "class<ESC>[0m=..." and never match.
    local i logs reason=""
    for ((i = 0; i < 150; i++)); do
        logs=$(para1_logs_since "${started_at}" | perl -pe 's/\e\[[0-9;]*m//g')
        if echo "${logs}" | grep -F "requesting unlock (refund)" | grep -q "${burn_hash}"; then
            reason=$(echo "${logs}" | grep -F "requesting unlock (refund)" | grep -m1 "${burn_hash}")
            break
        fi
        sleep 1
    done
    if [ -z "${reason}" ]; then
        fail "rgb20 unlock: the bridge did not request an unlock for ${burn_hash}. para1 log tail: $(para1_logs_since "${started_at}" | tail -5)"
    fi
    echo "${reason}" | grep -q "class=asset-contract-mismatch" ||
        fail "rgb20 unlock: expected class=asset-contract-mismatch, got: ${reason}"
    log_step "  bridge requested unlock: ${reason}"

    # 6. On-chain terminal state: released (consensus key, not a bridge self-report).
    local final_state="" state_retries=180
    for ((i = 0; i < state_retries; i++)); do
        final_state=$(query_withdraw_final_state "${burn_hash}")
        [ "${final_state}" = "released" ] && break
        sleep 1
    done
    assert_eq "${final_state}" "released" \
        "rgb20 unlock: on-chain terminal state must be released (TSS group signature + Exec_Unlock)"

    # 7. Refund reached the user and the lock address gave the amount back.
    local refunded
    refunded=$(wait_rgb20_balance_equal "${USER_MAIN_ADDR}" "${before}") ||
        fail "rgb20 unlock: user balance did not return to pre-withdraw ${before}, got ${refunded}"
    assert_balance "$(query_rgb20_balance "${lock_addr}")" "${lock_before}" \
        "rgb20 unlock: the lock address must drop back to its pre-withdraw balance"
    log_step "  refunded: user=${refunded} (pre-withdraw ${before}), locked=${lock_before}"

    # 8. The same burn can never be settled now: Confirm is rejected by the terminal-state key.
    #    The CLI exits 0 even when CheckTx rejects (see scenario_duplicate_confirm_rejected), so
    #    the assertions are on its output and on the node's own diagnostic.
    local details height index replay_started_at replay_out replay_rc
    details=$(${MAIN_CLI} tx query -s "${burn_hash}" 2>/dev/null || true)
    height=$(echo "${details}" | jq -r '.height // 0' 2>/dev/null || echo 0)
    index=$(echo "${details}" | jq -r '.index // 0' 2>/dev/null || echo 0)
    if ! [[ "${height}" =~ ^[0-9]+$ ]] || [ "${height}" -le 0 ]; then
        fail "rgb20 unlock: cannot locate burn ${burn_hash} on chain (tx query: $(echo "${details}" | head -c 200))"
    fi
    replay_started_at=$(date +%s)
    set +e
    replay_out=$(replay_confirm_bounded 106 "${height}" "${index}" "${burn_hash}" 30 2>&1)
    replay_rc=$?
    set -e
    log_step "  replay confirm after release: rc=${replay_rc} (diagnostic only), out=$(echo "${replay_out}" | head -c 300)"
    echo "${replay_out}" | grep -q "withdraw already confirmed" ||
        fail "rgb20 unlock: settling a released burn was not rejected with the terminal-state error. out=$(echo "${replay_out}" | head -c 300)"
    logs=$(main_logs_since "${replay_started_at}" | perl -pe 's/\e\[[0-9;]*m//g')
    echo "${logs}" | grep -q "checkWithdrawConfirm burn already used" ||
        fail "rgb20 unlock: main node did not run the terminal-state guard on the Confirm replay"

    # 9. Exactly one unlock, no duplicate signing/retry of the same burn, and no payment broadcast.
    local unlock_count request_count retry_count
    unlock_count=$(main_logs_since "${started_at}" | perl -pe 's/\e\[[0-9;]*m//g' |
        grep "Exec_Unlock released" | grep -c "${burn_hash}" || true)
    assert_eq "${unlock_count}" "1" \
        "rgb20 unlock: exactly one on-chain release expected for ${burn_hash}"
    logs=$(para1_logs_since "${started_at}" | perl -pe 's/\e\[[0-9;]*m//g')
    request_count=$(echo "${logs}" | grep -F "requesting unlock (refund)" | grep -c "${burn_hash}" || true)
    assert_eq "${request_count}" "1" \
        "rgb20 unlock: exactly one unlock request expected for ${burn_hash}"
    retry_count=$(echo "${logs}" | grep -c -e "unlock failed, will retry.*${burn_hash}" \
        -e "withdrawalProcessor retry unlock.*${burn_hash}" || true)
    assert_eq "${retry_count}" "0" \
        "rgb20 unlock: unlock was retried ${retry_count} time(s) for ${burn_hash} (duplicate signing/submission)"
    if echo "${logs}" | grep "BroadcastTx rgb20 withdraw" | grep -q "${burn_hash}"; then
        fail "rgb20 unlock: a payment was broadcast for ${burn_hash}, but unlock must only happen before any payment signature"
    fi

    # 10. Restore the original sidecar ledger so later scenarios (and the backup drill) see the
    #     contract configured in contracts.assetId.
    compose_cmd stop rgb-sidecar >/dev/null 2>&1 || true
    compose_cmd run --rm --no-deps -v "${backup_dir}":/backup rgb-sidecar \
        sh -c 'rm -rf /data/* /data/.[!.]* 2>/dev/null || true; tar -C /data -xf /backup/data.tar' \
        >/dev/null 2>&1 || fail "rgb20 unlock: cannot restore the rgb-sidecar data dir"
    compose_cmd up -d rgb-sidecar >/dev/null
    wait_rgb20_sidecar_grpc
    rm -rf "${backup_dir}"
    # The ledger's asset map is keyed by asset id, so look the entry up by its symbol field.
    local restored_id
    restored_id=$(rgb20_ledger_json |
        jq -r --arg s "${RGB20_SIDECAR_SYMBOL}" '[.assets[]? | select(.symbol == $s) | .asset_id] | first // empty')
    assert_eq "${restored_id}" "${old_asset_id}" \
        "rgb20 unlock: sidecar ledger restore did not bring back the original contract"
    log_step "  sidecar ledger restored: asset=${restored_id}"

    log_step "PASS: #70 permanently rejected withdraw released on chain (burn=${burn_hash}), balance ${before} -> ${refunded}, lock ${lock_before} -> ${lock_before}, Confirm rejected"
}

# =====================================================================
# Go↔Rust 互操作 smoke（compose 内暴露的侧车 50061）
# =====================================================================

function run_rgb20_sidecar_smoke() {
    log_step "RGB20 sidecar smoke: Go rgb20.SidecarClient <-> compose 内侧车"
    local repo_root
    repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)
    # 在宿主跑 rgb20 包 sidecar_live 测试（连 compose 暴露的 127.0.0.1:50061）
    (
        cd "${repo_root}"
        RGB_SIDECAR_ADDR=127.0.0.1:50061 \
            go test ./plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/ -run Test_SidecarLive_RoundTrip -v 2>&1 | tail -20
    ) || fail "rgb20 sidecar smoke failed"
    log_step "RGB20 sidecar smoke OK"
}

# =====================================================================
# 纯 genesis seal 提现探针（A2 回归）
# =====================================================================

# 必须在 run_rgb20_env 之后、任何充值之前跑：此刻侧车账本只有一枚 genesis seal，提现的锚定
# tx 尚未广播（侧车只能从本地 anchor 缓存解析它）。若 recipient_amount 只从 btcd 解析，这里
# 会得到 0，Go 桥的 ValidateWithdrawPsbt 即以 "withdraw amount exceeds sealed balance:
# consignment=0 expected=…" 拒绝 —— 即"跳过充值直接提现必失败"的根因。
# 重复 run（账本保留、已有链上历史锚）时探针仍会通过，作为回归保护。
function run_rgb20_sidecar_genesis_withdraw_probe() {
    log_step "RGB20 genesis-only withdrawal probe: consignment amount must be resolved from the local anchor"
    local user_invoice tss_address
    user_invoice=$(curl -s -X POST http://127.0.0.1:50064/sim/user_invoice \
        -H 'Content-Type: application/json' \
        -d "{\"asset_symbol\":\"${RGB20_SIDECAR_SYMBOL}\",\"amount\":${RGB20_WITHDRAW_AMOUNT}}" | jq -r '.invoice // empty')
    assert_non_empty "${user_invoice}" "rgb20 genesis probe user invoice empty"
    tss_address=$(${MAIN_CLI} rgbx getCrossChainInfo -s "${RGB20_SYMBOL}" | jq -r '.tssAddress // empty')
    assert_non_empty "${tss_address}" "rgb20 genesis probe tss address empty"

    local repo_root
    repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)
    (
        cd "${repo_root}"
        RGB_SIDECAR_ADDR=127.0.0.1:50061 \
        RGB_SIDECAR_ASSET_SYMBOL="${RGB20_SIDECAR_SYMBOL}" \
        RGB_SIDECAR_USER_INVOICE="${user_invoice}" \
        RGB_SIDECAR_TSS_ADDRESS="${tss_address}" \
        RGB_SIDECAR_WITHDRAW_AMOUNT="${RGB20_WITHDRAW_AMOUNT}" \
            go test ./plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/ \
            -run Test_SidecarLive_GenesisOnlyWithdrawal -v 2>&1 | tail -20
    ) || fail "rgb20 genesis-only withdrawal probe failed"
    log_step "RGB20 genesis-only withdrawal probe OK"
}

# =====================================================================
# E14 主动负例：同一笔确认提交两次必须被去重判据拒绝
# =====================================================================
#
# 为什么需要它：E14 的护栏（stateDB 的 formatConfirmUsedKey + checkConfirm 里的
# checkConfirmNotUsed）在整个 E2E 里**从来没被触发过** —— 所有场景都只提交过一次确认，
# 所以"全绿"证明不了这条护栏有效。这里主动构造负例：拿一笔链上已经确认结算过的 mint，
# 用 `chain33-cli rgbx confirm` 把**同一笔确认再提交一次**，断言它被拒绝、且护栏自己的
# 诊断日志出现在主链日志里。
#
# 诚实边界（重要，别把本场景读成"stateDB 键单独被验证了"）：
#   checkConfirm 里有**两层**去重，localdb 的 pendingTx.Confirmed（ExecLocal_Confirm 写的
#   纵深防御层）排在 stateDB 键之前。健康节点上 localdb 一定是新鲜的，所以重复确认会先被
#   第一层拦下，stateDB 键那一层**在线上不可达**（要它可达必须是"localdb 陈旧"，而那正是
#   E14 的真实故障前提，E2E 无法构造）。因此：
#     - 本场景证明的是"重复确认被去重机制拒绝 + 去重护栏在活跃路径上真的执行了"；
#     - E14 的 stateDB 键本身由 executor 的单测 confirm_dedup_test.go /
#       confirm_dedup_failclosed_test.go 覆盖（那里的 fixture 刻意把 localdb 留成陈旧态，
#       并且断言错误码是 ErrMintAlreadyConfirmed 而不是第一层的 ErrTxAlreadyConfirmed）。
#   - 本场景在"两层去重整体被拿掉"时会变红：那时重复确认会走到证明校验、报另一个错、
#     主链日志里不会出现去重诊断。
#
# 重复确认的 proof 字段故意留空：checkConfirmNotUsed 排在 validateBtcTxProof **之前**，
# 被拦下时根本走不到证明校验；反过来若它真被放行，空证明会以 ErrInvalidBtcTxProof 失败，
# 而输出里不会有去重诊断 —— 正是我们要判成失败的那种情形。
#
# ⚠️ 判据为什么**不能**用退出码（2026-09-19 主会话在跑起来的 E2E 上实测，勿"修"回去）：
#   `chain33-cli send rgbx confirm` 在这条路上**恒定返回 0**，即使交易被 CheckTx 拒绝。
#   原因是 CLI 的实现（chain33 的 system/dapp/commands/send.go oneStepSend）：内层
#   `wallet send` 失败时它只是 `fmt.Fprintln(os.Stderr, err)` 然后 return，**不设置退出码**；
#   而内层错误文本（含链上回的错误 `tx already confirmed`）会出现在 stderr 上。
#   实测：
#     $ docker exec <main> ... send rgbx confirm --actionType 101 --txBlockHeight 106 --txIndex 0 \
#           --txHash 1d2cd556... -k 0x6da92a...
#       stdout = []   stderr = ["exit status 1", "tx already confirmed"]   rc = 0
#     容器内不经 docker exec 再测一次同样 rc=0（排除 docker exec 的问题）：
#     $ docker exec <main> /bin/sh -c 'exit 7'; echo $?  →  7（docker exec 是透传退出码的）
#   ⇒ 判据只能是 **CLI 自己的输出里带护栏的拒绝诊断**（见下面第 3 条），rc 仅作诊断信息。

# 重复提交一笔已确认的 Confirm，打印 CLI 输出并把退出码作为返回值（**rc 仅供诊断**：
# 如上所述 CLI 被拒时也返回 0，任何断言都不得以 rc 为准）。
# 自带超时：CheckTx 被拒时 CLI 未必立刻返回（在等回执），绝不能挂住整个 E2E。
function replay_confirm_bounded() {
    local action_type="$1"
    local tx_block_height="$2"
    local tx_index="$3"
    local tx_hash_hex="$4"
    local timeout_sec="${5:-30}"

    local out_file rc_file i rc
    out_file=$(mktemp)
    rc_file=$(mktemp)
    (
        set +e
        ${MAIN_CLI} send rgbx confirm \
            --actionType "${action_type}" \
            --txBlockHeight "${tx_block_height}" \
            --txIndex "${tx_index}" \
            --txHash "${tx_hash_hex}" \
            -k "${AUTH_KEY1}" >"${out_file}" 2>&1
        echo "$?" >"${rc_file}"
    ) &
    local pid=$!

    for ((i = 0; i < timeout_sec; i++)); do
        [ -s "${rc_file}" ] && break
        sleep 1
    done

    if [ -s "${rc_file}" ]; then
        cat "${out_file}"
        rc=$(cat "${rc_file}" 2>/dev/null || echo 1)
        rm -f "${out_file}" "${rc_file}"
        return "${rc}"
    fi

    kill "${pid}" 2>/dev/null || true
    log_step "  (replay confirm CLI 未在 ${timeout_sec}s 内返回，已 kill；判据交给主链日志里的去重诊断)"
    echo "TIMEOUT: replay confirm CLI did not return within ${timeout_sec}s"
    rm -f "${out_file}" "${rc_file}"
    return 1
}

# scenario_duplicate_confirm_rejected <已经确认结算过的 chain33 tx hash>
function scenario_duplicate_confirm_rejected() {
    local mint_hash="$1"
    log_step "scenario: duplicate confirm must be rejected by the dedup guard (E14 negative case)"

    mint_hash="${mint_hash#0x}"
    assert_non_empty "${mint_hash}" "duplicate-confirm: mint tx hash empty"

    # 1. 这笔 mint 在链上的位置 = 它的 pending 键（ExecLocal_Mint 按 (height, index) 登记）
    local details height index
    details=$(${MAIN_CLI} tx query -s "${mint_hash}" 2>/dev/null || true)
    height=$(echo "${details}" | jq -r '.height // 0' 2>/dev/null || echo 0)
    index=$(echo "${details}" | jq -r '.index // 0' 2>/dev/null || echo 0)
    if ! [[ "${height}" =~ ^[0-9]+$ ]] || [ "${height}" -le 0 ]; then
        fail "duplicate-confirm: 找不到 mint 交易 ${mint_hash} 的落块高度（tx query 返回：$(echo "${details}" | head -c 200)）"
    fi
    log_step "  mint tx ${mint_hash} at height=${height} index=${index}"

    local minted_before
    minted_before=$(${MAIN_CLI} rgbx getAsset -s NATIVE1 | jq -r '.totalAmount // 0')
    assert_non_empty "${minted_before}" "duplicate-confirm: cannot read NATIVE1 supply before replay"

    local replay_started_at replay_out replay_rc
    replay_started_at=$(date +%s)
    set +e
    replay_out=$(replay_confirm_bounded 101 "${height}" "${index}" "${mint_hash}" 30 2>&1)
    replay_rc=$?
    set -e
    log_step "  replay confirm: rc=${replay_rc}（仅供诊断，rc 不是判据）, out=$(echo "${replay_out}" | head -c 300)"

    # 2. 节点侧判据：去重护栏自己的诊断必须出现在主链日志里。
    #    两层去重分别打 "already confirmed"（localdb 层）与 "already used"（E14 stateDB 层），
    #    任一层命中都算"护栏执行了"；两层都没有 = 重复确认没被去重拦下 ⇒ 场景失败。
    local logs
    logs=$(main_logs_since "${replay_started_at}")
    if ! echo "${logs}" | grep -qE "already confirmed|already used"; then
        fail "重复确认没有被去重护栏拒绝：主链日志里没有去重诊断（期望 checkConfirm 打出 'already confirmed' 或 'already used'）。replay rc=${replay_rc}, out=$(echo "${replay_out}" | head -c 300)"
    fi

    # 3. CLI 侧判据（本场景的主判据）：**CLI 自己的输出必须带护栏的拒绝诊断**。
    #    不能用 exit code —— 实测 CLI 被拒时也返回 0（见上面 ⚠️ 那段）。用输出文本才分得清：
    #      护栏在   ⇒ wallet send 以 CheckTx 的错误失败，CLI 把 "tx already confirmed"
    #                 （= ErrTxAlreadyConfirmed；stateDB 层则是 "mint already confirmed"）
    #                 打到 stderr，被下面的 grep 命中；
    #      护栏被摘 ⇒ checkConfirm 放行，wallet send 成功并把**新交易的 tx hash** 打到 stdout，
    #                 输出里没有任何 "already confirmed|already used" ⇒ 本断言变红。
    #                 （此时若空证明又被后面的判据拒绝，报的是 "invalid btc tx proof" 之类的
    #                  另一个错，同样命不中本 grep —— 两种情形都判红，正是我们要的。）
    if ! echo "${replay_out}" | grep -qE "already confirmed|already used"; then
        fail "重复确认的 CLI 输出里没有去重护栏的拒绝诊断：说明它没被去重拦下（被放行、或被别的判据拒绝）。out=$(echo "${replay_out}" | head -c 300)"
    fi

    # 4. 供给不变（二次结算的最直接后果就是发行量翻倍）
    local minted_after
    minted_after=$(${MAIN_CLI} rgbx getAsset -s NATIVE1 | jq -r '.totalAmount // 0')
    assert_eq "${minted_after}" "${minted_before}" \
        "重复确认改变了已发行总量（去重失效的二次结算后果）"

    log_step "duplicate confirm rejected by the dedup guard (minted supply unchanged: ${minted_before})"
}

# =====================================================================
# E6(b) + #47 布防观测：BTC 头查询的共识权威校验必须处于"武装"状态且在活跃路径上服务
# =====================================================================
#
# 背景：E6(b) 的护栏（lightclient/executor/btc_index_guard.go 的 checkBtcLocalIndexTip +
# checkBtcHeaderCanonical）用 statedb 的 canonical 窗口/tip 交叉校验 localdb 取到的头，
# 默认 fail-closed；逃生阀 allowBtcIndexMismatch 默认 false。E2E 里 localdb 与 statedb
# 从没分叉过，所以此前"全绿"不能说明这条护栏在活跃路径上、也没被打开逃生阀。
#
# 本场景**能**证明的三件事（都是可判定的）：
#   1. 护栏是"武装"的：CI 生成的 toml 里没有把 allowBtcIndexMismatch 打开（开启即等于
#      关掉这条护栏 —— 这是文档里唯一的绕过方式，所以这条断言在"有人把 CI 改成绕过"时变红）；
#   2. 护栏在**活跃路径**上服务且结论一致：直接向主链发起被护栏包住的查询
#      （lightcli btc header → Query_GetBtcHeader），断言它与共识侧读到的 tip 完全一致；
#   3. **护栏在活跃路径上执行过的直接证据**（本场景的主判据）：窗口外高度的查询会走
#      checkBtcHeaderCanonical 的"维持原行为"分支，护栏自己会打一条 Debug 诊断
#      （"height not in canonical window, keep unchanged"）。断言这条诊断出现 ⇒
#      **把 checkBtcHeaderCanonical 的调用摘掉/绕过，本场景立刻变红**（这就是"注释掉护栏
#      必须变红"的可判定形式）；同时它钉住了"窗口外不得因为校验不了就拒"这条语义。
#      取证读的是**节点日志文件**：CI 的日志配置是 loglevel=debug + logConsoleLevel=info，
#      Debug 行只进文件、不上 stdout，docker logs 抓不到。
#
# **本场景仍然不能证明的**（诚实记账，别读成"护栏的判定分支被验证了"）：
#   判定分支（窗口内 hash 比对 ⇒ 不一致即拒）没有被触发过 —— 健康的 E2E 里 localdb 恒等于
#   statedb，比对永远走"一致 ⇒ 放行"，不产生可观测差异；要构造出分叉，线上没有可行手段
#   （localdb 与 statedb 由同一次执行写入，重组路径还会主动删掉残留的逐高度头），
#   localdb 陈旧/丢库这类真实前提也无法在 E2E 里安全生产。该分支由
#   lightclient/executor/btc_index_guard_test.go 的用例覆盖（那里直接构造分叉）。
#   另外这里同时钉住 #47：生成的 toml 里不得再出现已删除的 btcHeaderStartHeight 键。

# scenario_btc_header_guard_armed 需要主链头链已提交（run_rgb20_env 之后）。
function scenario_btc_header_guard_armed() {
    log_step "scenario: btc header consensus guard armed + on the live path (E6(b)) + no stale config key (#47)"

    # ---- 1. 武装 + #47：生成出来的配置不得绕过护栏 / 不得带已删除的键 ----
    local toml
    for toml in "${ROOT_DIR}"/chain33.test.toml "${ROOT_DIR}"/chain33.para*.toml; do
        [ -f "${toml}" ] || continue
        if grep -qE '^[[:space:]]*allowBtcIndexMismatch[[:space:]]*=[[:space:]]*true' "${toml}"; then
            fail "E6(b) 护栏被绕过：${toml} 里打开了 allowBtcIndexMismatch=true（那是文档里的冷修逃生阀，CI 必须保持关闭）"
        fi
        if grep -qE '^[[:space:]]*[Bb]tcHeaderStartHeight[[:space:]]*=' "${toml}"; then
            fail "#47 回归：生成的 ${toml} 里又出现了已删除的配置键 btcHeaderStartHeight（该键已不存在，起点由 btcd 内置锚点本地推出）"
        fi
    done
    log_step "  guard armed: no allowBtcIndexMismatch=true and no stale btcHeaderStartHeight key in generated configs"

    # ---- 2. 活跃路径：被护栏包住的查询必须与共识侧的 tip 一致 ----
    local started_at tip tip_height header header_hash
    started_at=$(date +%s)
    tip=$(${MAIN_CLI} lightcli btc last | jq -r '.hash // empty')
    tip_height=$(${MAIN_CLI} lightcli btc last | jq -r '.height // 0')
    assert_non_empty "${tip}" "lightcli btc last 为空：主链头链还没提交过头（本场景必须跑在 run_rgb20_env 之后）"

    header=$(${MAIN_CLI} lightcli btc header -t "${tip_height}" || true)
    header_hash=$(echo "${header}" | jq -r '.hash // empty' 2>/dev/null || echo "")
    assert_eq "${header_hash}" "${tip}" \
        "被护栏包住的 GetBtcHeader 返回的头与共识侧 tip 不一致：cross-check 的输入/结论已经不可信"

    # 窗口大小 = btcWorkWindowSize = maxBtcReorgDepth + 1 = 25（statedb 里的 btc-chainstate 只留
    # 最近 25 个**逐头**节点）。头链必须比窗口长，否则构造不出"窗口外"的高度，第 3 步没有证据 ——
    # 那种情况下判失败并说明原因，而不是让断言静默失去意义。
    if ! [[ "${tip_height}" =~ ^[0-9]+$ ]] || [ "${tip_height}" -le 26 ]; then
        fail "E6(b) 取证失败：主链头链太短（tip=${tip_height}，窗口=最近 25 个逐头节点），构造不出窗口外的高度 —— 没有证据故判失败"
    fi

    # ---- 3. 护栏"进入过"的**直接证据**（本场景的主判据，可判定、可反向验证）----
    #
    # 窗口外（高度 < tip - btcWorkWindowSize）的查询走**维持原行为**那条分支：护栏函数
    # checkBtcHeaderCanonical 会打一条 Debug 诊断（"height not in canonical window, keep
    # unchanged"）然后放行。这条诊断有两个用处：
    #   a) 证明护栏**确实在活跃路径上执行过**（把调用注释掉 ⇒ 诊断消失 ⇒ 本场景变红）；
    #   b) 同时钉住"窗口外不得因为校验不了就拒"这条**语义**（放行而不是报错）。
    # 它证明的是"护栏进入了并且放行了不可比的高度"，**不是**"判定分支（窗口内 hash 比对）
    # 拒绝过什么"—— 后者在健康的 E2E 里不可构造（localdb 恒等于 statedb），见文件头注释。
    local log_path line_before probe_heights out_height probe_out probe_hash ok_probe guard_logs
    log_path=$(main_log_file_path)
    assert_non_empty "${log_path}" \
        "E6(b) 取证失败：读不到主链日志文件（候选：${MAIN_LOG_FILE_CANDIDATES}）。护栏的'进入'证据是 Debug 级、只写文件不上 stdout（CI 的 logConsoleLevel=info），没有它就没有证据，故判失败而不是静默通过"
    log_step "  main log file: ${log_path}"

    line_before=$(main_log_line_count "${log_path}")
    probe_heights="$((tip_height - 30)) 1"
    ok_probe=""
    for out_height in ${probe_heights}; do
        if ! [[ "${out_height}" =~ ^[0-9]+$ ]] || [ "${out_height}" -lt 1 ] || [ "${out_height}" -ge "${tip_height}" ]; then
            continue
        fi
        probe_out=$(${MAIN_CLI} lightcli btc header -t "${out_height}" || true)
        probe_hash=$(echo "${probe_out}" | jq -r '.hash // empty' 2>/dev/null || echo "")
        if [ -n "${probe_hash}" ]; then
            ok_probe="${out_height}:${probe_hash}"
            break
        fi
    done
    assert_non_empty "${ok_probe}" \
        "E6(b)：构造不出窗口外查询（tip=${tip_height}，试过 ${probe_heights}）—— 头链太短或 localdb 缺该高度，无法取证"

    guard_logs=$(main_log_file_since_line "${log_path}" "$((line_before + 1))")
    if ! echo "${guard_logs}" | grep -q "checkBtcHeaderCanonical height not in canonical window"; then
        fail "E6(b)：窗口外查询被服务了（${ok_probe}）但护栏没有留下诊断 —— 说明护栏没在活跃路径上执行（例如 checkBtcHeaderCanonical 被摘掉/绕过）。判失败。本窗口新日志：$(echo "${guard_logs}" | tail -5)"
    fi
    log_step "  guard entered: out-of-window probe ${ok_probe} left the guard's own diagnostic (window miss -> pass through)"

    # ---- 4. 本轮不得出现护栏的分歧诊断（出现即说明 localdb 与共识状态真的分叉了）----
    local logs
    logs=$(main_logs_since "${started_at}")
    if echo "${logs}" | grep -qE "disagrees with the on-chain canonical chain|no matching header at the on-chain btc tip"; then
        fail "E6(b) 护栏报了 localdb/共识状态分叉：$(echo "${logs}" | grep -m1 -E 'disagrees with the on-chain canonical chain|no matching header at the on-chain btc tip')"
    fi

    log_step "btc header guard observation OK: armed, entered on the live path, served the live query, tip=${tip_height}:${tip}"
}

# =====================================================================
# #78 链上充值地址绑定：登记 -> 回执/查询 -> 桥 watch 集 -> 向绑定地址充值铸币
# =====================================================================
#
# 链上契约（executor/deposit_script.go）：
#   - RegisterDepositScript：payload 为空，userID = tx.From()；Exec 按
#     (userID, CrossChainInfo.pubkey) 派生 P2WSH 地址，写登记表 + 按高度的索引，并在回执
#     TyDepositScriptLog（tyName=DepositScriptLog）里返回 userID/address/pkScript/spec；
#   - 幂等：同一 userID 重复绑定返回已存条目，不写任何 KV（索引与高度计数都不增长）；
#   - Query_GetDepositAddress 只读登记表：未绑定返回 ErrNotFound，绝不现场派生（否则会把
#     桥没 watch 过的地址展示给用户）。
#
# 桥侧（deposit_registry.go）：官方节点每 10s 扫 ListDepositRegistrations，把每条登记导入
# 钱包 watch 集（ensureUserDepositScript）。**这一步才是"地址可用"的关键**：登记在链上但
# 没进 watch 集，用户打进去的 BTC 对桥不可见。本场景因此不用充值地址发放 HTTP（那会替桥
# 把脚本导进去、掩盖链上同步是否生效），而是等 para1 的 watch 集日志，再向链上登记得到的
# 地址充值，断言 XBTC 确实铸到该 userID 名下。
#
# 被测账户取 USER_B（AUTH_ADDR2）：主链有余额可付手续费，且不参与后续 RGB20 余额断言
# （那些都用 USER_MAIN_ADDR）。
function scenario_rgbx_deposit_address_binding() {
    log_step "scenario: on-chain deposit address binding -> bridge watch set -> BTC deposit mints XBTC (#78)"
    local user="${DEPOSIT_BIND_USER_ADDR:-${USER_B_ADDR}}"
    local key="${DEPOSIT_BIND_USER_KEY:-${USER_B_KEY}}"
    local amount="${DEPOSIT_BIND_AMOUNT_SATS:-200000}"
    assert_non_empty "${user}" "deposit binding user address empty"
    assert_non_empty "${key}" "deposit binding user key empty"

    # ---- 1. 绑定前：明确 NotFound（不是空地址，也不是现场派生）----
    local pre
    pre=$(rgbx_query "GetDepositAddress" "{\"data\":\"${user}\"}")
    if ! echo "${pre}" | jq -e '.result == null and (.error | tostring | test("ErrNotFound"))' >/dev/null 2>&1; then
        fail "GetDepositAddress before binding must fail with ErrNotFound (it must never derive on the fly), got: $(echo "${pre}" | head -c 400)"
    fi
    log_step "  pre-binding: GetDepositAddress -> ErrNotFound (as required)"

    # ---- 2. 用测试账户发一笔 RegisterDepositScript（payload 为空）----
    local reg_started_at reg_hash reg_bare
    reg_started_at=$(date +%s)
    reg_hash=$(send_main_rgbx_action "RegisterDepositScript" '{}' "${key}")
    reg_bare=$(normalize_tx_hash "${reg_hash}")
    local reg_pos reg_height reg_index
    reg_pos=$(wait_main_tx_committed "${reg_bare}")
    reg_height=${reg_pos%% *}
    reg_index=${reg_pos##* }
    assert_eq "$(main_tx_receipt_ty "${reg_bare}")" "ExecOk" "RegisterDepositScript receipt"

    # ---- 3. 回执日志必须带 address/pkScript/spec（执行器写的就是这三样）----
    local reg_item address pk_script spec
    reg_item=$(main_tx_receipt_log "${reg_bare}" "DepositScriptLog")
    assert_non_empty "${reg_item}" "RegisterDepositScript receipt has no DepositScriptLog"
    address=$(echo "${reg_item}" | jq -r '.address // empty')
    pk_script=$(echo "${reg_item}" | jq -r '.pkScript // empty')
    spec=$(echo "${reg_item}" | jq -r '.spec // empty')
    assert_non_empty "${address}" "DepositScriptLog address empty"
    assert_length "${pk_script}" 68 "DepositScriptLog pkScript must be a 34-byte P2WSH program (68 hex chars)"
    assert_eq "${spec}" "p2wsh-deposit-v1" "DepositScriptLog spec"
    assert_eq "$(echo "${reg_item}" | jq -r '.userID // empty')" "${user}" \
        "DepositScriptLog userID must be tx.From()"
    log_step "  receipt: address=${address} pkScript=${pk_script} spec=${spec} height=${reg_height}"

    # 查询返回同一条登记（只读登记表，不是重新派生）。
    local q
    q=$(rgbx_query "GetDepositAddress" "{\"data\":\"${user}\"}" | jq -c '.result // empty' || true)
    assert_non_empty "${q}" "GetDepositAddress after binding returned nothing"
    assert_eq "$(echo "${q}" | jq -r '.address')" "${address}" "query address differs from the receipt"
    assert_eq "$(echo "${q}" | jq -r '.pkScript')" "${pk_script}" "query pkScript differs from the receipt"
    assert_eq "$(echo "${q}" | jq -r '.spec')" "${spec}" "query spec differs from the receipt"
    assert_eq "$(echo "${q}" | jq -r '.height')" "${reg_height}" "query height differs from the receipt"

    # ---- 4. 重复绑定必须幂等：返回同一条目，且不新增登记/索引 ----
    local dup_started_at dup_hash dup_bare dup_pos dup_height dup_item
    dup_started_at=$(date +%s)
    dup_hash=$(send_main_rgbx_action "RegisterDepositScript" '{}' "${key}")
    dup_bare=$(normalize_tx_hash "${dup_hash}")
    dup_pos=$(wait_main_tx_committed "${dup_bare}")
    dup_height=${dup_pos%% *}
    assert_eq "$(main_tx_receipt_ty "${dup_bare}")" "ExecOk" "duplicate RegisterDepositScript receipt"
    dup_item=$(main_tx_receipt_log "${dup_bare}" "DepositScriptLog")
    assert_non_empty "${dup_item}" "duplicate RegisterDepositScript receipt has no DepositScriptLog"
    assert_eq "$(echo "${dup_item}" | jq -r '.address // empty')" "${address}" \
        "duplicate binding must return the stored address"
    assert_eq "$(echo "${dup_item}" | jq -r '.pkScript // empty')" "${pk_script}" \
        "duplicate binding must return the stored pkScript"

    # 高度区间查询覆盖两次绑定所在的高度：userID 只能出现一次（第二次没有写 KV）。
    local list_start=$(( reg_height > 0 ? reg_height - 1 : 0 ))
    local reg_list reg_count
    reg_list=$(rgbx_query "ListDepositRegistrations" \
        "{\"startHeight\":${list_start},\"startIndex\":0,\"count\":100,\"endHeight\":$(( dup_height + 2 ))}")
    reg_count=$(echo "${reg_list}" | jq --arg u "${user}" '[.result.items[]? | select(.userID == $u)] | length' || true)
    assert_eq "${reg_count}" "1" \
        "duplicate binding wrote a second registration/index entry (window ${list_start}..$((dup_height + 2)))"

    # 第二次执行走 "already registered" 分支（不派生、不写 KV）。
    if ! main_logs_since "${dup_started_at}" | grep -q "Exec_RegisterDepositScript already registered"; then
        fail "the duplicate RegisterDepositScript did not take the already-registered branch"
    fi
    log_step "  duplicate binding is idempotent: same address/pkScript, 1 registry entry, no new index"

    # ---- 5. 桥必须把链上登记导入 watch 集（"地址可用"的关键）----
    local i bridge_logs imported=""
    for ((i = 0; i < 90; i++)); do
        bridge_logs=$(para1_logs_since "${reg_started_at}")
        # `|| true`：grep 无匹配时管道返回 1，而这是普通赋值（不是 if 条件），set -e 会让
        # 整个 run 在"还没等到日志"的那一刻静默退出。
        imported=$(echo "${bridge_logs}" | grep "ensureUserDepositScript watch set grown" | grep "${user}" | tail -1 || true)
        [ -n "${imported}" ] && break
        sleep 1
    done
    if [ -z "${imported}" ]; then
        if echo "${bridge_logs}" | grep -q "ALERT deposit registry entry cannot be imported"; then
            fail "the bridge failed to import the on-chain registration: $(echo "${bridge_logs}" | grep "ALERT deposit registry entry cannot be imported" | tail -1)"
        fi
        fail "the bridge did not import the on-chain registration of ${user} into its watch set; para1 log tail: $(echo "${bridge_logs}" | tail -8)"
    fi
    log_step "  bridge watch set: ${imported}"

    # ---- 5.1 等签名节点（para2-4）也拿到这条脚本，再充值 ----
    #
    # 为什么必须等：扫集由官方节点发起，但**签名节点各自核对**"这个 P2WSH 输入是不是已登记的
    # 用户充值脚本"（IsUserDepositScript）。非官方节点不扫链，它们每 30s 才从侧车并集里补齐
    # watch 集。如果充值刚确认、扫集 worker（3s 周期）立刻开签，签名节点还看不到该脚本，会以
    # "is not a registered user deposit script" 拒签；同一笔 PSBT 重试时复用同一个 CGGMP 会话名，
    # 超时轮次的残留消息会把后续轮次打成 "state Init -> Failed"，整条扫集路径就此卡住
    # （实测会把后面 scenario_user_deposit_sweep 一起拖失败）。
    local merged_all="" svc
    for ((i = 0; i < 90; i++)); do
        merged_all="true"
        for svc in para2 para3 para4; do
            if ! para_logs_since "${svc}" "${reg_started_at}" |
                grep -q "mergeRemoteDepositScripts merged remote deposit scripts"; then
                merged_all=""
                break
            fi
        done
        [ -n "${merged_all}" ] && break
        sleep 2
    done
    assert_true "${merged_all}" \
        "signing nodes did not merge the on-chain deposit script within 180s; signing a sweep now would be refused with 'not a registered user deposit script' (para2-4 log tail: $(para_logs_since para2 "${reg_started_at}" | tail -2; para_logs_since para3 "${reg_started_at}" | tail -2; para_logs_since para4 "${reg_started_at}" | tail -2))"
    log_step "  signing nodes merged the deposit script registry (para2-4)"

    # ---- 6. 向**绑定得到的地址**充值（不再经过发放 HTTP），断言铸到该 userID ----
    local tss_pubkey derived
    tss_pubkey=$(${MAIN_CLI} rgbx getCrossChainInfo -s "${MINT_SYMBOL}" | jq -r '.pubkey // empty')
    assert_non_empty "${tss_pubkey}" "tss pubkey empty before binding-scenario deposit"
    derived=$(compose_cmd exec -T main /root/chain33-cli rgbx btcDepositAddress \
        --net "${BTC_NETWORK}" -d "${user}" -k "${tss_pubkey}" 2>/dev/null | tail -1 || true)
    assert_eq "$(echo "${derived}" | jq -r '.address // empty')" "${address}" \
        "local derivation disagrees with the on-chain registration (bridge would watch a different script)"
    assert_eq "$(echo "${derived}" | jq -r '.pkScript // empty')" "${pk_script}" \
        "local pkScript derivation disagrees with the on-chain registration"

    local utxo before_xbtc expected_xbtc deposit_tx
    utxo=$(build_mature_coinbase_utxo "$(( amount + 500 ))")
    assert_non_empty "${utxo}" "funding utxo empty for binding-scenario deposit"
    before_xbtc=$(query_xbtc_balance "${user}")
    deposit_tx=$(compose_cmd exec -T main /root/chain33-cli rgbx btcDepositTx \
        --net "${BTC_NETWORK}" \
        --rpcHost "${BTC_RPC_ADDR}" \
        --rpcUser "${BTCD_RPC_USER}" \
        --rpcPass "${BTCD_RPC_PASS}" \
        --disableTLS=false \
        --rpcCertFile "${BTCD_RPC_CERT_IN_CONTAINER}" \
        --wif "${BTC_FUNDING_WIF}" \
        --utxo "${utxo}" \
        --depositAddress "${user}" \
        --tssPubkey "${tss_pubkey}" \
        --amount "${amount}" \
        --fee 500)
    assert_length "${deposit_tx}" 64 "btc deposit tx hash (binding scenario)"
    mine_btcd_blocks 2
    expected_xbtc=$(awk "BEGIN{printf \"%.8f\", ${before_xbtc} + ${amount}/100000000}")
    wait_xbtc_balance_not_less_than "${user}" "${expected_xbtc}" 180
    log_step "PASS: #78 binding usable end to end: user=${user} address=${address} btcTx=${deposit_tx} " \
        "xbtc ${before_xbtc} -> >= ${expected_xbtc}"
}

# =====================================================================
# 手工放弃提现（Abandon）：链上存证 -> 桥按本地白名单受理 -> TSS Unlock -> released
# =====================================================================
#
# 契约：
#   - 链上 Abandon（action 名 "Abandon"，字段 burnTxHash/owner/reason）只存证，不做授权判定；
#     它只校验提案描述的是"真实且未终态的提现"（payload/台账/owner/终态键）；
#   - 桥侧（只有官方节点跑 abandonWorker）用本地白名单 abandonOperators 决定是否受理；受理
#     还要求 pending 未确认、owner 与台账一致、链上无终态、本节点从未产出付款签名；
#   - 受理是单向的：先落盘 abandoning，再复用 Unlock 通道组签名退回锁定金额；
#   - 受理后结算重试永久让位（settlementAllowed=false）；同一 burn 的 Confirm 被链上终态键拒绝。
#
# "不会被付款的提现"的构造：发票**不扣**手续费 → 桥每轮 BuildWithdrawal 后以 payout mismatch
# 拒绝（可重试、但永远不签名不广播），pending 一直挂着。不用"BTC 主池余额不足"是因为它在本
# harness 里不可稳定构造：充值会被扫集自动归集回主池，而 XBTC 余额又受 1:1 铸币上限约束。
#
# 位置：排在 fee-mismatch 负例之后。它自己那笔提现在本场景内被退回（余额复原），不会影响
# 后续场景；后面唯一还会读 RGB20 余额的只有备份演练（它自己重新充值）。
function scenario_rgbx_manual_abandon_withdraw() {
    log_step "scenario: manual abandon (chain-33 proposal -> local whitelist -> TSS Unlock -> released)"

    # The "never payable but retryable" withdrawal is built from a fee-less invoice, which needs
    # a non-zero on-chain rgb20WithdrawFee. Without governance seeding the fee is 0, the invoice
    # is valid, and the withdraw would simply be paid — skip rather than assert the impossible.
    local abandon_fee
    abandon_fee=$(rgbx_query "GetBridgeConfig" "{\"data\":\"${RGB20_SYMBOL}\"}" |
        jq -r '[.result.rgb20WithdrawFee[]? | select((.symbol | ascii_upcase) == "'"${RGB20_SYMBOL}"'") | .feeMinUnits] | first // 0' 2>/dev/null)
    if [ -z "${abandon_fee}" ] || [ "${abandon_fee}" -le 0 ] 2>/dev/null; then
        log_step "SKIP: on-chain rgb20WithdrawFee is ${abandon_fee:-0} (governance not seeded); cannot construct an unpayable withdrawal for the abandon flow"
        return 0
    fi
    local operator="${ABANDON_OPERATOR_ADDR:-${AUTH_ADDR1}}"
    local operator_key="${ABANDON_OPERATOR_KEY:-${AUTH_KEY1}}"
    local amount="${RGB20_ABANDON_WITHDRAW_AMOUNT:-10000}"
    local started_at
    started_at=$(date +%s)

    local before lock_addr lock_before sidecar_before
    before=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    lock_addr=$(query_rgb20_lock_address)
    assert_non_empty "${lock_addr}" "cannot read the cross-chain lock address"
    lock_before=$(query_rgb20_balance "${lock_addr}")
    sidecar_before=$(query_rgb20_ledger_spendable)
    assert_true "$(awk "BEGIN{print (${sidecar_before} >= ${amount})?\"true\":\"false\"}")" \
        "rgb20 sidecar holdings too low for the abandon case: have=${sidecar_before}, need=${amount}"
    local amount_decimal
    amount_decimal=$(awk "BEGIN{printf \"%.8f\", ${amount}/100000000}")
    assert_true "$(awk "BEGIN{print (${before} >= ${amount_decimal})?\"true\":\"false\"}")" \
        "rgb20 user balance too low for the abandon case: have=${before}, need=${amount_decimal}"

    # ---- 1. 发票不扣手续费：桥会每轮拒绝（可重试），永远不会签名/广播 ----
    local user_invoice
    user_invoice=$(curl -s -X POST http://127.0.0.1:50064/sim/user_invoice \
        -H 'Content-Type: application/json' \
        -d "{\"asset_symbol\":\"${RGB20_SIDECAR_SYMBOL}\",\"amount\":${amount}}" | jq -r '.invoice // empty')
    assert_non_empty "${user_invoice}" "abandon case: user invoice empty"

    # ---- 2. 链上提现（金额进入锁定地址）----
    local withdraw_hash burn_hash burn_amount
    withdraw_hash=$(${MAIN_CLI} send rgbx withdraw -a "${amount_decimal}" -f 20 -d "${user_invoice}" \
        -s "${RGB20_SYMBOL}" -k "${GENESIS_KEY}")
    assert_length "${withdraw_hash}" 66 "abandon case: withdraw tx hash"
    burn_hash=$(normalize_tx_hash "${withdraw_hash}")
    burn_amount=$(wait_pending_withdraw_amount "${USER_MAIN_ADDR}" "${withdraw_hash}")
    assert_eq "${burn_amount}" "${amount}" "abandon case: chain33 burn amount"
    assert_balance "$(query_rgb20_balance "${USER_MAIN_ADDR}")" \
        "$(awk "BEGIN{printf \"%.8f\", ${before} - ${amount_decimal}}")" \
        "abandon case: the amount must leave the user's balance when the withdraw locks it"
    assert_balance "$(query_rgb20_balance "${lock_addr}")" \
        "$(awk "BEGIN{printf \"%.8f\", ${lock_before} + ${amount_decimal}}")" \
        "abandon case: the amount must arrive at the lock address"

    # 桥已经在重试结算、并且拒绝点早于签名：payout mismatch 是"永不付款"的直接证据。
    local i logs reason=""
    for ((i = 0; i < 60; i++)); do
        logs=$(para1_logs_since "${started_at}" | perl -pe 's/\e\[[0-9;]*m//g')
        # 必须按本笔 burn 过滤：上一场景（fee-mismatch 负例）的 pending 也会一直刷同一条
        # "withdraw payout mismatch"，只匹配文案会把别人的重试当成自己的证据。
        reason=$(echo "${logs}" | grep "withdraw payout mismatch" | grep "${burn_hash}" | tail -1 || true)
        [ -n "${reason}" ] && break
        sleep 1
    done
    if [ -z "${reason}" ]; then
        fail "abandon case: the bridge did not refuse the fee-less invoice (no 'withdraw payout mismatch'); para1 tail: $(para1_logs_since "${started_at}" | tail -5)"
    fi
    log_step "  the pending can never be paid: $(echo "${reason}" | head -c 300)"

    # ---- 3. 非白名单提案：链上存证，但桥必须忽略（不发起 unlock）----
    local nonwl_started_at nonwl_hash nonwl_bare nonwl_pos
    nonwl_started_at=$(date +%s)
    nonwl_hash=$(send_main_rgbx_action "Abandon" \
        "{\"burnTxHash\":\"0x${burn_hash}\",\"owner\":\"${USER_MAIN_ADDR}\",\"reason\":\"e2e non-whitelist proposer\"}" \
        "${GENESIS_KEY}")
    nonwl_bare=$(normalize_tx_hash "${nonwl_hash}")
    nonwl_pos=$(wait_main_tx_committed "${nonwl_bare}")
    assert_eq "$(main_tx_receipt_ty "${nonwl_bare}")" "ExecOk" "non-whitelist Abandon receipt"
    assert_non_empty "$(main_tx_receipt_log "${nonwl_bare}" "AbandonWithdrawLog")" \
        "the non-whitelist proposal must still be recorded on chain (the chain only stores it)"
    if ! main_logs_since "${nonwl_started_at}" | grep "Exec_Abandon recorded" | grep -q "${burn_hash}"; then
        fail "the non-whitelist proposal was not recorded on chain (no Exec_Abandon recorded for ${burn_hash})"
    fi

    local rejected=""
    for ((i = 0; i < 60; i++)); do
        logs=$(para1_logs_since "${nonwl_started_at}" | perl -pe 's/\e\[[0-9;]*m//g')
        rejected=$(echo "${logs}" | grep "abandon proposal rejected" | grep "${nonwl_bare}" | tail -1 || true)
        [ -n "${rejected}" ] && break
        sleep 1
    done
    if [ -z "${rejected}" ]; then
        fail "the bridge did not reject the non-whitelist proposal ${nonwl_bare}; para1 tail: $(para1_logs_since "${nonwl_started_at}" | tail -5)"
    fi
    echo "${rejected}" | grep -q "not a configured abandon operator" ||
        fail "the non-whitelist proposal was rejected for the wrong reason: ${rejected}"
    if echo "${logs}" | grep "abandon proposal accepted" | grep -q "${burn_hash}"; then
        fail "a non-whitelist proposal was accepted for ${burn_hash}"
    fi
    if main_logs_since "${nonwl_started_at}" | grep "Exec_Unlock released" | grep -q "${burn_hash}"; then
        fail "a non-whitelist proposal triggered an on-chain unlock for ${burn_hash}"
    fi
    log_step "  non-whitelist proposer ignored: ${rejected}"

    # ---- 4. 白名单提案：受理 -> abandoning 落盘 -> 组签名 Unlock ----
    local wl_started_at wl_hash wl_bare wl_pos accept_at accepted=""
    wl_started_at=$(date +%s)
    wl_hash=$(send_main_rgbx_action "Abandon" \
        "{\"burnTxHash\":\"0x${burn_hash}\",\"owner\":\"${USER_MAIN_ADDR}\",\"reason\":\"e2e operator abandonment\"}" \
        "${operator_key}")
    wl_bare=$(normalize_tx_hash "${wl_hash}")
    wl_pos=$(wait_main_tx_committed "${wl_bare}")
    assert_eq "$(main_tx_receipt_ty "${wl_bare}")" "ExecOk" "whitelisted Abandon receipt"

    for ((i = 0; i < 90; i++)); do
        logs=$(para1_logs_since "${wl_started_at}" | perl -pe 's/\e\[[0-9;]*m//g')
        accepted=$(echo "${logs}" | grep "abandon proposal accepted" | grep "${burn_hash}" | tail -1 || true)
        [ -n "${accepted}" ] && break
        sleep 1
    done
    if [ -z "${accepted}" ]; then
        fail "the whitelisted proposal was not accepted for ${burn_hash}; para1 tail: $(para1_logs_since "${wl_started_at}" | tail -8)"
    fi
    accept_at=$(date +%s)
    echo "${accepted}" | grep -q "${operator}" ||
        fail "acceptance log does not name the whitelisted proposer ${operator}: ${accepted}"
    log_step "  accepted: ${accepted}"

    # abandoning 落盘：受理日志只在 setWithdrawState 成功之后打印；再用 neutrino.db 里的
    # 明文值做一次独立核对（受理失败会记 "cannot persist abandoning state" 并拒绝受理）。
    if echo "${logs}" | grep -q "cannot persist abandoning state"; then
        fail "the bridge refused the proposal because the abandoning state could not be persisted"
    fi
    local persisted
    persisted=$(compose_cmd exec -T para1 sh -c \
        "grep -a -c 'abandoning' /root/paradatadir/lightclient/neutrino.db 2>/dev/null || true" | tr -d '\r')
    assert_true "$([ "${persisted:-0}" -ge 1 ] 2>/dev/null && echo true || echo false)" \
        "the abandoning state is not persisted in para1's neutrino.db (got '${persisted}')"

    # ---- 5. 链上终态 released（共识键，不是桥自报）+ 只解锁一次 ----
    local final_state="" state_retries=180
    for ((i = 0; i < state_retries; i++)); do
        final_state=$(query_withdraw_final_state "${burn_hash}")
        [ "${final_state}" = "released" ] && break
        sleep 1
    done
    assert_eq "${final_state}" "released" \
        "abandon case: on-chain terminal state must be released (TSS group signature + Exec_Unlock)"
    local unlock_count
    unlock_count=$(main_logs_since "${wl_started_at}" | perl -pe 's/\e\[[0-9;]*m//g' |
        grep "Exec_Unlock released" | grep -c "${burn_hash}" || true)
    assert_eq "${unlock_count}" "1" "abandon case: exactly one on-chain release expected for ${burn_hash}"

    # ---- 6. 退款到账、锁定地址回退、无付款广播 ----
    local refunded
    refunded=$(wait_rgb20_balance_equal "${USER_MAIN_ADDR}" "${before}") ||
        fail "abandon case: user balance did not return to ${before}, got ${refunded}"
    assert_balance "$(query_rgb20_balance "${lock_addr}")" "${lock_before}" \
        "abandon case: the lock address must drop back to its pre-withdraw balance"
    if para1_logs_since "${started_at}" | grep "BroadcastTx rgb20 withdraw" | grep -q "${burn_hash}"; then
        fail "abandon case: a payment was broadcast for ${burn_hash}, but abandonment must happen before any payment signature"
    fi

    # ---- 7. 受理后不得回到结算重试（唯一可观察的"单向状态"证据）----
    # 受理时在途的一轮结算重试已经结束（两边共用 paymentMu）；之后 settlementAllowed=false，
    # 不应再出现任何针对该 burn 的结算重试日志。
    sleep 6
    local retries_after
    retries_after=$(para1_logs_since "$(( accept_at + 5 ))" | perl -pe 's/\e\[[0-9;]*m//g' |
        grep "withdrawalProcessor rgb20 retry" | grep -c "${burn_hash}" || true)
    assert_eq "${retries_after}" "0" \
        "abandon case: settlement was retried ${retries_after} time(s) after acceptance for ${burn_hash}"

    # ---- 8. 重复 Confirm 被链上终态键拒绝 ----
    local details height index replay_started_at replay_out
    details=$(${MAIN_CLI} tx query -s "${burn_hash}" 2>/dev/null || true)
    height=$(echo "${details}" | jq -r '.height // 0' 2>/dev/null || echo 0)
    index=$(echo "${details}" | jq -r '.index // 0' 2>/dev/null || echo 0)
    if ! [[ "${height}" =~ ^[0-9]+$ ]] || [ "${height}" -le 0 ]; then
        fail "abandon case: cannot locate burn ${burn_hash} on chain (tx query: $(echo "${details}" | head -c 200))"
    fi
    replay_started_at=$(date +%s)
    set +e
    replay_out=$(replay_confirm_bounded 106 "${height}" "${index}" "${burn_hash}" 30 2>&1)
    set -e
    echo "${replay_out}" | grep -q "withdraw already confirmed" ||
        fail "abandon case: settling a released burn was not rejected with the terminal-state error. out=$(echo "${replay_out}" | head -c 300)"
    if ! main_logs_since "${replay_started_at}" | perl -pe 's/\e\[[0-9;]*m//g' |
        grep -q "checkWithdrawConfirm burn already used"; then
        fail "abandon case: the main node did not run the terminal-state guard on the Confirm replay"
    fi

    log_step "PASS: manual abandon released on chain: burn=${burn_hash} proposer=${operator} " \
        "balance ${before} -> ${refunded}, lock ${lock_before} -> ${lock_before}, no payment broadcast, Confirm rejected"
}

# =====================================================================
# 入口（testcase.sh 调用）
# =====================================================================

# 网关事件账本（#81）：桥把链上拿不到的审计事实投递到网关，网关按游标提供读取。
# 这里断言"关键生命周期的事件确实到达了网关"，而不是逐字段核对（字段形状由 Go 单测钉住）。
# 事件投递是异步的（本地 outbox → 后台投递 → 网关确认），所以这里做有界等待。
function scenario_bridge_event_ledger() {
    log_step "scenario: bridge event ledger receives deposit/withdraw audit events"
    local token="${RGB20_GATEWAY_TOKEN:-e2e-bridge-token-0123456789abcd}"
    local url="http://127.0.0.1:${RGB20_GATEWAY_PORT}/bridge/events/query?limit=1000"
    local types=""
    local i
    for ((i = 0; i < 30; i++)); do
        types=$(curl -sf -H "Authorization: Bearer ${token}" "${url}" |
            jq -r '.events[].type' 2>/dev/null || true)
        if echo "${types}" | grep -qx "deposit.minted" &&
            echo "${types}" | grep -qx "withdraw.broadcast" &&
            echo "${types}" | grep -qx "withdraw.locked"; then
            break
        fi
        sleep 1
    done
    for want in deposit.settled deposit.minted withdraw.locked withdraw.broadcast; do
        echo "${types}" | grep -qx "${want}" ||
            fail "bridge event ledger is missing ${want}; got: $(echo ${types} | tr '\n' ' ')"
    done
    # 账本必须只含允许的事件类型 —— 出现控制面/畸形事件说明桥发了不该发的东西。
    local bad
    bad=$(echo "${types}" | grep -vE '^(deposit|withdraw|sweep|treasury)\.' || true)
    assert_eq "${bad}" "" "unexpected event types in the bridge ledger: ${bad}"
    log_step "PASS: bridge event ledger: $(echo ${types} | sort -u | tr '\n' ' ')"
}

# 桥运行指标（#85）：桥按 gatewayMetricsInterval 主动推送快照，网关聚合后以 Prometheus 文本格式
# 暴露。本场景断言"推送链路真的通了"：latest 里至少一个节点、/metrics 里能看到该节点的
# deposit.minted 计数与 up 状态。推送是异步的（固定间隔），所以做有界等待。
#
# 必须排在充值/提现场景之后：计数从进程启动开始累计，此时 deposit.minted 与 withdraw.* 已 >0。
function scenario_bridge_metrics() {
    log_step "scenario: bridge runtime metrics reach the gateway and are exposed as Prometheus text (#85)"
    local token="${RGB20_GATEWAY_TOKEN:-e2e-bridge-token-0123456789abcd}"
    local base="http://127.0.0.1:${RGB20_GATEWAY_PORT}"
    local latest="" count="" i

    # 1) GET /bridge/metrics/latest：至少一个节点，且带网关侧的 received_at。
    for ((i = 0; i < 60; i++)); do
        latest=$(curl -sf -H "Authorization: Bearer ${token}" "${base}/bridge/metrics/latest" || true)
        count=$(echo "${latest}" | jq -r '.count // 0' 2>/dev/null || echo 0)
        if [ "${count:-0}" -ge 1 ]; then
            break
        fi
        sleep 1
    done
    if ! [ "${count:-0}" -ge 1 ]; then
        fail "gateway /bridge/metrics/latest returned no snapshots: $(echo "${latest}" | head -c 400)"
    fi
    echo "${latest}" | jq -e '.nodes[0].received_at > 0' >/dev/null ||
        fail "metrics snapshot has no received_at: $(echo "${latest}" | head -c 400)"
    echo "${latest}" | jq -e '.nodes[0].counters | type == "object"' >/dev/null ||
        fail "metrics snapshot has no counters object: $(echo "${latest}" | head -c 400)"

    # 2) GET /metrics（无鉴权）：Prometheus 文本里有该节点的事件计数，且 up=1。
    local metrics=""
    for ((i = 0; i < 30; i++)); do
        metrics=$(curl -sf "${base}/metrics" || true)
        if echo "${metrics}" | grep -qE 'rgbx_bridge_events_total\{node="[^"]+",type="deposit\.minted"\} [1-9]'; then
            break
        fi
        sleep 1
    done
    echo "${metrics}" | grep -qE 'rgbx_bridge_events_total\{node="[^"]+",type="deposit\.minted"\} [1-9]' ||
        fail "gateway /metrics has no positive deposit.minted counter for any node: $(echo "${metrics}" | head -c 500)"
    echo "${metrics}" | grep -qE 'rgbx_bridge_up\{node="[^"]+"\} 1' ||
        fail "gateway /metrics does not report any bridge as up: $(echo "${metrics}" | head -c 500)"
    echo "${metrics}" | grep -qE '^# TYPE rgbx_bridge_events_total counter$' ||
        fail "gateway /metrics is missing the Prometheus TYPE line for rgbx_bridge_events_total"
    echo "${metrics}" | grep -qE '^rgb_gateway_data_dir_writable (0|1)$' ||
        fail "gateway /metrics is missing rgb_gateway_data_dir_writable"

    log_step "PASS: bridge metrics: latest count=${count}, deposit.minted > 0 and rgbx_bridge_up=1 in /metrics"
}

function run_rgb20_all() {
    run_rgb20_env
    # #78 紧跟 env：桥（官方节点）刚就绪、btcd 的 segwit 已激活，充值地址绑定 + 充值铸币
    # 走的就是这条已就绪的 P2WSH 通道（不消耗 RGB20 流动性）。
    scenario_rgbx_deposit_address_binding
    run_rgb20_sidecar_genesis_withdraw_probe
    scenario_rgb20_deposit
    # E16 紧跟充值场景：它复用那笔已 settle 的 consignment 与 receive（不额外消耗流动性），
    # 且断言的是充值刚走过的同一条归属路径。
    scenario_rgb20_consignment_hint_mismatch
    scenario_rgb20_withdraw
    scenario_rgb20_two_withdrawals
    scenario_rgb20_truncation_withdraw
    # 事件账本紧跟前面的入金/出金场景：此时 deposit.*、withdraw.locked/broadcast 都已发生，
    # 投递是异步的，场景内部有界等待。
    scenario_bridge_event_ledger
    # #85：事件账本之后，指标快照（2s 间隔）里已包含同一批充值/提现计数。
    scenario_bridge_metrics
    run_rgb20_sidecar_smoke
    # #70 unlock: rebuilds the sidecar ledger (then restores it) to construct a permanently
    # rejected withdrawal. It must run before the fee-mismatch case: that case intentionally
    # leaves a pending, and a ledger swap while it exists would turn it into a second unlock.
    scenario_rgb20_unlock_after_permanent_rejection
    # 负例排在**最后**：它故意留下一笔永久 pending 并锁住用户的资产（见该场景的注释），
    # 不会（也不该）被后面的场景消化掉。
    scenario_rgb20_withdraw_fee_mismatch
    # 手工放弃提现：自己新建一笔"永不付款"的 pending（发票不扣手续费），分别用非白名单/白名单
    # 提案人验证受理判定，最后把它退回。排在 fee-mismatch 之后：它不再依赖用户余额（放弃是原额
    # 退回），也不会消化别人那笔 pending（所有等待都按 tx hash 认领）。
    scenario_rgbx_manual_abandon_withdraw
}
