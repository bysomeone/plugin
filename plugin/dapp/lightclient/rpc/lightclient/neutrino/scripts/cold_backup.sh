#!/usr/bin/env bash
# 冷备：**停 → 打包 → 起**（冷备 = 停机窗口内取样，因此不需要任何一致性快照机制；见
# BACKUP_RECOVERY.md 的缺口 G4 —— 在线 tar 可能拿到撕裂的组合，停住就没有这个问题）。
#
# 打包什么、为什么：BACKUP_RECOVERY.md §2 的 B1–B5（TSS share / 侧车 /data / 中继业务 bucket /
# 主链数据 / libp2p 身份私钥）。**btcd 数据不在清单里**：regtest 每次启动无条件清链，生产可从
# 网络重同步，本就不该当可恢复对象（§2.4）。
#
# 用法（生产示例：数据目录在本地、用 systemd 停起）：
#   BACKUP_SOURCES=$'main=/var/lib/chain33/main\npara1=/var/lib/chain33/para1\nsidecar=/var/lib/rgb-sidecar/data' \
#   BACKUP_REQUIRED=$'main:mavltree\npara1:lightclient/neutrino.db\npara1:lightclient/btcwallet.db\npara1:addrbook/dht\nsidecar:ledger.json\nsidecar:stock' \
#   BACKUP_OUT_DIR=/srv/backups \
#   BACKUP_STOP_CMD='systemctl stop chain33@main chain33@para1 rgb-sidecar' \
#   BACKUP_START_CMD='systemctl start chain33@main chain33@para1 rgb-sidecar' \
#   BACKUP_STOPPED_CHECK_CMD='! systemctl is-active --quiet chain33@main chain33@para1 rgb-sidecar' \
#   BACKUP_VERSION_CHAIN33="$(git -C /srv/src/chain33 describe --tags --always)" \
#   BACKUP_VERSION_SIDECAR="$(git -C /srv/src/plugin describe --tags --always)" \
#     cold_backup.sh --tag "$(date -u +%Y%m%dT%H%M%SZ)"
#
# ⚠️ **先确认每个节点的落盘路径都在你的源清单里**。chain33 的 `[wallet] dbPath` 是**独立配置**，
# 默认 `parawallet` / `wallet` —— 它可能不在 datadir 里（CI 拓扑曾经就是 `/root/parawallet`，卷外 ⇒
# 不在备份集内）。这个钱包持有 authAccount 的私钥，而 para 共识的签名钥就来自它
# （chain33 para 的 commitMsgClient.fetchPriKey → `wallet.DumpPrivkey(authAccount)`），TSS 的
# CommitDKG 提交也用同一把钥 ⇒ 丢了该节点再也无法以自己注册在链上的地址签名（同类问题见
# BACKUP_RECOVERY.md 的 B5）。处置二选一：**把 `[wallet] dbPath` 挪进 datadir**（推荐，CI 已这么做），
# 或者把钱包目录**单独列成一个源**：`--sources parawallet=/var/lib/chain33/parawallet`（并加
# `--required parawallet:...`）。查路径：`chain33-cli --conf=<toml> ...` 或直接看 toml 的 `[wallet] dbPath`。
#
# CI（compose 拓扑，数据在命名卷里）用它时，源路径是"挂进辅助容器的卷"、停起钩子是 compose：
# 见 plugin/dapp/rgbx/cmd/ci/scripts/backup_test.sh 的 scenario_cold_backup_restore（drill_run_script）。
#
# 环境变量（全部可空，除非标注必填）：
#   BACKUP_SOURCES        必填。多行 `label=path`；每个 path 是一个**数据目录**（该目录的内容进包）
#   BACKUP_REQUIRED       可选。多行 `label:relpath`；关键成员，缺一个即判"包不完整"（fail-closed）
#   BACKUP_EXTRA_FILES    可选。多行 `label=path`；不在数据目录里的单文件（如 RGB_SIDECAR_CONTRACTS
#                         声明文件，见缺口 G3）。按 `extra/<label>` 存进包，恢复时按同样规则放回
#   BACKUP_OUT_DIR        必填。产物目录（产生 bridge-backup-<tag>/ 与 bridge-backup-<tag>.tar）
#   BACKUP_TAG            可选。默认 UTC 时间戳；产物名 = <label>-backup-<tag>
#   BACKUP_LABEL          可选。产物名前缀，默认 bridge
#   BACKUP_STOP_CMD       停进程的钩子（空 = 调用方已经停好了，本脚本不碰）
#   BACKUP_START_CMD      起进程的钩子（**任何失败路径都会尝试执行**，避免把桥留在停着的状态）
#   BACKUP_STOPPED_CHECK_CMD  **生产必设**：断言"进程真的停了"的钩子（例如
#                         `! systemctl is-active --quiet chain33@main ...`）。不给、又不用
#                         --trust-caller-stopped 显式认领，脚本会**拒绝打包**：自检只能证明包没坏，
#                         证明不了取样的那一刻没有进程在写。CI 演练由 harness 停服并逐服务核对，
#                         所以它显式传 --trust-caller-stopped。
#   BACKUP_VERSION_*      chain33 / sidecar / plugin / plugin_commit —— 记进清单的"来源版本"
# 参数：
#   --tag <tag> | --out-dir <dir> | --sources <label=path>（可重复）| --required <label:relpath>（可重复）
#   --trust-caller-stopped | --no-stop | --no-start | --keep-staging | -h|--help
#
# 退出码：0 = 产出一份**自检通过**的包；非 0 = 没有可用的包（失败前会把服务起回来）。

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./backup_lib.sh
source "${SCRIPT_DIR}/backup_lib.sh"

