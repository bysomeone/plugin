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

# 本轮这笔提现在链上的 pending 金额（= CLI 换算出的最小单位数，= 实际 burn 额）。
function query_pending_withdraw_amount() {
    local from_addr="$1"
    local tx_hash="$2"
    ${MAIN_CLI} rgbx listPendByFrom -f "${from_addr}" |
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
        info=$(${MAIN_CLI} rgbx getCross -s "${RGB20_SYMBOL}" 2>/dev/null)
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
    ${MAIN_CLI} rgbx getCross -s "${RGB20_SYMBOL}" | jq -r '.pubkey // empty'
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

    compose_cmd up -d rgb-sidecar
    wait_rgb20_sidecar_grpc
    log_step "RGB20 env done: sidecar up (GG18 pubkey)"
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
# 因此本场景同时校验"两笔都成功 burn + 余额递减 + listPendByFrom 清空"与"第二笔的输入确实是
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
    tss_address=$(${MAIN_CLI} rgbx getCross -s "${RGB20_SYMBOL}" | jq -r '.tssAddress // empty')
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
