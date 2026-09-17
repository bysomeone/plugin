#!/usr/bin/env bash
# RGB20 测试入口（由 docker-compose.sh run_tests 通过 source 调用）。
#
# 分工：
#   - 环境初始化（chain33 主链 + 4 para TSS + btcd + DKG）由 docker-compose.sh do_up_only / run_tests 完成；
#   - 本入口负责：RGB20 全部（env/充值/提现/sidecar smoke）+ P2WSH 充值（C1/C2）+ 屏蔽旧 BTC 功能用例。
#
# 说明：脚本通过 source 引入（同一进程），可复用 docker-compose.sh 定义的辅助
# （MAIN_CLI/PARA1_CLI/compose_cmd/assert_*/log_step/wait_* 等）。

source "${ROOT_DIR}/scripts/btc_test.sh"
source "${ROOT_DIR}/scripts/rgb20_test.sh"

function testcase_entry() {
    # ---- 旧 BTC 功能用例（Phase 5 屏蔽，保留函数定义；如需启用去掉注释）----
    # 屏蔽的是 transfer / 旧 BTC 提现 / restart 三条：BTC 提现已迁到 RGB20 侧
    # （见下面的 scenario_rgb20_withdraw，E11/E9/E9-A 覆盖），旧用例断言的是 P2WPKH 时代的路径。
    # run_btc_functional_all

    # ---- RGB20 全部：env + 充值 + 提现 + sidecar smoke ----
    run_rgb20_all

    # ---- P2WSH 充值（C1/C2 新流程）：向桥领派生地址 -> 用户付 BTC -> 桥按 program 归因 -> mint XBTC ----
    # 放在 RGB20 之后：本场景是新增覆盖（此前一直屏蔽），万一卡住也不该吞掉前面已验证过的用例结果。
    scenario_user_deposit_via_btc_tx

    # ---- 扫集（C4）：把上面那笔充值（以及本场景自己那笔）归集回主池 ----
    # 没有扫集，P2WSH 充值地址就是"钱进得去、出不来"：钱停在用户各自的 P2WSH 上，
    # 而提现只能花主池的 BTC。
    scenario_user_deposit_sweep
}

# 供 scripts/btc_test.sh 顶层分组调用（当前未启用）。
function run_btc_functional_all() {
    scenario_user_deposit_via_btc_tx
    scenario_user_deposit_sweep
    scenario_user_transfer_crosschain_asset
    scenario_user_withdraw_auto_confirm
    scenario_restart_recovery
}
