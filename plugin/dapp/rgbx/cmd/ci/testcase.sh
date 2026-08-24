#!/usr/bin/env bash
# RGB20 测试入口（由 docker-compose.sh run_tests 通过 source 调用）。
#
# 分工：
#   - 环境初始化（chain33 主链 + 4 para TSS + btcd + DKG）由 docker-compose.sh do_up_only / run_tests 完成；
#   - 本入口只负责：屏蔽旧 BTC 功能用例 + RGB20 全部（env/充值/提现/sidecar smoke）。
#
# 说明：脚本通过 source 引入（同一进程），可复用 docker-compose.sh 定义的辅助
# （MAIN_CLI/PARA1_CLI/compose_cmd/assert_*/log_step/wait_* 等）。

source "${ROOT_DIR}/scripts/btc_test.sh"
source "${ROOT_DIR}/scripts/rgb20_test.sh"

function testcase_entry() {
    # ---- 旧 BTC 跨链功能用例（Phase 5 屏蔽，保留函数定义；如需启用去掉注释）----
    # run_btc_functional_all

    # ---- RGB20 全部：env + 充值 + 提现 + sidecar smoke ----
    run_rgb20_all
}

# 供 scripts/btc_test.sh 顶层分组调用（当前未启用）。
function run_btc_functional_all() {
    scenario_user_deposit_via_btc_tx
    scenario_user_transfer_crosschain_asset
    scenario_user_withdraw_auto_confirm
    scenario_restart_recovery
}
