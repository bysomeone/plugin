#!/usr/bin/env bash
# RGB20 全部测试（Phase 5）：环境初始化 + 充值 + 提现 + sidecar smoke，一个文件按函数组织。
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
        sleep 2
    done
    fail "rgb20 balance not reached, addr=${addr}, expected>=${expected}"
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

function run_rgb20_env() {
    log_step "RGB20 env: wait DKG -> issue USDT at GG18 script -> start sidecar"

    wait_rgb20_dkg_commit
    local pubkey
    pubkey=$(rgb20_tss_pubkey)
    assert_non_empty "${pubkey}" "rgb20 GG18 pubkey empty"
    log_step "GG18 pubkey=${pubkey}"
    export RGB20_TSS_PUBKEY="${pubkey}"

    # 停占位 pubkey 的侧车，清空其数据目录，用 GG18 公钥发行 USDT，再启动侧车。
    compose_cmd stop rgb-sidecar >/dev/null 2>&1 || true
    compose_cmd run --rm --no-deps rgb-sidecar sh -c 'rm -rf /data/* /data/.[!.]* 2>/dev/null || true' >/dev/null 2>&1 || true

    local issue_out
    issue_out=$(compose_cmd run --rm --no-deps \
        -e RGB_SIDECAR_TSS_PUBKEY="${pubkey}" \
        -e RGB_SIDECAR_ELECTRUM="rgb-electrs:60401" \
        -e RGB_BITCOIND_RPC="http://rgb-bitcoind:18443" \
        -e RGB_BITCOIND_USER="${RGB20_BITCOIND_USER:-rgb}" \
        -e RGB_BITCOIND_PASS="${RGB20_BITCOIND_PASS:-rgbpass123}" \
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

    # 5. Go 桥 pollTransfers → submitDeposit（TSS 签 deposit）→ chain33 铸造 X.RGB20_USDT
    local delta expected
    delta=$(awk "BEGIN{printf \"%.8f\", ${RGB20_DEPOSIT_AMOUNT}/100000000}")
    expected=$(awk "BEGIN{printf \"%.8f\", ${before} + ${delta}}")
    wait_rgb20_balance_not_less_than "${USER_MAIN_ADDR}" "${expected}"
    log_step "RGB20 deposit OK: balance ${before} -> >= ${expected}"
}

# =====================================================================
# RGB20 提现 E2E
# =====================================================================

function scenario_rgb20_withdraw() {
    log_step "scenario: RGB20 withdraw (chain33 withdraw -> sidecar BuildWithdrawal -> TSS signPsbt -> broadcast -> confirm -> burn)"
    local before
    before=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    assert_true "$(awk "BEGIN{print (${before} > 0)?\"true\":\"false\"}")" "rgb20 balance is zero before withdraw"

    # 1. 侧车 test-sim 创建用户发票（提现收款方）
    local user_invoice
    user_invoice=$(curl -s -X POST http://127.0.0.1:50064/sim/user_invoice \
        -H 'Content-Type: application/json' \
        -d "{\"asset_symbol\":\"${RGB20_SIDECAR_SYMBOL}\",\"amount\":${RGB20_WITHDRAW_AMOUNT}}" | jq -r '.invoice // empty')
    assert_non_empty "${user_invoice}" "rgb20 user invoice empty"

    # 2. chain33 发起提现（destinationAddr=invoice；CLI 硬编码 *1e8，反算 -a 口径）
    local amt withdraw_hash
    amt=$(awk "BEGIN{printf \"%.8f\", ${RGB20_WITHDRAW_AMOUNT}/100000000}")
    withdraw_hash=$(${MAIN_CLI} send rgbx withdraw -a "${amt}" -f 20 -d "${user_invoice}" -s "${RGB20_SYMBOL}" -k "${GENESIS_KEY}")
    assert_length "${withdraw_hash}" 66 "rgb20 withdraw tx hash"

    # 3. 等桥确认销毁（pending 清除 = rgbx Confirm 已提交）
    wait_no_withdraw_pending_for_user "${USER_MAIN_ADDR}"

    # 4. 断言余额减少（-a 口径 *1e8 = min units，显示口径 /1e8）
    local expected after
    expected=$(awk "BEGIN{printf \"%.8f\", ${before} - ${amt}}")
    after=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    assert_balance "${after}" "${expected}" "rgb20 balance not decreased after withdraw"
    log_step "RGB20 withdraw OK: balance ${before} -> ${after}"
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
# 入口（testcase.sh 调用）
# =====================================================================

function run_rgb20_all() {
    run_rgb20_env
    scenario_rgb20_deposit
    scenario_rgb20_withdraw
    run_rgb20_sidecar_smoke
}
