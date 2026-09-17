# P2WSH 充值（C1/C2）与扫集（C4）的 E2E 场景。
#
# 两个场景共用 p2wsh_deposit_once：领地址 -> 用户付 BTC 到派生的 P2WSH -> 桥按 program 归因 -> mint。
# 结果通过全局变量回传（不用 echo：函数里的 log_step 也写 stdout，命令替换会把日志一起captured）。

# 最近一次 p2wsh_deposit_once 的存款交易哈希与充值地址。
P2WSH_DEPOSIT_TX=""
P2WSH_DEPOSIT_ADDR=""

# p2wsh_deposit_once <amount_sats>：往该用户的 P2WSH 充值地址打一笔 BTC，并挖块确认。
function p2wsh_deposit_once() {
    local amount_sats="$1"
    local utxo
    # 必须同时满足"面额 >= amount+fee"（下面的 --amount/--fee）：链被复用到 coinbase 已减半多轮时，
    # 最新成熟块的面额可能不够，取到就会在 CLI 里以 "insufficient utxo amount" 失败。
    utxo=$(build_mature_coinbase_utxo "$((amount_sats + 500))")
    assert_non_empty "${utxo}" "funding utxo empty"

    # 充值地址 = P2WSH(chain33 地址, TSS 群公钥) 派生，链上按同一份派生认定归属（无 OP_RETURN）。
    local tss_pubkey
    tss_pubkey=$(${MAIN_CLI} rgbx getCrossChainInfo -s "${MINT_SYMBOL}" | jq -r '.pubkey // empty')
    assert_non_empty "${tss_pubkey}" "tss pubkey empty before deposit (checkCommitDKG must carry it)"

    # 桥必须先 watch 该用户的派生脚本才看得见这笔充值 —— 地址要**向桥要**（发放接口会按需 import），
    # 本地推导的地址（rgbx btcDepositAddress）仅供对账，桥不会因为本地推导就去 watch。
    # 桥侧需配置：neutrino.depositAddressListen（见 lightclient CONFIG.md）+ 把该端口暴露给本脚本。
    local bridge_deposit_url="${BRIDGE_DEPOSIT_URL:-http://127.0.0.1:17001/rgbx/v1/btc-deposit-address}"
    local deposit_addr
    # `|| true`：curl 连不上时（--fail 非 2xx / 连接被拒）赋值会因 set -e 直接结束整个 run，
    # 后面那句带排障提示的 assert 就永远看不到。这里让 assert 来报错，失败信息才有指向性。
    deposit_addr=$(curl -sf "${bridge_deposit_url}?chain33Addr=${USER_MAIN_ADDR}" | jq -r '.data.address // empty') || true
    assert_non_empty "${deposit_addr}" \
        "deposit address empty from ${bridge_deposit_url} (is neutrino.depositAddressListen configured and reachable?)"

    local deposit_tx_hash
    deposit_tx_hash=$(compose_cmd exec -T main /root/chain33-cli rgbx btcDepositTx \
        --net "${BTC_NETWORK}" \
        --rpcHost "${BTC_RPC_ADDR}" \
        --rpcUser "${BTCD_RPC_USER}" \
        --rpcPass "${BTCD_RPC_PASS}" \
        --disableTLS=false \
        --rpcCertFile "${BTCD_RPC_CERT_IN_CONTAINER}" \
        --wif "${BTC_FUNDING_WIF}" \
        --utxo "${utxo}" \
        --depositAddress "${USER_MAIN_ADDR}" \
        --tssPubkey "${tss_pubkey}" \
        --amount "${amount_sats}" \
        --fee 500)
    assert_length "${deposit_tx_hash}" 64 "btc deposit tx hash length mismatch"

    mine_btcd_blocks 2
    P2WSH_DEPOSIT_TX="${deposit_tx_hash}"
    P2WSH_DEPOSIT_ADDR="${deposit_addr}"
    log_step "p2wsh deposit sent: btcTx=${deposit_tx_hash} address=${deposit_addr} amount=${amount_sats}sats"
}

