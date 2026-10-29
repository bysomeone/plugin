#!/usr/bin/env bash
# 备份/恢复演练（任务 #30 第一步；BACKUP_RECOVERY.md 的缺口 G8）。
#
# 一句话：**备 → 真的销毁 → 还原 → 充一笔、提一笔**。备份从没被验证过 = 等于没有备份，
# 所以本场景的判据不是"脚本退出码是 0"，而是：
#   ① 还原后账本/余额与备份前**逐项相等**（证明是"还原"，不是"重建出一个空壳"）；
#   ② 还原后**能真的用**：一笔充值铸出来、一笔提现烧掉（阈值签名要成立 ⇒ B1 share 与
#      B5 libp2p 身份都是同一代；侧车构建提现要成立 ⇒ B2 侧车状态完整）。
#
# 销毁范围（"真的销毁"，不是做样子）：main / para1-4 / rgb-sidecar 六个数据卷的内容全部删掉
#   - paraN 的 lightclient/（B1 share + B3 业务 bucket + btcwallet.db 生日。**注意 btcwallet.db 只有
#     官方节点 para1 有**：IsOfficialNode 才建 neutrino ChainService + btcwallet，para2-4 是纯签名节点）
#   - paraN 的 addrbook/（**B5 libp2p 身份私钥**：peer id 由它派生，丢了节点就变成"新成员"）
#   - paraN 的 parawallet/（**authAccount 私钥**：para 共识签名钥 + TSS CommitDKG 提交钥；harness 已把它
#     放在 datadir 卷里 —— 卷外时它不在备份集内，这正是第一轮 E2E 发现的缺口之一）
#   - main 的 datadir（B4：CI 与生产同为单主节点，链状态没有副本）
#   - 侧车 /data（B2：RGB 是客户端验证，链上查不回来）
# **不动 btcd**：regtest 每次启动无条件清链，它本来就不是可恢复对象（BACKUP_RECOVERY.md §2.4）；
# 反过来说，一旦把 btcd 重启/清链，chain33 已提交的 BTC 头就错位了（§1.3），所以本场景全程不碰它。
#
# 为什么放在 testcase_entry 的**最后**：本场景会销毁并还原整个环境，失败时环境停在"已销毁"态；
# 放最后可以让它的失败不吞掉前面已验过的用例结果。
#
# 与冷备脚本的分工（重要）：数据在**命名卷**里，只有容器能看见；而 compose 只有宿主能操作。
# 所以 CI 里"停/起"由 harness 做（宿主侧），"打包/自检/还原/复核"由脚本在辅助容器里做
# （见 scripts/cold_backup.sh 的两种用法）。生产是单机场景，停起钩子与数据目录在同一台机器上，
# 由脚本自己按钩子停起（脚本头部的用法示例）。
#
# 可关：RGBX_BACKUP_DRILL=0（调试其它场景时跳过本场景）。
#
# =====================================================================
# 失败症状反查表（脚本坏了时 E2E 会怎么表现）
# =====================================================================
#   A. 找不到备份脚本（脚本没被带进 CI 目录、或 RGBX_BACKUP_SCRIPTS_DIR 没设）
#      → fail "backup scripts not found"（预检阶段就停，环境不受影响）
#   A2. 脚本在但参数契约漂了（例如给某一边传了它不认的选项）
#      → **销毁之前**的 dry-run 就 fail："restore --dry-run failed … BEFORE anything was destroyed:
#        the arguments/package are not restorable"（脚本自己会打 `unknown argument: --xxx`）。
#        这条规矩是花一轮 E2E 买来的：第一轮跑到"已销毁"才发现恢复侧不认 `--required` ⇒ 白毁一轮。
#   A3. 忘了"写者已停"的证据（既没给 *_STOPPED_CHECK_CMD 也没 --trust-caller-stopped）
#      → 脚本拒绝开工："refusing to package without a 'writers are stopped' check" /
#        "refusing to write into the targets without a 'writers are stopped' check"
#        （自检只能证明包没坏，证明不了取样的那一刻没有进程在写）
#   B. 冷备在预检就失败（源目录没有文件 / 必需成员缺失 / 输出目录不可写）
#      → 报 "[backup][ERROR] source <label> ... missing required members"，环境仍在停着的状态，
#        随即演练自己会把它起回来（见 drill_start_services 的调用点）
#   C. 打包后自检失败（归档截断 / 校验和对不上）
#      → 报 "[backup][ERROR] self-check ..."；**没有可用的包**，演练 fail
#   D. 销毁没真的发生（find -delete 失败或路径写错）→ "post-destroy 文件数 != 0" 断言先失败
#   E. 还原失败（包坏了 / 目标写不进去 / 复核不一致）
#      → "[backup][ERROR] ... refusing to touch the target directory" 或 "post-restore verification FAILED"
#   F. 还原成功但**状态是空的**（例如只还原了侧车、没还原 node datadir）
#      → 步骤 6 的账本/余额比对失败：打印 before/after 两组数字，一眼看出哪一项没回来
#   F2. 还原成功但**钱包身份没回来**（parawallet / btcwallet 等按角色区分的成员漏了）
#      → 起进程后、**重新导入密钥之前**的断言先失败：
#        "wallet identity (paraN after restore): <AUTH 地址> NOT in the wallet"
#        （它同时说明 required 清单与真实容器布局不一致 —— 第一轮就是这样抓出 para2-4 没有 btcwallet.db 的）
#   G. 还原成功但**桥不能用**（share 不同代 / peer id 变了 / 侧车 Stock 坏了）
#      → wait_bridge_signing_ready 超时，或充值卡在 wait_rgb20_balance_not_less_than，
#        para1 日志里有 TSS/签名相关错误（share 自检 panic 会直接拒绝启动，节点起不来）
#   H. 演练把环境留在"已销毁"状态（还原失败且服务已起）→ 需要 ./docker-compose.sh reset 重跑
# =====================================================================