DO_STOP=1
DO_START=1
KEEP_STAGING=0
EXTRA_COUNT=0

usage() {
    grep '^#' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | sed '/^退出码/,$d'
}

while [ "$#" -gt 0 ]; do
    case "$1" in
    --tag)
        BACKUP_TAG="$2"
        shift 2
        ;;
    --out-dir)
        BACKUP_OUT_DIR="$2"
        shift 2
        ;;
    --sources)
        add_source "$2"
        shift 2
        ;;
    --required)
        add_required "$2"
        shift 2
        ;;
    --no-stop)
        DO_STOP=0
        shift
        ;;
    --no-start)
        DO_START=0
        shift
        ;;
    --keep-staging)
        KEEP_STAGING=1
        shift
        ;;
    --trust-caller-stopped)
        # 显式认领"写者已停"这件事（见下方 STOPPED-CHECK 段落）。只有"停服由调用方做、且调用方
        # 自己已经核过"时才用，例如 CI 演练里 harness 停服 + 逐服务 `ps -q` 确认。
        TRUST_CALLER_STOPPED=1
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

require_cmd tar gzip sha256sum find sort awk sed wc mktemp cp
[ -n "${BACKUP_OUT_DIR:-}" ] || die "BACKUP_OUT_DIR (or --out-dir) is required"

# env 里的源清单与命令行 --sources 合并（env 优先解析，命令行追加；label 重复即失败）。
if [ -n "${BACKUP_SOURCES:-}" ]; then
    feed_list source "${BACKUP_SOURCES}"
