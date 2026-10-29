#!/usr/bin/env bash
# 备份新鲜度检查：判断 BACKUP_OUT_DIR 里「最近一次**成功**的冷备」是否在阈值内，并可选写出
# node_exporter textfile collector 指标（由 cron 定时跑，供 Prometheus 告警）。
#
# 成功判据（按 cold_backup.sh 的实际产物，不靠猜文件名）：
#   产物是 <label>-backup-<tag>.tar（cold_backup.sh 的 PACKAGE_NAME/OUTER_TAR；label 默认 bridge）。
#   打包期间同一目录下还有同名 staging 目录 <label>-backup-<tag>/；**只有自检通过后它才会被删掉**
#   （见 cold_backup.sh 第 3 节自检与第 4 节之前）。因此：
#     - 只有 .tar、没有同名目录   ⇒ 这次冷备走完自检并成功；
#     - .tar 与同名目录并存       ⇒ 正在打包/自检，或自检失败后的残留（半成品）⇒ 跳过；
#     - 只有目录、没有 .tar       ⇒ 还没进到外层 tar 阶段 ⇒ 不参与判断；
#     - .tar 大小为 0             ⇒ 跳过。
#   多个候选按 mtime 从新到旧看，取第一个满足条件的（被跳过的最新半成品不会遮住上一份好包）。
#   ⚠️ 已知例外：cold_backup.sh --keep-staging 是排查选项，成功时也会留下同名目录，本脚本会把它
#   当成未完成而跳过。**生产保留的备份不要用 --keep-staging。**
#
# 时间口径：取 .tar 的 mtime（外层包写完的时刻；其后的自检可能再花数十秒到数分钟，GB 级数据更久，
# 所以这个时间略早于"整次冷备真正结束"，对 36h 的阈值判断无影响）。年龄 = now - mtime。
# 边界：本脚本只判断"RPO 内有没有一次成功完成的冷备"，**不校验包内容**；包能否读、清单是否对得上
# 仍由 `restore_backup.sh --dry-run` 证明。
#
# 用法：
#   BACKUP_OUT_DIR=/srv/backups check_backup_freshness.sh
#   BACKUP_OUT_DIR=/srv/backups BACKUP_MAX_AGE_HOURS=24 \
#     check_backup_freshness.sh --textfile /var/lib/node_exporter/textfile/rgbx_backup.prom
#
# 环境变量：
#   BACKUP_OUT_DIR        必填。cold_backup.sh 的 BACKUP_OUT_DIR（产物目录）
#   BACKUP_MAX_AGE_HOURS  可选。新鲜度阈值（小时），默认 36；必须是正整数
# 参数：
#   --textfile <path>     可选。把下面的指标写入该文件（先写临时文件再 mv，避免采集器读到半截）
#   -h | --help
#
# 输出（stdout，恰好一行）：
#   PASS|FAIL 最新成功备份的包名、完成时间（UTC）、年龄、阈值；无备份时写明 none。
# 指标（--textfile；失败路径也照样写 —— 年龄大/为负数本身就是告警信号）：
#   rgbx_backup_last_success_timestamp_seconds  <unix 秒；找不到成功备份写 0>
#   rgbx_backup_age_seconds                     <秒；找不到成功备份写 -1>
#
# 退出码：0 = 新鲜；1 = 过期或没有任何成功备份；2 = 参数/环境错误（含 textfile 写失败）。

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./backup_lib.sh
source "${SCRIPT_DIR}/backup_lib.sh"

TEXTFILE=""
TEXTFILE_SET=0
TMP_LIST=""
TMP_METRIC=""

usage() {
    sed -n '2,$p' "${BASH_SOURCE[0]}" | sed '/^$/q' | sed 's/^# \{0,1\}//'
}

cleanup() {
    [ -z "${TMP_LIST}" ] || rm -f "${TMP_LIST}"
    [ -z "${TMP_METRIC}" ] || rm -f "${TMP_METRIC}"
}
trap cleanup EXIT

usage_error() {
    log_err "$*"
    exit 2
}

while [ "$#" -gt 0 ]; do
    case "$1" in
    --textfile)
        [ "$#" -ge 2 ] || usage_error "--textfile requires a path"
        TEXTFILE="$2"
        TEXTFILE_SET=1
        shift 2
        ;;
    -h | --help)
        usage
        exit 0
        ;;
    *)
        usage_error "unknown argument: $1 (try --help)"
        ;;
    esac
done

require_cmd find sort awk sed wc date stat basename dirname mktemp mv rm

[ -n "${BACKUP_OUT_DIR:-}" ] || usage_error "BACKUP_OUT_DIR is required"
: "${BACKUP_MAX_AGE_HOURS:=36}"
case "${BACKUP_MAX_AGE_HOURS}" in
'' | *[!0-9]* | 0) usage_error "BACKUP_MAX_AGE_HOURS must be a positive integer, got: ${BACKUP_MAX_AGE_HOURS}" ;;
esac
[ "${TEXTFILE_SET}" = "0" ] || [ -n "${TEXTFILE}" ] || usage_error "--textfile must not be empty"

MAX_AGE_SECONDS=$((BACKUP_MAX_AGE_HOURS * 3600))

# file_mtime <file>：输出 unix 秒（GNU stat 与 BSD/macOS stat 都支持）。
file_mtime() {
    stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null
}

# format_ts <unix>：UTC ISO8601（GNU date / BSD date 二选一）。
format_ts() {
    date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null ||
        date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null ||
        printf '@%s' "$1"
}

# format_age <seconds>：给人看的一行年龄。
format_age() {
    local s="$1"
    if [ "${s}" -ge 3600 ]; then
        printf '%dh%02dm' "$((s / 3600))" "$(((s % 3600) / 60))"
    elif [ "${s}" -ge 60 ]; then
        printf '%dm%02ds' "$((s / 60))" "$((s % 60))"
    else
        printf '%ds' "${s}"
    fi
}