# 六个数据组件。label 同时用于：卷名后缀、辅助容器里的挂载点 /src/<label>、清单里的成员前缀。
BACKUP_DRILL_LABELS="${BACKUP_DRILL_LABELS:-main para1 para2 para3 para4 sidecar}"

# 每个 label 的关键成员（= 冷备的 --required；路径相对**卷根**）。
#
# ⚠️ 关键成员**按节点角色区分** —— 这一点是第一轮 E2E 用真实的容器取证暴露出来的：`btcwallet.db`
# 只存在于官方节点。依据（读代码）：
#
#   - `IsOfficialNode` 才创建 neutrino ChainService + btcwallet（neutrino/client.go:132-140、232-240、
#     300-315，三个点都被 `if !n.cfg.IsOfficialNode { return }` 挡住）⇒ **只有 para1 有
#     lightclient/btcwallet.db**（watch-only 钱包：地址管理器 + tx store + **生日**，丢了历史充值永久
#     看不见，BACKUP_RECOVERY.md §2.3 把它列为 P0）。
#   - para2-4 是**纯签名节点**：只跑 rgb20 adapter（它的业务 bucket 同样落在 lightclient/neutrino.db 里）
#     + 用户充值脚本 watch 集（`n.deposits` 在所有节点上建，client.go:129-133），不跑钱包、不跑 SPV 链。
#   - **所有** para 节点都有 `addrbook/dht`（B5：libp2p 身份私钥，peer id 由它派生）与
#     `parawallet`（authAccount 私钥：para 共识签名钥 + TSS CommitDKG 提交钥。harness 已把它放进
#     datadir 卷，见 docker-compose.sh 的 config_para_file —— 第一轮 E2E 之前它在卷外，等于不在备份集里）。
#   - `main` 只跑 lightclient/rgbx **执行器**（不跑中继/钱包），所以是主链那三个配置推导出来的路径
#     （`[store] dbPath="datadir/mavltree"`、`[wallet] dbPath="datadir/wallet"`、
#     `[p2p] dbPath="datadir/addrbook"`）。
#   - 侧车：`ledger.json` + `stock`（§1.2 的清单里，`builds/` 与 `user_scripts.json` 可能在首次
#     充值/领地址之前还不存在，所以不列为必需）。
#
# 若预检报 `paraN:parawallet` 缺失：说明 para toml 是**旧版生成**的（钱包还在 /root/parawallet，
# 卷外 ⇒ 不在备份集内）。重新生成配置即可（`make depends` 后 `./docker-compose.sh run`），
# 见 config_para_file 里那段注释。
function drill_required_args() {
    local label
    for label in ${BACKUP_DRILL_LABELS}; do
        case "${label}" in
        main)
            printf '%s\n' "main:mavltree" "main:wallet" "main:addrbook/dht"
            ;;
        para1)
            # 官方节点：比验证节点多一个 watch-only 钱包
            printf '%s\n' "para1:lightclient/neutrino.db" "para1:lightclient/btcwallet.db" \
                "para1:addrbook/dht" "para1:parawallet"
            ;;
        para2 | para3 | para4)
            # 验证节点：没有 btcwallet.db（不跑钱包）
            printf '%s\n' "${label}:lightclient/neutrino.db" "${label}:addrbook/dht" "${label}:parawallet"
            ;;
        sidecar)
            printf '%s\n' "sidecar:ledger.json" "sidecar:stock"
            ;;
        esac
    done
}

function drill_service_name() {
    case "$1" in
    sidecar) echo "rgb-sidecar" ;;
    *) echo "$1" ;;
    esac
}

function drill_volume_suffix() {
    case "$1" in
    sidecar) echo "rgb-sidecar-data" ;;
    *) echo "$1-data" ;;
    esac
}

