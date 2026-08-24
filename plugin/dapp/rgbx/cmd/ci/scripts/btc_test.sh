function scenario_user_deposit_via_btc_tx() {
    log_step "scenario: user deposit via btc tx -> service auto submit deposit"
    local before_balance
    before_balance=$(query_xbtc_balance "${USER_MAIN_ADDR}")
    # make sure segwit is activated
    mine_btcd_blocks 450
    local utxo
    utxo=$(build_mature_coinbase_utxo)
    assert_non_empty "${utxo}" "funding utxo empty"

    local tss_addr
    tss_addr=$(${MAIN_CLI} rgbx getCross -s "${MINT_SYMBOL}" | jq -r '.tssAddress // empty')
    assert_non_empty "${tss_addr}" "tssAddress empty before deposit"

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
        --tssAddress "${tss_addr}" \
        --chain33Address "${USER_MAIN_ADDR}" \
        --amount "${BTC_DEPOSIT_AMOUNT_SATS}" \
        --fee 500)
    assert_length "${deposit_tx_hash}" 64 "btc deposit tx hash length mismatch"

    mine_btcd_blocks 2
    local expected_balance
    expected_balance=$(awk "BEGIN{printf \"%.8f\", ${before_balance}+${BTC_DEPOSIT_AMOUNT_SATS}/100000000}")
    wait_xbtc_balance_not_less_than "${USER_MAIN_ADDR}" "${expected_balance}"
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
    before=$(${MAIN_CLI} rgbx listPend -s 0 -i 0 -c 20 | jq -r '.pendingList | length')

    compose_cmd restart main >/dev/null
    wait_cli_ready "${MAIN_CLI}"
    save_seed_and_unlock "${MAIN_CLI}" || true

    local after
    after=$(${MAIN_CLI} rgbx listPend -s 0 -i 0 -c 20 | jq -r '.pendingList | length')
    assert_true "$([ "${after}" -ge 0 ] && echo true || echo false)" "pending list query failed after restart"
    log_step "pending continuity check before=${before}, after=${after}"
}