fi
[ ${#SOURCE_LABELS[@]} -gt 0 ] || die "BACKUP_SOURCES (or --sources) is required"

: "${BACKUP_TAG:=$(date -u +%Y%m%dT%H%M%SZ)}"
: "${BACKUP_LABEL:=bridge}"

# env 里的 BACKUP_REQUIRED 是基线，命令行 --required 是补充（两者合并，顺序不影响判定）。
if [ -n "${BACKUP_REQUIRED:-}" ]; then
    _env_required="${REQUIRED_ENTRIES[*]-}"
    REQUIRED_ENTRIES=()
    feed_list required "${BACKUP_REQUIRED}"
    if [ -n "${_env_required}" ]; then
        for _r in ${_env_required}; do add_required "${_r}"; done
    fi
fi

PACKAGE_NAME="${BACKUP_LABEL}-backup-${BACKUP_TAG}"
STAGING_DIR="${BACKUP_OUT_DIR}/${PACKAGE_NAME}"
OUTER_TAR="${BACKUP_OUT_DIR}/${PACKAGE_NAME}.tar"

# --- 停机窗口的守护：只关自己开的窗口 —— 一旦**尝试过**停进程，无论怎么退出都要尝试起回来 ---
_started=0
_window_opened=0
_ensure_started() {
    [ "${_window_opened}" = "1" ] || return 0
    [ "${_started}" = "0" ] || return 0
    _started=1
    if [ "${DO_START}" = "1" ] && [ -n "${BACKUP_START_CMD:-}" ]; then
        if ! bash -c "${BACKUP_START_CMD}"; then
            log_err "START HOOK FAILED: the services stopped by this script are still DOWN: ${BACKUP_START_CMD}"
        fi
    fi
}
trap '_ensure_started' EXIT

# =====================================================================
# 0. 预检：源与关键成员先看一遍（不产出半成品包）
# =====================================================================
log_info "preflight: ${#SOURCE_LABELS[@]} source(s), tag=${BACKUP_TAG}"
_i=0
for ((_i = 0; _i < ${#SOURCE_LABELS[@]}; _i++)); do
    _label="${SOURCE_LABELS[_i]}"
    _path="${SOURCE_PATHS[_i]}"
    [ -d "${_path}" ] || die "source ${_label} is not a directory: ${_path}"
    _fcount=$(list_source_files "${_path}" | wc -l | tr -d ' ')
    [ "${_fcount}" -gt 0 ] ||
        die "source ${_label} has no regular file: ${_path} (wrong mount/path?)"
    check_required_paths "${_path}" "${_label}" ||
        die "source ${_label} (${_path}) is missing required members — the package would be incomplete; fix the required list or the mount"
    log_info "preflight: ${_label} <- ${_path} (${_fcount} file(s))"
done
[ -d "${BACKUP_OUT_DIR}" ] || mkdir -p "${BACKUP_OUT_DIR}" || die "cannot create BACKUP_OUT_DIR: ${BACKUP_OUT_DIR}"
[ -w "${BACKUP_OUT_DIR}" ] || die "BACKUP_OUT_DIR not writable: ${BACKUP_OUT_DIR}"
[ ! -e "${STAGING_DIR}" ] || die "staging dir already exists (pick another tag, or remove it): ${STAGING_DIR}"
[ ! -e "${OUTER_TAR}" ] || die "package already exists (pick another tag): ${OUTER_TAR}"

# =====================================================================
# 1. 停：这是"冷"的全部含义 —— 取样点必须没有任何进程在写
#
# STOPPED-CHECK（**生产必设**）：打包后的自检只能证明"包没坏"，**证明不了取样的那一刻内容是静止的**；
# 进程还在写的时候打出来的包会带着一份"自检通过"的假信心。所以：
#   - 给了 BACKUP_STOPPED_CHECK_CMD ⇒ 由它断言（例如 `! systemctl is-active --quiet ...`）；
#   - 没给 ⇒ 必须用 --trust-caller-stopped 显式认领"停服且已核对由调用方负责"，否则**拒绝打包**。
#     （`--no-stop` 本身**不等于**已核对：它只是"别在这停"，CI 演练就是这么用的 —— 它由 harness 停服，
#      并且 harness 逐服务 `ps -q` 确认过，所以它显式传了 --trust-caller-stopped。）
# =====================================================================
if [ -z "${BACKUP_STOPPED_CHECK_CMD:-}" ] && [ "${TRUST_CALLER_STOPPED:-0}" != "1" ]; then
    die "refusing to package without a 'writers are stopped' check: set BACKUP_STOPPED_CHECK_CMD (production MUST set it) or pass --trust-caller-stopped if the caller already verified it"
fi
WINDOW_START=$(date +%s)
if [ "${DO_STOP}" = "1" ]; then
    _window_opened=1 # 先置位再执行：stop 钩子失败/中途退出时也要尝试起回来
    run_hook "stop" "${BACKUP_STOP_CMD:-}"
else
    log_info "stop hook skipped (--no-stop): the caller guarantees nothing is writing"
fi
if [ -n "${BACKUP_STOPPED_CHECK_CMD:-}" ]; then
    run_hook "stopped-check" "${BACKUP_STOPPED_CHECK_CMD}"
fi

# =====================================================================
# 2. 打包：清单（逐文件 size+sha256）→ 每个源一个 tar.gz → 外层一个容器 tar
# =====================================================================
mkdir -p "${STAGING_DIR}" || die "cannot create staging dir: ${STAGING_DIR}"
MANIFEST="${STAGING_DIR}/manifest.tsv"

FILE_COUNT=$(write_manifest "${MANIFEST}" "${BACKUP_TAG}") || die "failed to write the manifest"
log_info "manifest written: ${FILE_COUNT} file(s) sampled from the stopped sources"

# extra files（可选）：不在数据目录里的单文件（缺口 G3 的合约声明文件就是这种）
if [ -n "${BACKUP_EXTRA_FILES:-}" ]; then
    mkdir -p "${STAGING_DIR}/extra"
    while IFS= read -r _line; do
        _line="${_line#"${_line%%[![:space:]]*}"}"
        [ -n "${_line}" ] || continue
        case "${_line}" in \#*) continue ;; esac
        _elabel="${_line%%=*}"
        _epath="${_line#*=}"
        [ "${_elabel}" != "${_epath}" ] || die "BACKUP_EXTRA_FILES entry must be label=path: ${_line}"
        [ -f "${_epath}" ] || die "extra file not found: ${_epath}"
        cp -p "${_epath}" "${STAGING_DIR}/extra/${_elabel}" || die "cannot copy extra file: ${_epath}"
        _esize=$(wc -c <"${STAGING_DIR}/extra/${_elabel}" | tr -d ' ')
        _esha=$(sha256sum "${STAGING_DIR}/extra/${_elabel}" | awk '{print $1}')
        printf 'file\t%s\t%s\t%s\t%s\n' "${BACKUP_LABEL}" "extra/${_elabel}" "${_esize}" "${_esha}" \
            >>"${MANIFEST}"
        EXTRA_COUNT=$((EXTRA_COUNT + 1))
        log_info "extra file packed: ${_elabel} <- ${_epath}"
    done <<<"${BACKUP_EXTRA_FILES}"
    FILE_COUNT=$((FILE_COUNT + EXTRA_COUNT))
fi

for ((_i = 0; _i < ${#SOURCE_LABELS[@]}; _i++)); do
    _label="${SOURCE_LABELS[_i]}"
    _path="${SOURCE_PATHS[_i]}"
    # `-C <dir> .`：包内成员路径 = 源根下的相对路径（与清单的 relpath 口径一致）
    tar -C "${_path}" -czf "${STAGING_DIR}/${_label}.tar.gz" . ||
        die "tar failed for source ${_label} (${_path})"
    log_info "packed ${_label}.tar.gz ($(wc -c <"${STAGING_DIR}/${_label}.tar.gz" | tr -d ' ') bytes)"
done

{
    printf 'package:  %s\n' "${PACKAGE_NAME}"
    printf 'created:  %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'manifest: manifest.tsv (%s file entries)\n' "$(manifest_file_count "${MANIFEST}")"
    printf 'members:  %s\n' "$(cd "${STAGING_DIR}" && ls -1 ./*.tar.gz 2>/dev/null | xargs -n1 basename | tr '\n' ' ')"
    printf 'versions: chain33=%s sidecar=%s plugin=%s\n' \
        "$(manifest_header "${MANIFEST}" version.chain33)" \
        "$(manifest_header "${MANIFEST}" version.sidecar)" \
        "$(manifest_header "${MANIFEST}" version.plugin)"
    printf 'TSS 代次: dkgRid=%s (distinct=%s)  ← 恢复时先核一眼"这份包是哪一代"（详见 manifest 的 tss.* 行）\n' \
        "$(manifest_header "${MANIFEST}" tss.generation)" \
        "$(manifest_header "${MANIFEST}" tss.generation.distinct)"
    printf '\n%s\n' "恢复（restore_backup.sh）之后**建议再跑一轮 refresh**，理由与做法："
    printf '%s\n' "  - 旧 share 在服役期间可能已经暴露过；恢复旧代等于把它重新激活。refresh 是唯一"
    printf '%s\n' "    「换材料、不换群公钥」的机制（CONFIG.md §4.3.2），跑一轮就把可能暴露过的旧材料作废。"
    printf '%s\n' "  - 做法（运维动作，脚本不自动做）：删掉**所有**签名节点的 cggmp-refresh-result 后一起重启，"
    printf '%s\n' "    让它们补跑一轮 refresh（单节点删没用：refresh 是多方协议）。"
    printf '%s\n' "  - 闭环：refresh 换的是材料而不是 rid（rid 标识这把钥），所以刷完之后**这份包就过代了** ——"
    printf '%s\n' "    此时应当重新冷备一次；手里包里的 tss.* 与你链上/节点的现状不符时就说明该重备了。"
    printf 'restore:  restore_backup.sh --archive <本文件>（见 BACKUP_RECOVERY.md §3）\n'
} >"${STAGING_DIR}/README.txt"

# 外层容器 tar（**不压缩**：内层已经是 gz，外层再压等于白烧 CPU）
tar -C "${BACKUP_OUT_DIR}" -cf "${OUTER_TAR}" "${PACKAGE_NAME}" || die "cannot create package: ${OUTER_TAR}"
log_info "package created: ${OUTER_TAR} ($(wc -c <"${OUTER_TAR}" | tr -d ' ') bytes)"

# =====================================================================
# 3. 自检：把产物**读回来**逐文件核对（读不出来 / 对不上 ⇒ 判失败，不留一份假成功的包）
# =====================================================================
CHECK_DIR=$(mktemp -d) || die "mktemp failed"
trap '_ensure_started; rm -rf "${CHECK_DIR}"' EXIT
tar -C "${CHECK_DIR}" -xf "${OUTER_TAR}" || die "self-check: package is not readable: ${OUTER_TAR}"
CHECK_ROOT="${CHECK_DIR}/${PACKAGE_NAME}"
[ -f "${CHECK_ROOT}/manifest.tsv" ] || die "self-check: manifest.tsv missing inside the package"
[ -f "${CHECK_ROOT}/README.txt" ] || die "self-check: README.txt missing inside the package"
# 包里的 manifest 必须与写出去的那份**逐字节**相同（比对 sha256：ubuntu 基础镜像一定有 coreutils，
# 但不一定有 diffutils，所以不用 cmp/diff）。
[ "$(sha256sum <"${MANIFEST}" | awk '{print $1}')" = "$(sha256sum <"${CHECK_ROOT}/manifest.tsv" | awk '{print $1}')" ] ||
    die "self-check: manifest.tsv inside the package differs from the one written (truncated archive?)"

for ((_i = 0; _i < ${#SOURCE_LABELS[@]}; _i++)); do
    _label="${SOURCE_LABELS[_i]}"
    _dest="${CHECK_DIR}/tree-${_label}"
    mkdir -p "${_dest}"
    tar -C "${_dest}" -xzf "${CHECK_ROOT}/${_label}.tar.gz" ||
        die "self-check: cannot extract ${_label}.tar.gz from the package"
    verify_tree_against_manifest "${CHECK_ROOT}/manifest.tsv" "${_label}" "${_dest}" ||
        die "self-check FAILED for ${_label}: the package does not reproduce the source byte-for-byte"
    check_required_paths "${_dest}" "${_label}" ||
        die "self-check FAILED for ${_label}: required member missing inside the package"
done
if [ "${EXTRA_COUNT}" -gt 0 ]; then
    for _f in "${CHECK_ROOT}"/extra/*; do
        [ -f "${_f}" ] || continue
        _rel="extra/$(basename "${_f}")"
        _want=$(awk -F'\t' -v r="${_rel}" '$1=="file" && $3==r {print $5}' "${CHECK_ROOT}/manifest.tsv" | sed -n 1p)
        _got=$(sha256sum "${_f}" | awk '{print $1}')
        [ "${_want}" = "${_got}" ] || die "self-check FAILED for extra file ${_rel}"
    done
fi
log_info "self-check OK: the package re-reads and every member matches size+sha256"

rm -rf "${CHECK_DIR}"
if [ "${KEEP_STAGING}" = "1" ]; then
    log_info "staging dir kept: ${STAGING_DIR}"
else
    rm -rf "${STAGING_DIR}"
fi

# =====================================================================
# 4. 起：把停机窗口关掉
# =====================================================================
_ensure_started
WINDOW_END=$(date +%s)
trap - EXIT

log_info "DONE  package=${OUTER_TAR}"
log_info "      files=${FILE_COUNT}  stop-window=$((WINDOW_END - WINDOW_START))s"
log_info "      恢复演练见 plugin/dapp/rgbx/cmd/ci/scripts/backup_test.sh"