function drill_volume_name() {
    echo "${COMPOSE_PROJECT_NAME}_$(drill_volume_suffix "$1")"
}

function drill_services_list() {
    local label out=""
    for label in ${BACKUP_DRILL_LABELS}; do
        out="${out} $(drill_service_name "${label}")"
    done
    echo "${out# }"
}

# 备份脚本所在目录：优先 env 覆盖 → 仓库里 script 的正式位置（组件旁边，与 BACKUP_RECOVERY.md 同目录）
# → 万一将来被拷进 CI 目录（${ROOT_DIR}/scripts）。三者都找不到就 fail（不要"静默跳过演练"）。
function drill_scripts_dir() {
    local repo_root=""
    if command -v git >/dev/null 2>&1; then
        repo_root=$(git -C "${ROOT_DIR}" rev-parse --show-toplevel 2>/dev/null || true)
    fi
    local candidates=(
        "${RGBX_BACKUP_SCRIPTS_DIR:-}"
        "${repo_root}/plugin/dapp/lightclient/rpc/lightclient/neutrino/scripts"
        "${ROOT_DIR}/scripts"
    )
    local c
    for c in "${candidates[@]}"; do
        [ -n "${c}" ] || continue
        if [ -x "${c}/cold_backup.sh" ] && [ -x "${c}/restore_backup.sh" ]; then
            echo "${c}"
            return 0
        fi
    done
    fail "backup scripts not found (looked in: ${candidates[*]}); set RGBX_BACKUP_SCRIPTS_DIR to the directory holding cold_backup.sh"
}

# drill_run <ro|rw> <shell 片段>：在辅助容器里跑一段 shell，六个数据卷都挂在 /src/<label>。
# 用 compose 的 main 服务（chain33 镜像 = ubuntu + bash/coreutils/tar，且已在本地构建好，不需要联网拉镜像）。
function drill_run() {
    local mode="$1"
    shift
    local args=()
    local label
    for label in ${BACKUP_DRILL_LABELS}; do
        args+=(-v "$(drill_volume_name "${label}"):/src/${label}:${mode}")
    done
    compose_cmd run --rm --no-deps -T "${args[@]}" main bash -c "$*"
}

# drill_preflight_scripts：**在停服/销毁之前**先把两个脚本本身验一遍（存在、可执行、参数被接受）。
#
# 这条规矩是花一轮 E2E 买来的（2026-09-21）：第一轮跑到"已销毁"才发现 restore_backup.sh 不认
# `--required`（演练把同一组参数同时传给了两个脚本）⇒ 白毁一轮（CI 能 reset，生产演练就是事故）。
# 固化下来：**破坏性步骤之前先验证后续步骤可执行**。
#   - 两个脚本的 --help 必须能跑（证明脚本在位、语法没问题）；
#   - 恢复侧还要做一次 `--dry-run`（只读、完整校验：读包 + 逐文件核对 + 必需成员 + 每个 label 有目标），
#     这一步在**销毁之前**跑，所以"后续能不能执行"是拿产物验过的，不是猜的。
function drill_preflight_scripts() {
    local script
    for script in cold_backup.sh restore_backup.sh; do
        # 在**宿主**上跑 --help：脚本目录是 bind mount 进容器的，宿主跑得通即"存在 + 参数解析器能起来"。
        # 容器侧那一路由后面的 restore --dry-run 完整验证（它真的在容器里、拿产物跑一遍）。
        bash "${DRILL_SCRIPT_DIR}/${script}" --help >/dev/null 2>&1 ||
            fail "backup script not runnable: ${DRILL_SCRIPT_DIR}/${script} --help failed"
    done
    log_step "preflight: both backup scripts are runnable (--help OK on the host; container side is checked by the restore --dry-run)"
}

# drill_run_script <ro|rw> <脚本名> [参数...]：在辅助容器里跑脚本（六个卷挂在 /src/<label>，
# 另外挂脚本目录与产物目录）。参数由调用方传（**调用方用同一个数组展开两次**跑 dry-run 与真跑，
# 保证"验过的"与"要执行的"逐字一致）。
#
# 注意不要用 `${!arr[@]}` 做数组间接展开：那是**取下标**（bash 的 `${!arr[@]}` = indices），
# 而 `local -n`（nameref）在 macOS 自带的 bash 3.2 上不存在 —— harness 是在宿主上跑的，
# 所以这里只收普通参数、"$@" 转发（3.2 兼容）。
function drill_run_script() {
    local mode="$1"
    local script="$2"
    shift 2
    local args=()
    local label
    for label in ${BACKUP_DRILL_LABELS}; do
        args+=(-v "$(drill_volume_name "${label}"):/src/${label}:${mode}")
    done
    compose_cmd run --rm --no-deps -T "${args[@]}" \
        -v "${DRILL_SCRIPT_DIR}:/scripts:ro" \
        -v "${DRILL_OUT_DIR}:/out" \
        main bash "/scripts/${script}" "$@"
}

