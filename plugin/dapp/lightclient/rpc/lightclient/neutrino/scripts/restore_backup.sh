#!/usr/bin/env bash
# 恢复：把 cold_backup.sh 产出的包还原到指定数据目录，**还原后逐文件复核**（size+sha256 与清单一致）。
#
# 与冷备对称：**停 → 校验包 → 落盘 → 复核 → 起**。顺序上有一条硬约定：
# **先校验包、再动目标目录** —— 一个坏包绝不能先把现场毁掉。
#
# 用法（生产示例）：
#   RESTORE_ARCHIVE=/srv/backups/bridge-backup-20260921T120000Z.tar \
#   RESTORE_TARGETS=$'main=/var/lib/chain33/main\npara1=/var/lib/chain33/para1\nsidecar=/var/lib/rgb-sidecar/data' \
#   RESTORE_STOP_CMD='systemctl stop chain33@main chain33@para1 rgb-sidecar' \
#   RESTORE_START_CMD='systemctl start chain33@main chain33@para1 rgb-sidecar' \
#   RESTORE_STOPPED_CHECK_CMD='! systemctl is-active --quiet chain33@main chain33@para1 rgb-sidecar' \
#     restore_backup.sh
#
# 建议先只读地核一遍再动现场（产物可恢复性 + 参数是否被接受）：
#   restore_backup.sh --dry-run --archive <包> --target <同样的 label=path> ...
#
# 环境变量：
#   RESTORE_ARCHIVE       必填。cold_backup.sh 的产物（.tar），或它解开的那个目录（staging 形式）
#   RESTORE_TARGETS       必填。多行 `label=path`；label 必须与包里的源 label 一一对应
#   RESTORE_REPLACE       `refuse`（默认）| `move-aside` | `clear`：目标目录已存在且**非空**时怎么办
#                           refuse      = 直接失败（最安全，要求运维先自己清）
#                           move-aside  = 把现有目录整体改名成 <path>.pre-restore-<tag>（可回滚）
#                           clear       = 清空现有内容（CI 演练用：它刚刚"故意销毁"过，就是要覆盖）
#   RESTORE_EXTRA_TARGETS 可选。多行 `label=path`：`extra/<label>` 单文件的落点（如合约声明文件）
#   RESTORE_REQUIRED      可选。多行 `label:relpath`；必需成员。与冷备侧同名同义，恢复侧**同样**用它：
#                         ① 包内先核（不完整的包在动目标目录之前就被拒）；② 落盘后复核（证明真的就位）。
#                         清单本身也随包走，这里是**追加**要求（并集、去重）。
#   RESTORE_STOP_CMD / RESTORE_START_CMD  与冷备同名同义
#   RESTORE_STOPPED_CHECK_CMD  **生产必设**：断言写者已停的钩子。不给、又不用 --trust-caller-stopped
#                         显式认领，脚本会拒绝落盘（"复核只证明文件对得上，证明不了没有进程在写"）
#   RESTORE_SKIP_POST_VERIFY  可选。=1 时跳过"落盘后复核"（**不推荐**，只在排查脚本本身时用）
# 参数：--archive <path> | --target <label=path>（可重复）| --required <label:relpath>（可重复）
#       | --replace <policy> | --dry-run | --trust-caller-stopped | --no-stop | --no-start | -h
#
# `--dry-run` 是**只读的完整校验**：解析参数 → 读包 → 逐文件核对 → 必需成员在位 → 每个 label 都有目标，
# 然后退出、不碰任何目标。用途：破坏性步骤（清卷/清目录）之前先证明"后续能执行"，别毁完才发现参数不对
# ——CI 演练就是这么用的（backup_test.sh 的 drill_preflight_restore）。
#
# 注意：解开时按 tar 默认保留 uid/gid/mode（chain33 与侧车在生产都以该数据目录的属主身份跑；
# 跨机器恢复且属主不同时，自行 chown 后再起进程）。
#
# 恢复后**推荐的收尾步骤：跑一轮 refresh**（运维动作，本脚本不自动做 —— 它牵扯密钥生命周期）
#
# 为什么：旧 share 在服役期间可能已经暴露过（备份本身就是它离开原机的证据之一）；恢复旧代等于把
# 这份可能暴露过的材料**重新激活**。refresh 是 CGGMP 里唯一的「换材料、不换群公钥」机制
# （CONFIG.md §4.3.2：链上 CrossChainInfo 每个 symbol 只写一份、不可改，所以 re-DKG 修不了任何东西），
# 跑一轮就把可能暴露过的旧份额/旧 Paillier 材料作废，而地址与链上登记都不变。
#
# 怎么做：删掉**所有**签名节点（本拓扑 para1-4）的 `cggmp-refresh-result` 记录后**一起重启**，
# 它们会补跑一轮 refresh（单节点删没用：refresh 是多方协议；缺记录的节点必须与其余节点落在同一轮，
# 会话名由 DKG rid 派生所以天然对齐）。做完看启动日志 `ensureRefresh refresh round done`，并按
# BACKUP_RECOVERY.md §3.4 的自检清单核一遍（share↔链同代 / 侧车账 ↔ 链上供应量）。
#
# **闭环**：refresh 换的是材料、**不是 rid**（rid 标识的是这把钥 / 这轮 DKG），所以刷完之后
# **这份包就"过代"了** —— 此时应当重新冷备一次。判据就在包里/日志里：manifest 的 tss.* 与
# 节点现状不符（或你刚跑过一轮 refresh），就该重备。
#
# 退出码：0 = 目标目录已回到包里的状态且复核通过；非 0 = 未完成（已尽力把服务起回来）。

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./backup_lib.sh
source "${SCRIPT_DIR}/backup_lib.sh"

