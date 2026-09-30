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
source "${ROOT_DIR}/scripts/backup_test.sh"

function testcase_entry() {
    # ---- 旧 BTC 功能用例（Phase 5 屏蔽，保留函数定义；如需启用去掉注释）----
    # 屏蔽的是 transfer / 旧 BTC 提现 / restart 三条：BTC 提现已迁到 RGB20 侧
    # （见下面的 scenario_rgb20_withdraw，E11/E9/E9-A 覆盖），旧用例断言的是 P2WPKH 时代的路径。
    # run_btc_functional_all

    # ---- RGB20 全部：env + 充值 + 提现 + sidecar smoke ----
    #
    # BTC-only 演示模式（RGBX_BTC_ONLY=1）：跳过 RGB20 的场景断言，但**环境准备照跑** ——
    # run_rgb20_env 里还有 BTC 也依赖的东西（segwit 激活 / DKG 等待 / TSS 注资 / 起侧车），
    # 整段跳掉会让下面的 E6(b) 报"主链头链还没提交过头"（实测）。
    # 备份演练也跳过：它的提现腿走的是 RGB20。
    # 剩下的（头链护栏 / P2WSH 充值 / 扫集 / native mint / E14 负例）就是 BTC 那条轨道。
    if [ "${RGBX_BTC_ONLY}" = "1" ]; then
        log_step "BTC-only mode: RGB20 env runs as usual, RGB20 scenarios skipped"
        run_rgb20_env
        export RGBX_BACKUP_DRILL=0
    else
        run_rgb20_all
    fi

    # ---- E6(b)：BTC 头查询的共识权威校验必须"武装 + 在活跃路径上服务" ----
    # 放在 RGB20 之后：本场景直接查主链头链（lightcli btc last/header），要求头链已提交。
    scenario_btc_header_guard_armed

    # ---- P2WSH 充值（C1/C2 新流程）：向桥领派生地址 -> 用户付 BTC -> 桥按 program 归因 -> mint XBTC ----
    # 放在 RGB20 之后：本场景是新增覆盖（此前一直屏蔽），万一卡住也不该吞掉前面已验证过的用例结果。
    scenario_user_deposit_via_btc_tx

    # ---- 扫集（C4）：把上面那笔充值（以及本场景自己那笔）归集回主池 ----
    # 没有扫集，P2WSH 充值地址就是"钱进得去、出不来"：钱停在用户各自的 P2WSH 上，
    # 而提现只能花主池的 BTC。
    scenario_user_deposit_sweep

    # ---- BTC-only 演示模式：把被屏蔽的两条也启用 ----
    # 两条都属于 run_btc_functional_all 那批（Phase 5 起屏蔽，旧断言针对 P2WPKH 时代），
    # 但演示都要用：链上流转（用户之间转 XBTC）与提现回 BTC。这里单点启用。
    # 放在扫集之后：提现花的是主池的 BTC，而扫集刚把用户充值归集回主池。
    if [ "${RGBX_BTC_ONLY}" = "1" ]; then
        scenario_user_transfer_crosschain_asset
        scenario_user_withdraw_auto_confirm
    fi

    # ---- 上游 native asset 场景（合并 upstream 时保留）：纯 chain33 原生资产 mint
    # + btcMintSpend 的 OP_RETURN 承诺确认。放最后：它覆盖的是**链上原生资产**这条
    # 与 BTC 桥充值不同的路径，失败不影响前面已验过的过桥用例；合并确认路径
    # （checkConfirm 改以 merkle 认证过的交易为准）后需要它来证明旧路径没被改坏。
    scenario_native_asset_mint

    # ---- E14 主动负例：把上面那笔 mint 的确认**再提交一次**，断言被去重护栏拒绝 ----
    # 依赖 scenario_native_asset_mint 导出的 NATIVE_MINT_TX_HASH（在它成功分支里赋值）。
    # 放最后：它需要一笔"已经确认结算过"的 mint，且失败不该吞掉前面已验过的用例结果。
    scenario_duplicate_confirm_rejected "${NATIVE_MINT_TX_HASH}"

    # ---- 备份/恢复演练（#30 / BACKUP_RECOVERY.md G8）：备 → 真的销毁 → 还原 → 充值 + 提现 ----
    # 放在**整个入口的最后**：它会销毁并还原 main/para1-4/侧车的全部数据（含主链、TSS share、
    # libp2p 身份私钥、侧车账本），失败时环境停在"已销毁"态 —— 那会盖掉后面所有用例的结果，
    # 所以它必须是最后一个。跳过方式：RGBX_BACKUP_DRILL=0。
    run_backup_drill_all
}

# 供 scripts/btc_test.sh 顶层分组调用（当前未启用）。
function run_btc_functional_all() {
    scenario_user_deposit_via_btc_tx
    scenario_user_deposit_sweep
    scenario_user_transfer_crosschain_asset
    scenario_user_withdraw_auto_confirm
    scenario_restart_recovery
}