# 卷里"还剩几个文件"（用于断言销毁真的发生了 / 断言还原前确实是空的）。
function drill_count_files() {
    drill_run ro 'find /src -type f | wc -l' | tr -d ' \r'
}

# 六个数据卷的内容全删（**真的销毁**）。btcd 不在其中（见文件头）。
function drill_destroy_state() {
    log_step "destroy: wipe the contents of all six data volumes (main/para1-4/sidecar)"
    drill_run rw 'for d in /src/*; do find "$d" -mindepth 1 -delete; done; echo destroyed' >/dev/null ||
        fail "failed to wipe the data volumes"
    local left
    left=$(drill_count_files)
    [ "${left}" = "0" ] || fail "destroy did not take effect: ${left} file(s) still present in the data volumes"
    log_step "destroy verified: 0 file(s) left in the six data volumes"
}

function drill_stop_services() {
    log_step "stop the six data-writing services (btcd stays up on purpose)"
    # shellcheck disable=SC2086
    compose_cmd stop $(drill_services_list) >/dev/null
    local svc cid
    for svc in $(drill_services_list); do
        # `ps -q` 默认只列运行中的容器（v1/v2 都是），因此非空 = 还活着。
        cid=$(compose_cmd ps -q "${svc}" 2>/dev/null | head -n 1)
        [ -z "${cid}" ] || fail "service ${svc} is still running after stop (cid=${cid}): the cold sample would not be consistent"
    done
    log_step "stop verified: all six services are down (btcd untouched)"
}

function drill_start_services() {
    log_step "start the six services again"
    # shellcheck disable=SC2086
    compose_cmd start $(drill_services_list) >/dev/null
}

# 钱包身份断言：authAccount（AUTH_ADDRn）必须在钱包的账户列表里。
#
# 这条是**在重新导入任何密钥之前**跑的，所以它证明的是"钱包是从包里还原回来的"，而不是"后面 import
# 兜住了"。AUTH 地址背后那把钥就是 para 共识签名钥（commitMsgClient.fetchPriKey → wallet.DumpPrivkey）
# 与 TSS CommitDKG 的提交钥 —— 丢了它节点就再也无法以自己注册在链上的地址签名。
#
# 注意：account list 只读地址、不需要解锁，因此可以在 unlock 之前调用。
function drill_assert_wallet_identity() {
    local cli="$1"
    local addr="$2"
    local when="$3"
    if ${cli} account list 2>/dev/null | grep -q -- "${addr}"; then
        log_step "wallet identity (${when}): ${addr} present"
        return 0
    fi
    fail "wallet identity (${when}): ${addr} NOT in the wallet — the account key (para consensus + TSS commit) is missing"
}

# 还原后的状态回归：CLI 就绪 → **钱包身份还原** → 钱包解锁 → DKG/桥就绪。
function drill_wait_env_ready() {
    wait_cli_ready "${MAIN_CLI}"
    wait_cli_ready "${PARA1_CLI}"
    wait_cli_ready "${PARA2_CLI}"
    wait_cli_ready "${PARA3_CLI}"
    wait_cli_ready "${PARA4_CLI}"
    # The drill compares the peer-name set before/after the restore. DHT reconnection after the
    # restart is asynchronous: sampling immediately can miss the last peer and fail the identity
    # assertion even though every identity key came back. Wait for convergence first.
    wait_para_dht_discovery
    # 钱包身份先断言（此时还没有任何 seed/import 动作），再解锁 + 幂等重导入 AUTH 私钥
    # （与 init 阶段重启 para 后要做的完全一样；否则 TSS 拿不到 commit 私钥）。
    drill_assert_wallet_identity "${PARA1_CLI}" "${AUTH_ADDR1}" "para1 after restore"
    drill_assert_wallet_identity "${PARA2_CLI}" "${AUTH_ADDR2}" "para2 after restore"
    drill_assert_wallet_identity "${PARA3_CLI}" "${AUTH_ADDR3}" "para3 after restore"
    drill_assert_wallet_identity "${PARA4_CLI}" "${AUTH_ADDR4}" "para4 after restore"
    prepare_accounts
    wait_auto_dkg_commit
    wait_rgb20_sidecar_grpc
    wait_bridge_signing_ready
}

# 侧车账本的"可观测指纹"：资产 id 集合 + seal 数 + 可花费额。用来断言"账本真的回来了"。
function drill_ledger_fingerprint() {
    local json
    json=$(rgb20_ledger_json)
    if [ -z "${json}" ]; then
        echo "unreadable"
        return 0
    fi
    echo "${json}" | jq -r '"assets=" + ((.assets // {}) | keys | join(",")) + " seals=" + ((.seals // {}) | length | tostring) + " spendable=" + (([.seals[]? | select((.status | ascii_downcase) != "consumed") | .amount] | add // 0) | tostring)'
}