DO_STOP=1
DO_START=1
DRY_RUN=0
declare -a TARGET_LABELS=()
declare -a TARGET_PATHS=()
declare -a EXTRA_TARGET_LABELS=()
declare -a EXTRA_TARGET_PATHS=()

usage() {
    grep '^#' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | sed '/^退出码/,$d'
}

add_target() {
    local entry="$1"
    local label="${entry%%=*}"
    local path="${entry#*=}"
    [ "${entry}" != "${path}" ] || die "--target/RESTORE_TARGETS entry must be label=path, got: ${entry}"
    [ -n "${label}" ] && [ -n "${path}" ] || die "empty label/path in: ${entry}"
    local i
    for ((i = 0; i < ${#TARGET_LABELS[@]}; i++)); do
        [ "${TARGET_LABELS[i]}" != "${label}" ] || die "duplicate target label: ${label}"
    done
    TARGET_LABELS+=("${label}")
    TARGET_PATHS+=("${path}")
}

target_path_for() {
    local label="$1"
    local i
    for ((i = 0; i < ${#TARGET_LABELS[@]}; i++)); do
        if [ "${TARGET_LABELS[i]}" = "${label}" ]; then
            printf '%s' "${TARGET_PATHS[i]}"
            return 0
        fi
    done
    return 1
}

while [ "$#" -gt 0 ]; do
    case "$1" in
    --archive)
        RESTORE_ARCHIVE="$2"
        shift 2
        ;;
    --target)
        add_target "$2"
        shift 2
        ;;
    --required)
        # 与冷备侧同名同义：必需成员。恢复侧**同样**用它，而且用在两个地方：
        #   ① 包内先核（坏包/不完整的包在动目标目录之前就被拒）；
        #   ② 落盘后复核（证明必需成员真的就位，而不只是"解开了"）。
        # 清单本身随包走（manifest 的 `# required=`），命令行/env 传的是**追加**要求（并集、去重）。
        add_required "$2"
        shift 2
        ;;
    --replace)
        RESTORE_REPLACE="$2"
        shift 2
        ;;
    --trust-caller-stopped)
        # 显式放弃"确认写者已停"的验证（见下方 STOPPED-CHECK 段落）。
        TRUST_CALLER_STOPPED=1
        shift
        ;;
    --dry-run)
        DRY_RUN=1
        shift
        ;;
    --no-stop)
        DO_STOP=0
        shift
        ;;
    --no-start)
        DO_START=0
        shift
        ;;
    -h | --help)
        usage
        exit 0
        ;;
    *)
        die "unknown argument: $1 (try --help)"
        ;;
    esac