# write_textfile <timestamp> <age>：原子写（同目录临时文件 + mv）。失败返回非 0。
write_textfile() {
    local ts="$1" age="$2" dir tmp
    [ -n "${TEXTFILE}" ] || return 0
    dir=$(dirname "${TEXTFILE}")
    [ -d "${dir}" ] || {
        log_err "textfile directory does not exist: ${dir}"
        return 1
    }
    [ ! -d "${TEXTFILE}" ] || {
        log_err "textfile path is a directory: ${TEXTFILE}"
        return 1
    }
    tmp="${TEXTFILE}.tmp.$$"
    TMP_METRIC="${tmp}"
    {
        printf '# HELP rgbx_backup_last_success_timestamp_seconds Unix time of the most recent successful cold backup (0 = none found).\n'
        printf '# TYPE rgbx_backup_last_success_timestamp_seconds gauge\n'
        printf 'rgbx_backup_last_success_timestamp_seconds %s\n' "${ts}"
        printf '# HELP rgbx_backup_age_seconds Seconds since the most recent successful cold backup (-1 = none found).\n'
        printf '# TYPE rgbx_backup_age_seconds gauge\n'
        printf 'rgbx_backup_age_seconds %s\n' "${age}"
    } >"${tmp}" || {
        log_err "cannot write textfile: ${tmp}"
        return 1
    }
    mv -f "${tmp}" "${TEXTFILE}" || {
        log_err "cannot replace textfile: ${TEXTFILE}"
        return 1
    }
    TMP_METRIC=""
    return 0
}

finish() {
    local code="$1"
    if ! write_textfile "$2" "$3"; then
        exit 2
    fi
    exit "${code}"
}

NOW=$(date +%s)

# 路径存在但不是目录 ⇒ 用法/配置错误；目录不存在（例如备份卷没挂上）⇒ 按「没有成功备份」告警。
if [ -e "${BACKUP_OUT_DIR}" ] && [ ! -d "${BACKUP_OUT_DIR}" ]; then
    usage_error "BACKUP_OUT_DIR is not a directory: ${BACKUP_OUT_DIR}"
fi
if [ ! -d "${BACKUP_OUT_DIR}" ]; then
    log_err "backup dir not found (is it mounted?): ${BACKUP_OUT_DIR}"
    printf 'FAIL backup freshness: no successful backup in %s (dir not found, threshold=%sh)\n' \
        "${BACKUP_OUT_DIR}" "${BACKUP_MAX_AGE_HOURS}"
    finish 1 0 -1
fi

# 候选包按 mtime 从新到旧排序（TAB 分隔；路径里含空格也安全）。
TMP_LIST=$(mktemp) || usage_error "mktemp failed"
while IFS= read -r -d '' _f; do
    _m=$(file_mtime "${_f}") || continue
    printf '%s\t%s\n' "${_m}" "${_f}"
done < <(find "${BACKUP_OUT_DIR}" -maxdepth 1 -type f -name '*-backup-*.tar' -print0) |
    LC_ALL=C sort -rn >"${TMP_LIST}"

PACKAGE=""
PACKAGE_PATH=""
PACKAGE_MTIME=""
while IFS=$'\t' read -r _mtime _path; do
    [ -n "${_path}" ] || continue
    _name=$(basename "${_path}")
    _pkg="${_name%.tar}"
    # 同名 staging 目录还在 ⇒ 这次备份未完成（或失败残留）⇒ 跳过，继续看上一份。
    if [ -e "${BACKUP_OUT_DIR}/${_pkg}" ]; then
        log_warn "skipping incomplete backup (staging dir still present): ${_name}"
        continue
    fi
    if [ ! -s "${_path}" ]; then
        log_warn "skipping empty backup artifact: ${_name}"
        continue
    fi
    PACKAGE="${_pkg}"
    PACKAGE_PATH="${_path}"
    PACKAGE_MTIME="${_mtime}"
    break
done <"${TMP_LIST}"

if [ -z "${PACKAGE}" ]; then
    log_err "no successful backup found in ${BACKUP_OUT_DIR} (no <label>-backup-<tag>.tar with a completed staging step)"
    printf 'FAIL backup freshness: no successful backup in %s (threshold=%sh)\n' \
        "${BACKUP_OUT_DIR}" "${BACKUP_MAX_AGE_HOURS}"
    finish 1 0 -1
fi

AGE=$((NOW - PACKAGE_MTIME))
if [ "${AGE}" -lt 0 ]; then
    log_warn "backup mtime is in the future (${PACKAGE_PATH}); clock skew? treating age as 0"
    AGE=0
fi

if [ "${AGE}" -le "${MAX_AGE_SECONDS}" ]; then
    printf 'PASS backup freshness: latest=%s finished=%s age=%s threshold=%sh dir=%s\n' \
        "${PACKAGE}" "$(format_ts "${PACKAGE_MTIME}")" "$(format_age "${AGE}")" \
        "${BACKUP_MAX_AGE_HOURS}" "${BACKUP_OUT_DIR}"
    finish 0 "${PACKAGE_MTIME}" "${AGE}"
else
    printf 'FAIL backup freshness: latest=%s finished=%s age=%s threshold=%sh dir=%s\n' \
        "${PACKAGE}" "$(format_ts "${PACKAGE_MTIME}")" "$(format_age "${AGE}")" \
        "${BACKUP_MAX_AGE_HOURS}" "${BACKUP_OUT_DIR}"
    finish 1 "${PACKAGE_MTIME}" "${AGE}"
fi