# display 口径的余额（"0.0004"）→ 最小单位整数（40000）。
# 换算系数 1e8 是 chain33 侧的统一口径：withdraw 的 -a 也是 display 口径，rgb20_withdraw_once
# 里用 amount/100000000 反算就能对上；同一笔账在账本指纹里显示为 spendable=40000。
function drill_balance_min_units() {
    awk -v b="${1}" 'BEGIN{printf "%d", b * 100000000}'
}

# 账本里**真正花得出去**的额度：只有 status=minted 的 seal 能被 select_seals 选中
# （rgb-sidecar/src/ledger.rs:248-270，条件是 `s.status == SealStatus::Minted`）。
#
# 注意这与 drill_ledger_fingerprint 的 `spendable` **不是一回事**：后者只排除 consumed，
# 所以 pending-mint 也算进去，会给出偏大的数。付款能力只能看这个。
function drill_ledger_minted_total() {
    local json
    json=$(rgb20_ledger_json)
    if [ -z "${json}" ]; then
        echo "0"
        return 0
    fi
    echo "${json}" | jq -r '([.seals[]? | select((.status | ascii_downcase) == "minted") | .amount] | add // 0) | tostring'
}

# 每一枚 minted seal 的 outpoint/面额，只用于把"这次充值能用多少"写进日志（排障时一眼能对上）。
function drill_minted_seals_desc() {
    local json
    json=$(rgb20_ledger_json)
    if [ -z "${json}" ]; then
        echo "<ledger unreadable>"
        return 0
    fi
    echo "${json}" |
        jq -r '[.seals[]? | select((.status | ascii_downcase) == "minted") | "\(.outpoint)=\(.amount)"] | join(" ") | if . == "" then "<none>" else . end'
}

# 付款前重新读一次账本，并等它连续若干次读数一致再采用。
#
# 为什么不能沿用更早的读数：侧车重启后桥会立刻重试前面场景遗留的 pending 提现，
# 每次 BuildWithdrawal 都会先 sync() 一遍账本（引擎串行，但读侧车文件的是我们）。
# 在 sync 落地前后取到的额度可能不同，而付款（test-sim 的 select_seals）只认那一刻的状态。
# 读稳定后的值，才能保证"按这个额度付款"与"付款时账本真的能付"是同一份状态。
# 超时仍不一致时返回最后一次读数，由调用方按实际额度决定（0 就走显式 SKIP）。
function drill_minted_total_stable() {
    local prev="" cur="" same=0 i
    for ((i = 0; i < 12; i++)); do
        cur=$(drill_ledger_minted_total)
        if [ "${cur}" = "${prev}" ]; then
            same=$((same + 1))
            [ "${same}" -ge 2 ] && { echo "${cur}"; return 0; }
        else
            same=0
        fi
        prev="${cur}"
        sleep 1
    done
    echo "${cur}"
}