done

require_cmd tar gzip sha256sum find sort awk sed wc mktemp cp mv

if [ -n "${RESTORE_TARGETS:-}" ]; then
    while IFS= read -r _line; do
        _line="${_line#"${_line%%[![:space:]]*}"}"
        [ -n "${_line}" ] || continue
        case "${_line}" in \#*) continue ;; esac
        add_target "${_line}"
    done <<<"${RESTORE_TARGETS}"
fi
if [ -n "${RESTORE_EXTRA_TARGETS:-}" ]; then
    while IFS= read -r _line; do
        _line="${_line#"${_line%%[![:space:]]*}"}"
        [ -n "${_line}" ] || continue
        case "${_line}" in \#*) continue ;; esac
        _l="${_line%%=*}"
        _p="${_line#*=}"
        [ "${_l}" != "${_p}" ] || die "RESTORE_EXTRA_TARGETS entry must be label=path: ${_line}"
        EXTRA_TARGET_LABELS+=("${_l}")
        EXTRA_TARGET_PATHS+=("${_p}")
    done <<<"${RESTORE_EXTRA_TARGETS}"
fi

: "${RESTORE_REPLACE:=refuse}"
case "${RESTORE_REPLACE}" in
refuse | move-aside | clear) ;;
*) die "RESTORE_REPLACE must be refuse|move-aside|clear, got: ${RESTORE_REPLACE}" ;;
esac
: "${RESTORE_ARCHIVE:?RESTORE_ARCHIVE (or --archive) is required}"
[ ${#TARGET_LABELS[@]} -gt 0 ] || die "RESTORE_TARGETS (or --target) is required"

# --- 停机窗口守护（与冷备同一套约定：只关自己开的窗口） ---
_started=0
_window_opened=0
_ensure_started() {
    [ "${_window_opened}" = "1" ] || return 0
    [ "${_started}" = "0" ] || return 0
    _started=1
    if [ "${DO_START}" = "1" ] && [ -n "${RESTORE_START_CMD:-}" ]; then
        if ! bash -c "${RESTORE_START_CMD}"; then
            log_err "START HOOK FAILED: the services stopped by this script are still DOWN: ${RESTORE_START_CMD}"
        fi
    fi
}
trap '_ensure_started' EXIT

# =====================================================================
# 0. 解开包到临时目录（**只读**，不碰任何目标）
# =====================================================================
WORK=$(mktemp -d) || die "mktemp failed"
trap '_ensure_started; rm -rf "${WORK}"' EXIT

if [ -d "${RESTORE_ARCHIVE}" ]; then
    # staging 形式（cold_backup.sh --keep-staging 产出的目录）：直接读
    PKG_DIR="${RESTORE_ARCHIVE}"
    log_info "source: staging dir ${PKG_DIR}"
else
    [ -f "${RESTORE_ARCHIVE}" ] || die "archive not found: ${RESTORE_ARCHIVE}"
    tar -C "${WORK}" -xf "${RESTORE_ARCHIVE}" || die "cannot read the package: ${RESTORE_ARCHIVE}"
    # 包里恰好一个顶层目录（cold_backup.sh 就是这么造的）
    PKG_DIR=$(find "${WORK}" -mindepth 1 -maxdepth 1 -type d | LC_ALL=C sort | sed -n 1p)
    [ -n "${PKG_DIR}" ] || die "package has no top-level directory: ${RESTORE_ARCHIVE}"
    log_info "source: ${RESTORE_ARCHIVE}"
fi
MANIFEST="${PKG_DIR}/manifest.tsv"
[ -f "${MANIFEST}" ] || die "manifest.tsv not found in the package: ${PKG_DIR}"
log_info "package tag=$(manifest_header "${MANIFEST}" tag) created=$(manifest_header "${MANIFEST}" created) files=$(manifest_file_count "${MANIFEST}")"
log_info "versions: chain33=$(manifest_header "${MANIFEST}" version.chain33) sidecar=$(manifest_header "${MANIFEST}" version.sidecar) plugin=$(manifest_header "${MANIFEST}" version.plugin)"
# 代次（包自带"我是哪一代"，不必靠人对时间/文件名判断）。每个源的 tss.<label>.* 行给出该节点那份
# share 的 dkgRid / refresh 记录状态；tss.generation.distinct != 1 说明包里几份 share 不是同一代。
_tss_gen=$(manifest_header "${MANIFEST}" tss.generation)
_tss_distinct=$(manifest_header "${MANIFEST}" tss.generation.distinct)
log_info "TSS generation: dkgRid=${_tss_gen:-unknown} (distinct=${_tss_distinct:-unknown})"
if [ -n "${_tss_distinct}" ] && [ "${_tss_distinct}" != "1" ]; then
    log_warn "this package mixes ${_tss_distinct} different DKG generations: restoring it would leave some nodes unable to sign — do NOT use it as-is"
fi
if [ "${_tss_gen}" = "unknown" ] || [ -z "${_tss_gen}" ]; then
    log_warn "the package does not carry a usable TSS generation (tss.generation=unknown): you cannot tell which generation this package holds — compare against the chain's CrossChainInfo / refresh state by hand before relying on it"
fi

# =====================================================================
# 1. 校验包：每个 label 都必须有 tar.gz、能解开、且逐文件对得上清单
#    —— 这一步在任何"动目标目录"的动作之前完成（坏包不得先毁现场）
# =====================================================================
declare -a PKG_LABELS=()
while IFS= read -r _l; do
    [ -n "${_l}" ] || continue
    PKG_LABELS+=("${_l}")
done < <(manifest_source_labels "${MANIFEST}")

[ ${#PKG_LABELS[@]} -gt 0 ] || die "the manifest lists no file at all (empty/corrupt package?)"

for _l in "${PKG_LABELS[@]}"; do
    if ! target_path_for "${_l}" >/dev/null; then
        die "package contains label '${_l}' but no target was given for it (add --target ${_l}=<dir>)"
    fi
    [ -f "${PKG_DIR}/${_l}.tar.gz" ] || die "package is missing ${_l}.tar.gz"
    _vdest="${WORK}/verify-${_l}"
    mkdir -p "${_vdest}"
    tar -C "${_vdest}" -xzf "${PKG_DIR}/${_l}.tar.gz" || die "cannot extract ${_l}.tar.gz (corrupt package?)"
    verify_tree_against_manifest "${MANIFEST}" "${_l}" "${_vdest}" ||
        die "package verification FAILED for ${_l}: refusing to touch the target directory"
done

# 关键成员 = **包里的清单（随包走，恢复方不需要记得当初传了什么）** ∪ **本次命令行/env 追加的要求**。
# 用并集而不是二选一：包里的那份是"这份包当初承诺过的"，调用方补的是"这次恢复额外要求的"，
# 两者都必须成立。两边都参与下面两处核对：包内先核（拦坏包/不完整的包）+ 落盘后复核（证明到位）。
_env_required="${REQUIRED_ENTRIES[*]-}"
REQUIRED_ENTRIES=()
while IFS= read -r _r; do
    [ -n "${_r}" ] || continue
    add_required "${_r}"
done < <(grep '^# required=' "${MANIFEST}" | sed 's/^# required=//')
if [ -n "${_env_required}" ]; then
    for _r in ${_env_required}; do add_required "${_r}"; done
fi
log_info "required members: $((${#REQUIRED_ENTRIES[@]})) entr(ies) (manifest ∪ caller)"
for _l in "${PKG_LABELS[@]}"; do
    check_required_paths "${WORK}/verify-${_l}" "${_l}" ||
        die "package verification FAILED for ${_l}: a required member is missing (incomplete package)"
done
log_info "package verified: ${#PKG_LABELS[@]} label(s), all members match size+sha256"

if [ "${DRY_RUN}" = "1" ]; then
    log_info "--dry-run: package is good; not touching any target"
    for _l in "${PKG_LABELS[@]}"; do
        log_info "  would restore ${_l} -> $(target_path_for "${_l}")"
    done
    exit 0
fi

# =====================================================================
# 2. 停：落盘之前不能有进程在写目标目录
#
# STOPPED-CHECK（与冷备同一套约定，**生产必设**）：只给 --no-stop 不等于"写者真的停了"，
# 而"恢复完的复核只证明文件对得上、证明不了没有进程在同时写"。所以要么给 RESTORE_STOPPED_CHECK_CMD
# 让它自己核一遍，要么用 --trust-caller-stopped 显式认领这件事（CI 演练由 harness 停服并用
# `ps -q` 逐服务确认，走的就是后者）。
# =====================================================================
if [ ${#TARGET_LABELS[@]} -gt 0 ] && [ -z "${RESTORE_STOPPED_CHECK_CMD:-}" ] && [ "${TRUST_CALLER_STOPPED:-0}" != "1" ]; then
    die "refusing to write into the targets without a 'writers are stopped' check: set RESTORE_STOPPED_CHECK_CMD (production MUST set it) or pass --trust-caller-stopped if the caller already verified it"
fi
if [ "${DO_STOP}" = "1" ]; then
    _window_opened=1 # 先置位再执行：stop 钩子失败/中途退出时也要尝试起回来
    run_hook "stop" "${RESTORE_STOP_CMD:-}"
else
    log_info "stop hook skipped (--no-stop): the caller guarantees nothing is writing the targets"
fi
if [ -n "${RESTORE_STOPPED_CHECK_CMD:-}" ]; then
    run_hook "stopped-check" "${RESTORE_STOPPED_CHECK_CMD}"
fi

# =====================================================================
# 3. 落盘：按 RESTORE_REPLACE 策略处理已有内容，再把包解开到目标
# =====================================================================
_backup_tag=$(manifest_header "${MANIFEST}" tag)
for _l in "${PKG_LABELS[@]}"; do
    _target=$(target_path_for "${_l}")
    _exists_nonempty=0
    if [ -d "${_target}" ] && [ "$(count_source_files "${_target}")" -gt 0 ]; then
        _exists_nonempty=1
    fi
    if [ "${_exists_nonempty}" = "1" ]; then
        case "${RESTORE_REPLACE}" in
        refuse)
            die "target ${_l} (${_target}) exists and is NOT empty; refusing to overwrite (use --replace move-aside to keep the current data, or clear to wipe it)"
            ;;
        move-aside)
            _aside="${_target}.pre-restore-${_backup_tag}"
            [ ! -e "${_aside}" ] || _aside="${_aside}.$(date -u +%H%M%S)"
            log_warn "moving the current ${_l} aside: ${_target} -> ${_aside} (roll back by moving it back)"
            mv "${_target}" "${_aside}" || die "cannot move aside ${_target}"
            ;;
        clear)
            log_warn "clearing ${_target} (RESTORE_REPLACE=clear)"
            find "${_target}" -mindepth 1 -delete || die "cannot clear ${_target}"
            ;;
        esac
    fi
    mkdir -p "${_target}" || die "cannot create target dir: ${_target}"
    tar -C "${_target}" -xzf "${PKG_DIR}/${_l}.tar.gz" || die "extract failed into ${_target}"
    _lcount=$(awk -F'\t' -v l="${_l}" '$1=="file" && $2==l {n++} END {print n+0}' "${MANIFEST}")
    log_info "restored ${_l} -> ${_target} (${_lcount} file entries in the package)"
done

# 包里带的 extra 单文件（`extra/<label>`）：先核校验和，再按 RESTORE_EXTRA_TARGETS 放回。
for ((_i = 0; _i < ${#EXTRA_TARGET_LABELS[@]}; _i++)); do
    _src="${PKG_DIR}/extra/${EXTRA_TARGET_LABELS[_i]}"
    _dst="${EXTRA_TARGET_PATHS[_i]}"
    [ -f "${_src}" ] || die "package has no extra file named '${EXTRA_TARGET_LABELS[_i]}'"
    _want=$(awk -F'\t' -v r="extra/${EXTRA_TARGET_LABELS[_i]}" '$1=="file" && $3==r {print $5}' "${MANIFEST}" | sed -n 1p)
    [ -n "${_want}" ] || die "extra file '${EXTRA_TARGET_LABELS[_i]}' is not in the manifest"
    _got=$(sha256sum "${_src}" | awk '{print $1}')
    [ "${_want}" = "${_got}" ] || die "extra file '${EXTRA_TARGET_LABELS[_i]}' does not match the manifest (corrupt package?)"
    mkdir -p "$(dirname "${_dst}")"
    cp -p "${_src}" "${_dst}" || die "cannot place extra file: ${_dst}"
    _got2=$(sha256sum "${_dst}" | awk '{print $1}')
    [ "${_want}" = "${_got2}" ] || die "extra file placed at ${_dst} does not match the manifest"
    log_info "restored extra file ${EXTRA_TARGET_LABELS[_i]} -> ${_dst}"
done

# 包里带了 extra 文件、调用方却没给落点：说清楚漏了什么（否则会"看起来恢复成功"）
_missing_extra=$(manifest_extra_entries "${MANIFEST}" | awk -F'\t' '{print $3}' | wc -l | tr -d ' ')
if [ "${_missing_extra}" -gt 0 ] && [ ${#EXTRA_TARGET_LABELS[@]} -eq 0 ]; then
    log_warn "the package carries ${_missing_extra} extra file(s) but no RESTORE_EXTRA_TARGETS was given; they were NOT restored:"
    manifest_extra_entries "${MANIFEST}" | awk -F'\t' '{printf "  %s (%s bytes, sha256=%s)\n", $3, $4, $5}' >&2
fi

# =====================================================================
# 4. 落盘后复核：目标目录现在必须与清单逐文件一致（这是"恢复成功"的判据，不是"命令没报错"）
# =====================================================================
if [ "${RESTORE_SKIP_POST_VERIFY:-0}" = "1" ]; then
    log_warn "post-restore verification SKIPPED (RESTORE_SKIP_POST_VERIFY=1)"
else
    for _l in "${PKG_LABELS[@]}"; do
        _target=$(target_path_for "${_l}")
        verify_tree_against_manifest "${MANIFEST}" "${_l}" "${_target}" ||
            die "post-restore verification FAILED for ${_l} (${_target}): the restored tree does not match the manifest"
        check_required_paths "${_target}" "${_l}" ||
            die "post-restore verification FAILED for ${_l}: a required member is not in place"
    done
    log_info "post-restore verification OK: every restored file matches the manifest"
fi

_ensure_started
trap - EXIT
log_info "DONE  tag=${_backup_tag}  tssGeneration=${_tss_gen:-unknown}"
log_info "      起进程后的自检清单见 BACKUP_RECOVERY.md §3.4（share↔链同代 / 侧车账↔链上供应量）"
log_info "      **建议再跑一轮 refresh**（做法与本脚本头部说明）：旧 share 可能已暴露，refresh 把它作废；"
log_info "      刷完材料变了而 rid 不变 ⇒ 这份包即告过代，应重新冷备一次。"
