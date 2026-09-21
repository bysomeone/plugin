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

function wait_rgb20_balance_not_less_than() {
    local addr="$1"
    local expected="$2"
    local retries="${3:-60}"
    local i
    for ((i = 0; i < retries; i++)); do
        local balance
        balance=$(query_rgb20_balance "${addr}")
        if awk "BEGIN{exit !(${balance} >= ${expected})}"; then
            log_step "rgb20 balance reached: addr=${addr}, balance=${balance} >= ${expected}"
            return 0
        fi
        # 主链铸币依赖签名节点的 VerifyDepositSpv —— 它查主链 lightclient 的 BTC 头。
        # 充值付款交易通常落在 btcd 的 tip 上，而 para1 提交 BTC 头受 blockConfirmations(=1)
        # 限制只能提交到 tip-1；若此时链条不再前进，该高度的头永远提交不上去，spv verify 一直
        # ErrNotFound、充值永不铸币（全新重建后必现）。这里等待期间持续挖块，让头链跟上。
        mine_btcd_blocks 1
        sleep 2
    done
    fail "rgb20 balance not reached, addr=${addr}, expected>=${expected}"
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
    log_step "RGB20 env done: sidecar up (GG18 pubkey), contracts.assetId=${RGB20_ASSET_ID}"
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

# =====================================================================
# RGB20 充值 E2E
# =====================================================================

function scenario_rgb20_deposit() {
    log_step "scenario: RGB20 deposit (user pay -> sidecar settle -> TSS deposit sign -> chain33 mint)"
    local before
    before=$(query_rgb20_balance "${USER_MAIN_ADDR}")

    # 1. Go 桥 CreateReceive（para1 rgb20 HTTP）→ 侧车 invoice
    local rec_json
    rec_json=$(curl -s -X POST http://127.0.0.1:17000/rgbx/v1/deposit \
        -H 'Content-Type: application/json' \
        -d "{\"requestId\":\"rgb20-dep-1\",\"assetSymbol\":\"${RGB20_SYMBOL}\",\"amount\":${RGB20_DEPOSIT_AMOUNT},\"chain33Addr\":\"${USER_MAIN_ADDR}\"}")
    local receive_id invoice
    receive_id=$(echo "${rec_json}" | jq -r '.data.receiveId // empty')
    invoice=$(echo "${rec_json}" | jq -r '.data.invoice // empty')
    assert_non_empty "${receive_id}" "rgb20 receive_id empty"
    assert_non_empty "${invoice}" "rgb20 invoice empty"
    log_step "  receive_id=${receive_id}"

    # 2. 侧车 test-sim 构建用户付款（未签 PSBT + consignment）
    local pay_json
    pay_json=$(curl -s -X POST http://127.0.0.1:50064/sim/user_pay \
        -H 'Content-Type: application/json' -d "{\"invoice\":\"${invoice}\"}")
    local psbt_hex cons_hex
    psbt_hex=$(echo "${pay_json}" | jq -r '.psbt // empty')
    cons_hex=$(echo "${pay_json}" | jq -r '.consignment // empty')
    assert_non_empty "${psbt_hex}" "rgb20 user_pay psbt empty"
    assert_non_empty "${cons_hex}" "rgb20 user_pay consignment empty"

    # 3. Go 桥 TSS 组签名 PSBT（test sign-psbt 端点）
    local signed_psbt
    signed_psbt=$(curl -s -X POST http://127.0.0.1:17000/rgbx/v1/sign-psbt \
        -H 'Content-Type: application/json' -d "{\"psbt\":\"${psbt_hex}\"}" | jq -r '.data.psbt // empty')
    assert_non_empty "${signed_psbt}" "rgb20 sign-psbt empty"

    # 4. 侧车 test-sim 广播已签 PSBT + provide consignment → settle
    local settle_status
    settle_status=$(curl -s -X POST http://127.0.0.1:50064/sim/user_pay_submit \
        -H 'Content-Type: application/json' \
        -d "{\"psbt\":\"${signed_psbt}\",\"consignment\":\"${cons_hex}\",\"receive_id\":\"${receive_id}\"}" | jq -r '.status // empty')
    assert_eq "${settle_status}" "settled" "rgb20 settle status"

    # 本场景的日志窗口起点（下面断言"首次归因没有报 empty txid"）。
    local deposit_started_at
    deposit_started_at=$(date +%s)

    # 4.5 上传 consignment 给 Go 桥（submitDeposit 需要；否则 pollTransfers 报
    # "consignment not provided"，铸造不触发）。端点为 base64。侧车对已 settle 的 receive
    # 幂等返回，故此处 200。
    local cons_b64 cons_up
    cons_b64=$(echo "${cons_hex}" | xxd -r -p | base64)
    cons_up=$(curl -s -X POST http://127.0.0.1:17000/rgbx/v1/consignment \
        -H 'Content-Type: application/json' \
        -d "{\"receiveId\":\"${receive_id}\",\"consignment\":\"${cons_b64}\"}")
    echo "${cons_up}" | grep -q '"code":200' || fail "rgb20 consignment upload failed: ${cons_up}"

    # 5. Go 桥 pollTransfers → submitDeposit（TSS 签 deposit）→ chain33 铸造 X.RGB20_USDT
    local delta expected
    delta=$(awk "BEGIN{printf \"%.8f\", ${RGB20_DEPOSIT_AMOUNT}/100000000}")
    expected=$(awk "BEGIN{printf \"%.8f\", ${before} + ${delta}}")
    wait_rgb20_balance_not_less_than "${USER_MAIN_ADDR}" "${expected}"
    log_step "RGB20 deposit OK: balance ${before} -> >= ${expected}"

    # 6. 首次归因必须一次成功。旧实现在首次归因分支里拿着 Settle() 之前的陈旧副本调
    #    BuildSpvProof，必然报 "build spv proof: empty txid"，要等下一轮 30s 轮询拿到
    #    settled 记录才自愈（三轮 run 均复现）。这里用日志窗口钉住"不再发生"。
    local deposit_logs
    deposit_logs=$(para1_logs_since "${deposit_started_at}")
    if echo "${deposit_logs}" | grep -q "build spv proof: empty txid"; then
        fail "deposit first attribution used a stale receive record: $(echo "${deposit_logs}" | grep -m1 'empty txid')"
    fi
    log_step "RGB20 deposit: first attribution succeeded without retry (no empty txid)"
}

# =====================================================================
# RGB20 提现 E2E
# =====================================================================

# rgb20_withdraw_once <amount_min_units> <label>：跑一遍完整提现（chain33 withdraw → 侧车
# BuildWithdrawal → TSS 组签 → 广播 → 确认 → 链上 burn），断言 pending 清空 + 余额正确递减。
function rgb20_withdraw_once() {
    local amount="$1"
    local label="${2:-withdraw}"
    local before after amt withdraw_hash expected

    before=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    # 1. 侧车 test-sim 创建用户发票（提现收款方）
    local user_invoice
    user_invoice=$(curl -s -X POST http://127.0.0.1:50064/sim/user_invoice \
        -H 'Content-Type: application/json' \
        -d "{\"asset_symbol\":\"${RGB20_SIDECAR_SYMBOL}\",\"amount\":${amount}}" | jq -r '.invoice // empty')
    assert_non_empty "${user_invoice}" "rgb20 user invoice empty (${label})"

    # 2. chain33 发起提现（destinationAddr=invoice；CLI 硬编码 *1e8，反算 -a 口径）
    amt=$(awk "BEGIN{printf \"%.8f\", ${amount}/100000000}")
    withdraw_hash=$(${MAIN_CLI} send rgbx withdraw -a "${amt}" -f 20 -d "${user_invoice}" -s "${RGB20_SYMBOL}" -k "${GENESIS_KEY}")
    assert_length "${withdraw_hash}" 66 "rgb20 withdraw tx hash (${label})"

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
    # change seal 金额 = 提现前侧车持仓 - 提现额（RGB 状态守恒：被花 seal 的面额 = 提现额 + 找零）
    local expect_change_min_unit=$((sidecar_before - RGB20_TWO_WITHDRAW_AMOUNT))
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

    # 1. 侧车 test-sim 发票：金额用精确的最小单位数
    user_invoice=$(curl -s -X POST http://127.0.0.1:50064/sim/user_invoice \
        -H 'Content-Type: application/json' \
        -d "{\"asset_symbol\":\"${RGB20_SIDECAR_SYMBOL}\",\"amount\":${RGB20_TRUNCATION_WITHDRAW_AMOUNT}}" | jq -r '.invoice // empty')
    assert_non_empty "${user_invoice}" "rgb20 truncation user invoice empty"

    # 2. chain33 发起提现（-a 口径 = min units / 1e8；旧实现下 ${RGB20_TRUNCATION_WITHDRAW_AMOUNT} 会少 1）
    amt=$(awk "BEGIN{printf \"%.8f\", ${RGB20_TRUNCATION_WITHDRAW_AMOUNT}/100000000}")
    withdraw_hash=$(${MAIN_CLI} send rgbx withdraw -a "${amt}" -f 20 -d "${user_invoice}" -s "${RGB20_SYMBOL}" -k "${GENESIS_KEY}")
    assert_length "${withdraw_hash}" 66 "rgb20 truncation withdraw tx hash"

    # 3. 断言 burn 额（链上 pending）== invoice 额。浮点截断时这里是 59999。
    burn_amount=$(wait_pending_withdraw_amount "${USER_MAIN_ADDR}" "${withdraw_hash}")
    assert_non_empty "${burn_amount}" "rgb20 truncation withdraw pending not found (${withdraw_hash})"
    assert_eq "${burn_amount}" "${RGB20_TRUNCATION_WITHDRAW_AMOUNT}" \
        "chain33 burn amount != invoice amount (CLI decimal->min unit conversion lost units)"

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

    log_step "RGB20 truncation withdraw OK: burn == invoice == payout == ${RGB20_TRUNCATION_WITHDRAW_AMOUNT} min units (retry logs=${retries})"
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
# 入口（testcase.sh 调用）
# =====================================================================

function run_rgb20_all() {
    run_rgb20_env
    run_rgb20_sidecar_genesis_withdraw_probe
    scenario_rgb20_deposit
    scenario_rgb20_withdraw
    scenario_rgb20_two_withdrawals
    scenario_rgb20_truncation_withdraw
    run_rgb20_sidecar_smoke
}