# =====================================================================
# 场景本体
# =====================================================================
function scenario_cold_backup_restore() {
    if [ "${RGBX_BACKUP_DRILL:-1}" != "1" ]; then
        log_step "cold backup/restore drill skipped (RGBX_BACKUP_DRILL=${RGBX_BACKUP_DRILL})"
        return 0
    fi

    log_step "scenario: cold backup -> REAL destroy -> restore -> deposit + withdraw (BACKUP_RECOVERY.md G8)"

    DRILL_SCRIPT_DIR=$(drill_scripts_dir)
    DRILL_OUT_DIR="${ROOT_DIR}/backup-drill"
    DRILL_TAG="${DRILL_TAG:-drill-$(date -u +%Y%m%dT%H%M%SZ)}"
    mkdir -p "${DRILL_OUT_DIR}"

    # 两个脚本共用同一组必需成员参数（约定期约：`--required` 两侧同名同义，见两个脚本的头部用法）。
    local -a required_argv=()
    local line
    while IFS= read -r line; do
        [ -n "${line}" ] || continue
        required_argv+=(--required "${line}")
    done < <(drill_required_args)

    # 停服/销毁之前先验脚本本身（规矩见 drill_preflight_scripts 的注释）
    drill_preflight_scripts

    local asset_id_before balance_before ledger_before peers_before
    asset_id_before="${RGB20_ASSET_ID:-}"
    balance_before=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    ledger_before=$(drill_ledger_fingerprint)
    peers_before=$(collect_peer_names_from_cli "${PARA1_CLI}" | LC_ALL=C sort -u | tr '\n' ' ')
    log_step "before: balance=${balance_before} ledger=[${ledger_before}] contracts.assetId=${asset_id_before}"
    log_step "before: TSS peer names = ${peers_before}"
    # 备份前先在位断言一次钱包身份：否则"本来就没有"会在还原后被误报成"还原失败"。
    drill_assert_wallet_identity "${PARA1_CLI}" "${AUTH_ADDR1}" "para1 before backup"
    drill_assert_wallet_identity "${PARA2_CLI}" "${AUTH_ADDR2}" "para2 before backup"
    drill_assert_wallet_identity "${PARA3_CLI}" "${AUTH_ADDR3}" "para3 before backup"
    drill_assert_wallet_identity "${PARA4_CLI}" "${AUTH_ADDR4}" "para4 before backup"

    # ---- 1) 冷备：停 → 打包（辅助容器里跑通用脚本）----
    # --no-stop/--no-start：停起由 harness 做（见文件头"与冷备脚本的分工"）；
    # --trust-caller-stopped：上面 drill_stop_services 已逐服务 `ps -q` 确认停住了，这里显式认领。
    drill_stop_services
    local -a backup_argv=(
        --out-dir /out --tag "${DRILL_TAG}"
        --sources main=/src/main --sources para1=/src/para1 --sources para2=/src/para2
        --sources para3=/src/para3 --sources para4=/src/para4 --sources sidecar=/src/sidecar
        --no-stop --no-start --trust-caller-stopped
        "${required_argv[@]}"
    )
    local backup_rc=0
    drill_run_script ro cold_backup.sh "${backup_argv[@]}" || backup_rc=$?
    # 打包失败也必须把服务起回来（停机窗口内失败不留停摆环境）
    if [ "${backup_rc}" != "0" ]; then
        drill_start_services
        fail "cold_backup.sh failed (rc=${backup_rc}) — see the [backup] log lines above; the six services were started again"
    fi
    local package="${DRILL_OUT_DIR}/bridge-backup-${DRILL_TAG}.tar"
    if [ ! -f "${package}" ]; then
        drill_start_services
        fail "cold backup reported success but produced no package at ${package}"
    fi
    log_step "cold backup OK: $(wc -c <"${package}" | tr -d ' ') bytes at ${package}"

    # ---- 1.2) 代次元数据：包必须自带"我是哪一代"（manifest 的 tss.*）----
    # 这一条就是 A 的可证伪断言：CI 里 4 份 neutrino.db 都在卷里、DKG/refresh 都跑过，所以
    # 提取必须拿到真实 rid（拿不到说明 manifest 的 tss.* 抠取坏了 —— 那会把"代次"变成一句空话）。
    local pkg_manifest="${DRILL_OUT_DIR}/pkg-manifest.tsv"
    tar -xOf "${package}" "$(basename "${package}" .tar)/manifest.tsv" >"${pkg_manifest}" 2>/dev/null ||
        fail "cannot read manifest.tsv out of the package: ${package}"
    local tss_gen tss_distinct
    tss_gen=$(grep -m1 '^# tss.generation=' "${pkg_manifest}" | sed 's/^# tss.generation=//')
    tss_distinct=$(grep -m1 '^# tss.generation.distinct=' "${pkg_manifest}" | sed 's/^# tss.generation.distinct=//')
    log_step "package TSS generation: dkgRid=${tss_gen} (distinct=${tss_distinct})"
    [ -n "${tss_gen}" ] && [ "${tss_gen}" != "unknown" ] ||
        fail "the package carries no usable TSS generation (tss.generation='${tss_gen}') — the DKG rid extraction in cold_backup.sh/backup_lib.sh is broken (see tss_generation_lines)"
    [ "${tss_distinct}" = "1" ] ||
        fail "the package mixes ${tss_distinct} DKG generations — the four shares in it are not from the same key"
    # 两条 TSS 记录（DKG 的 rid / refresh 记录的 dkgRid）必须指向同一把钥：不一致说明抠到了僵尸页，
    # 那时的"代次"是假的可信值 —— 在 CI 里必须为 no。
    if grep -q 'ridMismatch=yes' "${pkg_manifest}"; then
        fail "TSS generation is NOT trustworthy: the DKG record and the refresh record disagree in at least one share DB (see the tss.* lines of $(basename "${pkg_manifest}"))"
    fi

    # ---- 1.5) **销毁之前**：拿产物把"恢复"这一步只读地跑一遍 ----
    # 只读、完整校验（参数 + 包 + 逐文件 + 必需成员 + 每个 label 有目标），不碰任何目标。
    # 这一条就是"破坏性步骤之前先验证后续步骤可执行"的落地：参数不被接受 / 包不可恢复，
    # 都在**销毁之前**失败，不会白毁一轮。dry-run 与真跑用**同一个 argv**（逐字一致）。
    local -a restore_argv=(
        --archive "/out/$(basename "${package}")"
        --target main=/src/main --target para1=/src/para1 --target para2=/src/para2
        --target para3=/src/para3 --target para4=/src/para4 --target sidecar=/src/sidecar
        --replace clear --no-stop --no-start --trust-caller-stopped
        "${required_argv[@]}"
    )
    local dry_rc=0
    drill_run_script ro restore_backup.sh --dry-run "${restore_argv[@]}" || dry_rc=$?
    if [ "${dry_rc}" != "0" ]; then
        drill_start_services
        fail "restore --dry-run failed (rc=${dry_rc}) BEFORE anything was destroyed: the arguments/package are not restorable — fix and re-run (nothing was wiped)"
    fi
    log_step "preflight: restore --dry-run OK (arguments accepted, package verified restorable) — nothing destroyed yet"

    # ---- 2) 真的销毁 ----
    drill_destroy_state

    # ---- 3) 还原（脚本自己做"包校验 → 落盘 → 逐文件复核"）----
    local restore_rc=0
    drill_run_script rw restore_backup.sh "${restore_argv[@]}" || restore_rc=$?
    if [ "${restore_rc}" != "0" ]; then
        drill_start_services
        fail "restore_backup.sh failed (rc=${restore_rc}) — the data volumes are in the destroyed state; fix the scripts and re-run (the environment needs ./docker-compose.sh reset if you want a clean start)"
    fi
    log_step "restore OK (package verification + post-restore verification both passed)"

    # ---- 4) 起进程 + 等就绪 ----
    drill_start_services
    drill_wait_env_ready

    # ---- 5) 断言"状态是还原回来的"，不是"重建出一个空壳" ----
    local balance_after ledger_after peers_after
    balance_after=$(query_rgb20_balance "${USER_MAIN_ADDR}")
    ledger_after=$(drill_ledger_fingerprint)
    peers_after=$(collect_peer_names_from_cli "${PARA1_CLI}" | LC_ALL=C sort -u | tr '\n' ' ')
    log_step "after:  balance=${balance_after} ledger=[${ledger_after}]"
    log_step "after:  TSS peer names = ${peers_after}"

    assert_eq "${balance_after}" "${balance_before}" \
        "chain33 balance after restore != before backup (B4 main chain data / B2 sidecar ledger did not come back)"
    assert_eq "${ledger_after}" "${ledger_before}" \
        "sidecar ledger fingerprint after restore != before backup (B2: the sidecar state did not come back intact)"
    assert_eq "${peers_after}" "${peers_before}" \
        "TSS peer-name set changed across restore (B5: the libp2p identity key was not restored — the node would come back as a NEW group member)"

    # 侧车"能正常起来"的断言（#60 的原子写/fail-loud 落地后这条才真正有意义）：gRPC 在服务 +
    # 账本可解析 + 资产 id 与配置一致。
    assert_non_empty "$(rgb20_ledger_json | jq -r '.assets // {} | keys | join(",")')" \
        "sidecar ledger has no asset after restore"
    if [ -n "${asset_id_before}" ]; then
        local ledger_asset_id
        ledger_asset_id=$(rgb20_ledger_json | jq -r '.assets // {} | keys[0] // empty')
        assert_eq "${ledger_asset_id}" "${asset_id_before}" \
            "sidecar asset id after restore != contracts.assetId (restored ledger is not the contract the bridge is configured for)"
    fi

    # ---- 6) 证明真的能用：一笔充值 + 一笔提现 ----
    #
    # 充值的付款方是**桥自己**（test-sim 用桥的 seal 模拟用户付款，找零回 TSS 脚本），所以账本里
    # 必须有 status=minted 的 seal 才付得出这笔钱（ledger.rs:248-270 的 select_seals 只认 Minted）。
    #
    # ⚠️ 覆盖边界（别把它读成"充值腿已验证"）：
    # 账本被前面的场景花干后，**这笔模拟充值在 fixture 层面就付不出来**，与还原无关。
    # 原因是 test-sim 的"用户付款"是**用桥自己的 seal 模拟**的（simulate_user_pay 花掉桥的 seal、
    # 找零回 TSS 脚本），所以它必须有 minted 的桥 seal；而主套件的充值+提现循环会把它们耗尽
    # （2026-09-21 实测备份那一刻：5 consumed + 1 pending-mint，零 minted）。
    # **生产里没有这个限制**——真实用户带自己的 USDT 来付款，不花桥的余额。
    # 因此这里按实际额度决定：有 minted 就跑充值腿；没有就显式跳过并把边界打出来，不假装覆盖。
    local minted_after
    # 付款前现读一次（并等读数稳定），不用任何更早的快照：侧车重启后桥会重试遗留的 pending，
    # 每次 BuildWithdrawal 内部的 sync() 都可能改变 seal 生命周期，旧读数的额度可能已经花不出去。
    minted_after=$(drill_minted_total_stable)
    log_step "post-restore deposit quota (live read): minted=${minted_after}, seals=$(drill_minted_seals_desc)"
    local drill_deposit_leg="skipped (restored ledger had 0 minted seals)"
    if [ "${minted_after}" -gt 0 ] 2>/dev/null; then
        local drill_deposit_amount="${RGB20_DEPOSIT_AMOUNT}"
        if [ "${drill_deposit_amount}" -gt "${minted_after}" ]; then
            drill_deposit_amount="${minted_after}"
            log_step "post-restore deposit capped to the restored spendable: ${RGB20_DEPOSIT_AMOUNT} -> ${drill_deposit_amount} (minted seals total)"
        fi
        # requestId 必须与主套件那次区分开：CreateReceive 按 requestId 幂等，复用会拿到旧 receive。
        scenario_rgb20_deposit "rgb20-drill-post-restore" "${drill_deposit_amount}"
        drill_deposit_leg="ran (${drill_deposit_amount} min units)"
    else
        log_step "post-restore deposit leg SKIPPED: restored ledger has 0 minted seals, and the test-sim payer spends the bridge's own seals (fixture limitation, no production analogue — a real user pays from their own wallet). COVERAGE: the deposit path after restore is NOT covered by this drill; the withdraw leg below is."
    fi

    # 提现腿：不依赖桥的 minted 余额（用户在 chain33 侧销毁、桥从 BTC 主池付款），所以它是
    # **无条件**的可用性证明——恢复后的桥真的还能签名、还能对外付款。
    #
    # 但金额必须跟着**还原后的实际余额**走。用户余额是主套件充值/提现循环的残余（实测 0.0004
    # = 40000 最小单位），而默认 RGB20_WITHDRAW_AMOUNT=500000 比它大 12 倍 —— 直接用默认值
    # 必然 ErrInsufficientBalance，看起来像"还原后不能提现"，实则是金额选错了。
    # 取一半，给销毁/手续费留空间。
    local bal_units withdraw_after
    bal_units=$(drill_balance_min_units "${balance_after}")
    if [ "${bal_units}" -lt 2 ] 2>/dev/null; then
        fail "restored balance too small to withdraw (${balance_after} = ${bal_units} min units): a restored-but-empty balance cannot be told apart from a failed restore here — check the before/after balance assertion above first"
    fi
    withdraw_after=$(( bal_units / 2 ))
    if [ "${withdraw_after}" -gt "${RGB20_WITHDRAW_AMOUNT}" ]; then
        withdraw_after="${RGB20_WITHDRAW_AMOUNT}"
    fi
    log_step "post-restore withdraw amount: ${withdraw_after} min units (restored balance ${balance_after} = ${bal_units} min units; cap ${RGB20_WITHDRAW_AMOUNT})"
    rgb20_withdraw_once "${withdraw_after}" "post-restore"

    log_step "cold backup/restore drill PASSED: state restored byte-for-byte; post-restore deposit leg: ${drill_deposit_leg}; post-restore withdraw leg: ran"
    log_step "artifact kept at ${package} (harness output dir; not a deliverable — the durable copy is off-host)"

    # G7 探针：删掉 btcwallet.db（保留其余数据）再起官方节点，证明"新钱包 + 非空链"会明确告警，
    # 而不是静默漏掉历史充值。放在最后：它会让 para1 的钱包变成新建的，破坏后续场景的前提。
    scenario_wallet_birthday_guard
}