# 扫集相关：主池（TSS P2WPKH）地址 —— 扫集的归集目标。
function rgbx_main_pool_address() {
    ${MAIN_CLI} rgbx getCrossChainInfo -s "${MINT_SYMBOL}" | jq -r '.tssAddress // empty'
}

# 在付给 addr 的交易里，找一笔**花了 spent_txid** 的交易哈希（回显，找不到回显空）。
# 用主池地址查：扫集交易的输出付给主池，输入里包含那笔充值 —— 同时命中这两个条件的就是它。
function find_tx_spending_from_address() {
    local addr="$1"
    local spent_txid="$2"
    local txs
    txs=$(${BTC_CTL} --"${BTC_NETWORK}" searchrawtransactions "${addr}" 1 0 100 0 true 2>/dev/null) || true
    [ -n "${txs}" ] || return 0
    echo "${txs}" | jq -r --arg d "${spent_txid}" \
        '[.[] | select(any(.vin[]?; .txid == $d)) | .txid] | first // empty'
}

# 某笔交易付给 addr 的金额合计（sat）。
function tx_output_sats_to_address() {
    local txid="$1"
    local addr="$2"
    ${BTC_CTL} --"${BTC_NETWORK}" getrawtransaction "${txid}" 1 | jq -r --arg addr "${addr}" '
        [ .vout[]
          | select((((.scriptPubKey.addresses? // []) | index($addr)) != null) or (.scriptPubKey.address? == $addr))
          | (.value * 100000000 | floor) ] | add // 0'
}

function scenario_user_deposit_via_btc_tx() {
    log_step "scenario: user deposit via btc tx -> service auto submit deposit"
    local before_balance
    before_balance=$(query_xbtc_balance "${USER_MAIN_ADDR}")
    # make sure segwit is activated
    mine_btcd_blocks 450
    p2wsh_deposit_once "${BTC_DEPOSIT_AMOUNT_SATS}"
    local expected_balance
    expected_balance=$(awk "BEGIN{printf \"%.8f\", ${before_balance}+${BTC_DEPOSIT_AMOUNT_SATS}/100000000}")
    wait_xbtc_balance_not_less_than "${USER_MAIN_ADDR}" "${expected_balance}"
    # 成功行：本场景此前是唯一一个只靠断言静默通过的用例（绿了也没有任何输出），
    # 排障时无法从日志里确认"充值到账"这一步到底发生过没有。
    log_step "PASS: p2wsh deposit credited: user=${USER_MAIN_ADDR} amount=${BTC_DEPOSIT_AMOUNT_SATS}sats" \
        "btcTx=${P2WSH_DEPOSIT_TX} depositAddress=${P2WSH_DEPOSIT_ADDR} xbtc=${expected_balance}"
}

# 扫集（C4）：充值到 P2WSH -> 桥的闲时 ticker 归集回主池 -> 断言钱真的回到了主池。
#
# 为什么必须测它：P2WSH 充值地址上线后，用户充进来的 BTC **不再进主池**（停在各自的 P2WSH 上），
# 而提现只能花主池的 BTC。没有扫集，桥就是"钱进得去、出不来"。
function scenario_user_deposit_sweep() {
    log_step "scenario: sweep user P2WSH deposit back into the main pool"

    local main_pool_addr
    main_pool_addr=$(rgbx_main_pool_address)
    assert_non_empty "${main_pool_addr}" "tss (main pool) address empty in CrossChainInfo"

    # 一笔**本场景自己**的充值：断言只围绕它，不受前面场景遗留 UTXO（也会被扫掉）的干扰。
    p2wsh_deposit_once "${SWEEP_DEPOSIT_AMOUNT_SATS}"
    local deposit_tx="${P2WSH_DEPOSIT_TX}"
    local deposit_addr="${P2WSH_DEPOSIT_ADDR}"
    assert_non_empty "${deposit_tx}" "sweep scenario deposit tx empty"
    assert_non_empty "${deposit_addr}" "sweep scenario deposit address empty"

    # 等扫集发生：para1 的扫集 ticker（userDepositSweep.intervalSeconds=3，E2E 压到最小）构造 ->
    # TSS 签名 -> 广播。每轮挖一块推进确认（扫集要等充值确认够 minConfirmations）。
    local sweep_tx=""
    local i
    for ((i = 0; i < 60; i++)); do
        sweep_tx=$(find_tx_spending_from_address "${main_pool_addr}" "${deposit_tx}")
        if [ -n "${sweep_tx}" ]; then
            break
        fi
        mine_btcd_blocks 1
        sleep 2
    done
    assert_non_empty "${sweep_tx}" \
        "no sweep tx found: nothing paying the main pool (${main_pool_addr}) spends the deposit ${deposit_tx} \
         (is neutrino.userDepositSweep.enable set on the official node? see chain33.para1.toml)"

    # 1) 那笔充值 UTXO 必须真的被花掉（扫集动了这笔钱本身，而不是主池碰巧收到别的钱）。
    local deposit_vout
    deposit_vout=$(${BTC_CTL} --"${BTC_NETWORK}" getrawtransaction "${deposit_tx}" 1 | jq -r --arg a "${deposit_addr}" \
        '[.vout[] | select((.scriptPubKey.address? == $a) or (((.scriptPubKey.addresses? // []) | index($a)) != null)) | .n] | first')
    assert_non_empty "${deposit_vout}" "deposit output to ${deposit_addr} not found in ${deposit_tx}"
    local live
    live=$(${BTC_CTL} --"${BTC_NETWORK}" gettxout "${deposit_tx}" "${deposit_vout}" 2>/dev/null) || true
    assert_true "$([ -z "${live}" ] || [ "${live}" = "null" ] && echo true || echo false)" \
        "deposit utxo ${deposit_tx}:${deposit_vout} is still unspent after the sweep"

    # 2) 归集额至少覆盖本场景那笔（扣掉手续费）。注意这**不是**"等于充值额"：扫集会把当时所有
    #    待归集的充值 UTXO 合并成一笔（本场景之前的充值也可能还没被扫），所以下界比对才成立 ——
    #    只扫了本笔时 swept = 充值额 - 手续费；合并了别的则更大。
    local swept_sats
    swept_sats=$(tx_output_sats_to_address "${sweep_tx}" "${main_pool_addr}")
    assert_non_empty "${swept_sats}" "sweep tx ${sweep_tx} has no output to the main pool"
    assert_true "$([ "${swept_sats}" -ge $((SWEEP_DEPOSIT_AMOUNT_SATS - 10000)) ] && echo true || echo false)" \
        "swept amount too small: deposited=${SWEEP_DEPOSIT_AMOUNT_SATS} swept=${swept_sats} (sweep=${sweep_tx})"

    # 3) 不变式（规格 §2.3(b)）：桥的任何自有付款都不得落到用户 P2WSH —— 扫集输出回到充值脚本，
    #    就会被该用户回头当成充值证明再认领一次（同一笔 BTC 两边入账）。
    local back_to_user
    back_to_user=$(tx_output_sats_to_address "${sweep_tx}" "${deposit_addr}")
    assert_eq "${back_to_user}" "0" "sweep must not pay back into the user deposit script"

    # 4) 主池现在持有这笔钱 —— 这正是"充值进来的 BTC 可以被提现花掉"的前提。
    local pool_balance
    pool_balance=$(query_latest_received_sats "${main_pool_addr}")
    assert_non_empty "${pool_balance}" "main pool balance query failed"
    log_step "PASS: sweep landed: sweepTx=${sweep_tx} depositTx=${deposit_tx} depositAddress=${deposit_addr}" \
        "sweptToMainPool=${swept_sats}sats mainPool=${main_pool_addr} mainPoolLatestReceived=${pool_balance}sats"
}

function scenario_user_transfer_crosschain_asset() {
    log_step "scenario: user A transfer cross-chain asset(XBTC) to user B on mainchain"
    local before_a
    local before_b
    before_a=$(query_xbtc_balance "${USER_MAIN_ADDR}")
    before_b=$(query_xbtc_balance "${USER_B_ADDR}")

    local transfer_hash
    local xbtc_transfer_amount
    xbtc_transfer_amount=$(awk "BEGIN{printf \"%.8f\", ${XBTC_TRANSFER_AMOUNT}/100000000}")
    transfer_hash=$(${MAIN_CLI} send rgbx transfer -a "${xbtc_transfer_amount}" -s XBTC \
        -t "${USER_B_ADDR}" -k "${GENESIS_KEY}")
    assert_length "${transfer_hash}" 66 "transfer tx hash"
    # tx_wait "${MAIN_CLI}" "${transfer_hash}"

    local after_a
    local after_b
    after_a=$(query_xbtc_balance "${USER_MAIN_ADDR}")
    after_b=$(query_xbtc_balance "${USER_B_ADDR}")
    expected_a=$(awk "BEGIN{printf \"%.4f\", ${before_a} - ${xbtc_transfer_amount}}")
    expected_b=$(awk "BEGIN{printf \"%.4f\", ${before_b} + ${xbtc_transfer_amount}}")
    assert_balance "${after_a}" "${expected_a}" "user A xbtc not decreased after transfer"
    assert_balance "${after_b}" "${expected_b}" "user B xbtc not increased after transfer"
}

function scenario_user_withdraw_auto_confirm() {
    log_step "scenario: user withdraw on mainchain -> service auto confirm"
    local before_balance
    before_balance=$(query_xbtc_balance "${USER_MAIN_ADDR}")

    local withdraw_hash
    local btc_withdraw_amount
    btc_withdraw_amount=$(awk "BEGIN{printf \"%.8f\", ${BTC_WITHDRAW_AMOUNT_SATS}/100000000}")
    withdraw_hash=$(${MAIN_CLI} send rgbx withdraw -a "${btc_withdraw_amount}" -f "${BTC_WITHDRAW_FEE_RATE}" \
        -d "${WITHDRAW_DEST_ADDR}" -s "${MINT_SYMBOL}" -k "${GENESIS_KEY}")
    assert_length "${withdraw_hash}" 66
    # tx_wait "${MAIN_CLI}" "${withdraw_hash}"
    sleep 10 # wait for withdraw tx to be committed
    wait_no_withdraw_pending_for_user "${USER_MAIN_ADDR}"
    received_sats=$(query_latest_received_sats "${WITHDRAW_DEST_ADDR}")
    local expected_received=$((BTC_WITHDRAW_AMOUNT_SATS - 5000))
    # 允许 ±1000 sats 的误差
    diff=$((received_sats - expected_received))
    if ((diff < 0)); then diff=$((-diff)); fi
    if ((diff >= 1000)); then
        fail "btc withdraw amount mismatch, expect≈${expected_received}, actual=${received_sats}"
    fi

    local after_balance
    after_balance=$(query_xbtc_balance "${USER_MAIN_ADDR}")
    expected_balance=$(awk "BEGIN{printf \"%.4f\", ${before_balance} - ${btc_withdraw_amount}}")
    assert_balance "${after_balance}" "${expected_balance}" "xbtc balance not decreased after withdraw settle"
}

function scenario_restart_recovery() {
    log_step "scenario: restart recovery and pending continuity"
    local before
    before=$(${MAIN_CLI} rgbx listPendingTx -s 0 -i 0 -c 20 | jq -r '.pendingList | length')

    compose_cmd restart main >/dev/null
    wait_cli_ready "${MAIN_CLI}"
    save_seed_and_unlock "${MAIN_CLI}" || true

    local after
    after=$(${MAIN_CLI} rgbx listPendingTx -s 0 -i 0 -c 20 | jq -r '.pendingList | length')
    assert_true "$([ "${after}" -ge 0 ] && echo true || echo false)" "pending list query failed after restart"
    log_step "pending continuity check before=${before}, after=${after}"
}