# G7 探针（对应 #63 的 B 臂）：只删 btcwallet.db，保留 neutrino 头链与其余状态。
# 新钱包的生日 = 现在，而链已经很高 —— 停桥期间（或钱包被误删期间）的充值不在钱包 tx store 里，
# 桥的重放读的是钱包 store，因此这些充值永远发现不了。代码在首次链通知时打印 ALERT；本探针断言它真的出现。
function scenario_wallet_birthday_guard() {
    log_step "scenario: a fresh btcwallet.db on a non-empty chain must raise the birthday alarm (G7 probe)"
    local vol
    vol=$(drill_volume_name para1)
    compose_cmd stop para1 >/dev/null
    compose_cmd run --rm --no-deps -T --entrypoint sh -v "${vol}:/data" gateway \
        -c 'test -d /data/lightclient && rm -f /data/lightclient/btcwallet.db && echo removed' >/dev/null ||
        fail "failed to remove btcwallet.db from volume ${vol} (the probe cannot run)"

    local started_at i logs
    started_at=$(date +%s)
    compose_cmd start para1 >/dev/null
    for ((i = 0; i < 120; i++)); do
        logs=$(para1_logs_since "${started_at}" 2>/dev/null || true)
        if echo "${logs}" | grep -q "wallet birthday cannot cover chain history"; then
            log_step "PASS: birthday guard fired: $(echo "${logs}" | grep -m1 'wallet birthday cannot cover chain history')"
            return 0
        fi
        sleep 1
    done
    fail "the birthday guard did not fire after removing btcwallet.db: a fresh wallet on a non-empty chain would silently miss deposits (para1 log tail: $(para1_logs_since "${started_at}" | tail -5))"
}

function run_backup_drill_all() {
    scenario_cold_backup_restore
}
