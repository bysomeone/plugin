#!/usr/bin/env bash
# 冷备/恢复脚本的公共部分（冷备 = 先停进程，再打包；见同目录 cold_backup.sh / restore_backup.sh）。
#
# 设计约束（为什么长这样）：
#   1. **不认识 Docker**。脚本只认"数据目录 + 停/起钩子"，所以同一份脚本既能在 CI 的 compose 拓扑里
#      被 docker cp/挂卷驱动，也能在生产里直接对本地 datadir 跑（停/起换成 systemctl 或手工命令）。
#   2. **清单是可机器校验的纯文本**（TSV + `#` 头），不依赖 jq/python —— 生产与 CI 的最小环境
#      （ubuntu:22.04 基础镜像）都只有 coreutils/tar/gzip。
#   3. **每一位成员文件**都记 相对路径/大小/sha256：这是"产物可读、校验和对得上"的唯一判据，
#      也是恢复后逐文件复核的依据（见 BACKUP_RECOVERY.md §3.4 的自检清单）。
#
# 清单格式（manifest.tsv）：
#   # bridge-cold-backup manifest v1
#   # tag=<备份标识>            created=<ISO8601>      host=<hostname>
#   # source=<label>:<绝对路径> （每个源一行）
#   # required=<label>:<相对路径>（关键成员，缺一个就判包不完整）
#   # version.<key>=<value>     （chain33 / sidecar / plugin 的版本或 git describe）
#   # size.bytes=<归档字节数>   file.count=<成员文件数>
#   file<TAB>label<TAB>relpath<TAB>size<TAB>sha256
#
# 用法：由 cold_backup.sh / restore_backup.sh `source` 本文件；不要直接执行。

set -o pipefail

BACKUP_LIB_TAB=$'\t'

log_info() { printf '[backup] %s\n' "$*"; }
log_warn() { printf '[backup][WARN] %s\n' "$*" >&2; }
log_err() { printf '[backup][ERROR] %s\n' "$*" >&2; }

# die 打印错误并退出（非 0）。调用方负责在此之前把服务起回来（见各脚本的 _ensure_started）。
die() {
    log_err "$*"
    exit 1
}

# require_cmd 缺工具就直接失败：宁可不动手，也不产出半成品（半成品的备份比没有备份更危险）。
require_cmd() {
    local c
    for c in "$@"; do
        command -v "${c}" >/dev/null 2>&1 || die "required command not found: ${c}"
    done
}

# parse_sources 把 `label=path` 列表（换行或空格分隔）落到全局数组 SOURCE_LABELS / SOURCE_PATHS。
# 允许同一个 label 既来自 env 又来自 --source（后者追加），重复 label 直接失败（避免静默覆盖）。
declare -a SOURCE_LABELS=()
declare -a SOURCE_PATHS=()

add_source() {
    local entry="$1"
    local label="${entry%%=*}"
    local path="${entry#*=}"
    [ "${entry}" != "${path}" ] || die "--source/BACKUP_SOURCES entry must be label=path, got: ${entry}"
    [ -n "${label}" ] || die "source label empty in: ${entry}"
    [ -n "${path}" ] || die "source path empty in: ${entry}"
    local i
    for ((i = 0; i < ${#SOURCE_LABELS[@]}; i++)); do
        [ "${SOURCE_LABELS[i]}" != "${label}" ] || die "duplicate source label: ${label}"
    done
    SOURCE_LABELS+=("${label}")
    SOURCE_PATHS+=("${path}")
}

# parse_label_path_list 解析多行 `label=path` / `label:path`（分隔符可选），回调分别是 add_source
# 与 add_required —— 两类清单的语义不同（一个是目录，一个是相对路径），所以用两套入口。
declare -a REQUIRED_ENTRIES=()

add_required() {
    local entry="$1"
    local i
    for ((i = 0; i < ${#REQUIRED_ENTRIES[@]}; i++)); do
        [ "${REQUIRED_ENTRIES[i]}" != "${entry}" ] || return 0 # 去重：CLI/env 与清单里的同一条不必核两遍
    done
    REQUIRED_ENTRIES+=("${entry}")
}

# feed_list <mode> <text>：把多行文本逐行喂给 add_source / add_required，忽略空行与 # 注释。
feed_list() {
    local mode="$1"
    local text="$2"
    local line
    while IFS= read -r line; do
        line="${line#"${line%%[![:space:]]*}"}" # ltrim
        line="${line%"${line##*[![:space:]]}"}" # rtrim
        [ -n "${line}" ] || continue
        case "${line}" in
        \#*) continue ;;
        esac
        if [ "${mode}" = "source" ]; then
            add_source "${line}"
        else
            add_required "${line}"
        fi
    done <<<"${text}"
}

# run_hook <描述> <命令>：钩子失败即 die（调用方保证先尝试把服务起回来）。
run_hook() {
    local what="$1"
    local cmd="$2"
    [ -n "${cmd}" ] || return 0
    log_info "hook(${what}): ${cmd}"
    if ! bash -c "${cmd}"; then
        die "hook(${what}) failed: ${cmd}"
    fi
}

# manifest_path_for <dir> —— 清单文件名固定，便于人工排查。
manifest_path_for() { printf '%s/manifest.tsv' "$1"; }

# report_versions 把版本信息汇总成 manifest 的头部行（来源是 env：CI 传 git describe，生产传版本号）。
version_header_lines() {
    local key
    for key in chain33 sidecar plugin plugin_commit; do
        local var="BACKUP_VERSION_$(printf '%s' "${key}" | tr '[:lower:]-' '[:upper:]_')"
        local val="${!var:-}"
        [ -n "${val}" ] || val="unknown"
        printf '# version.%s=%s\n' "${key}" "${val}"
    done
}

# --- 成员文件枚举与清单 ---
#
# 只登记**普通文件**（-type f）：符号链接/设备文件不进清单（本场景的数据目录里不该有它们；
# 真出现了也不该让备份"看起来成功"—— 由 verify 的"多出来的文件"检查兜住）。

# list_source_files <dir>：输出相对路径（相对该源根），按 C 排序保证同一份数据两次备份得到同一清单。
list_source_files() {
    local dir="$1"
    (cd "${dir}" && find . -type f ! -name 'lost+found' -print) | sed 's|^\./||' | LC_ALL=C sort
}

# count_source_files <dir>：成员文件数（用 wc 读完整条流，避免 head 提前退出触发 SIGPIPE +
# `set -o pipefail` 把赋值判成失败）。
count_source_files() {
    list_source_files "$1" | wc -l | tr -d ' '
}

# write_manifest <manifest> <tag> ：按 SOURCE_* 枚举每一位成员，算 size+sha256。
# 返回（通过 stdout）成员文件数。失败即 die。
write_manifest() {
    local manifest="$1"
    local tag="$2"
    {
        printf '# bridge-cold-backup manifest v1\n'
        printf '# tag=%s\n' "${tag}"
        printf '# created=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        printf '# host=%s\n' "${HOSTNAME:-$(uname -n 2>/dev/null || echo unknown)}"
        local i
        for ((i = 0; i < ${#SOURCE_LABELS[@]}; i++)); do
            printf '# source=%s:%s\n' "${SOURCE_LABELS[i]}" "${SOURCE_PATHS[i]}"
        done
        local r
        for r in ${REQUIRED_ENTRIES+"${REQUIRED_ENTRIES[@]}"}; do
            printf '# required=%s\n' "${r}"
        done
        version_header_lines
        # TSS 代次（DKG rid / refresh 记录）：从各源的 neutrino.db 现抠，见 tss_generation_lines 的说明。
        tss_generation_lines
        printf '# file.count=PLACEHOLDER\n'
    } >"${manifest}" || die "cannot write manifest: ${manifest}"

    local count=0
    local i rel abs size sha
    for ((i = 0; i < ${#SOURCE_LABELS[@]}; i++)); do
        local label="${SOURCE_LABELS[i]}"
        local root="${SOURCE_PATHS[i]}"
        while IFS= read -r rel; do
            [ -n "${rel}" ] || continue
            abs="${root}/${rel}"
            size=$(wc -c <"${abs}" | tr -d ' ')
            sha=$(sha256sum "${abs}" | awk '{print $1}')
            printf 'file\t%s\t%s\t%s\t%s\n' "${label}" "${rel}" "${size}" "${sha}" >>"${manifest}" ||
                die "cannot append to manifest: ${manifest}"
            count=$((count + 1))
        done < <(list_source_files "${root}")
    done

    # 回填计数（清单头部要保持在前，所以用整体重写而不是 append）
    sed -i.bak "s/^# file.count=PLACEHOLDER\$/# file.count=${count}/" "${manifest}" &&
        rm -f "${manifest}.bak" || die "cannot finalize manifest: ${manifest}"
    printf '%s' "${count}"
}

# --- TSS 代次（DKG rid / refresh 记录）：让包**自带"我是哪一代"** ---
#
# 为什么要记：B1 那两份材料（`cggmp-dkg-result` / `cggmp-refresh-result`）是同一把钥的不同代产物，
# 恢复时"这份包是哪一代"必须能从包里看出来，而不是靠人对时间/文件名。rid 由 DKG 生成、每把钥唯一
# （`cggmp/result.go` 的 `Rid`），refresh 轮的会话名也从它派生（`tss.go` 的 `refreshSessionName(rid)`），
# 所以 rid 就是"代次"的标识。
#
# 怎么读（**不依赖任何工具**）：这两条记录的值是 JSON，落在 bbolt（`neutrino.db`）的数据页里是
# **原样连续**的字节，所以 `grep -a` 能直接抠出字段：
#   - `cggmp-refresh-result` 的值里带 `"dkgRid":"<hex>"`（`tss.go` 的 refreshRecord）⇒ 直接是十六进制；
#   - `cggmp-dkg-result` 的值里带 `"rid":"<base64>"`（`cggmp/result.go` 的 `Rid []byte`，Go 的
#     encoding/json 把 []byte 编成 base64）⇒ 解成十六进制便于比对。
# 抠不到（首次启动、键被清、格式变了）一律写 `unknown`，**绝不留空** —— "没写"和"读不到"必须能区分。
#
# ⚠️ 局限（写在这里免得被当成保证）：这只证明**包里那份 DB 的字段值**；它证明不了 share 本身完好
# （BACKUP_RECOVERY.md 的缺口 G2 仍未解）。另外 refresh **不会**改 rid（rid 标识的是这把钥/这轮 DKG），
# 它换的是 refresh 材料 ⇒ 刷过 refresh 之后包就"过代"了，见 restore_backup.sh 头部与包内 README。
TSS_DB_RELPATH_DEFAULT="lightclient/neutrino.db"

# tss_rid_from_db <neutrino.db 路径>：输出 `dkgRid=... rid=... refreshRecord=present|absent`。
tss_rid_from_db() {
    local db="$1"
    local dkg_rid="unknown" rid_b64="unknown" rid_hex="unknown" refresh_rec="unknown" rid_mismatch="no"
    if [ -f "${db}" ]; then
        # 有没有 refresh 记录（bbolt 的 bucket/key 名也是原样存的）
        if grep -aq 'cggmp-refresh-result' "${db}"; then
            refresh_rec="present"
        else
            refresh_rec="absent"
        fi
        # refresh 记录里的 dkgRid（hex，且是两条记录里**后写**的那条 ⇒ 口径优先）
        local v
        v=$(grep -ao '"dkgRid":"[0-9a-fA-F]\{8,\}"' "${db}" 2>/dev/null | tail -1 | sed 's/.*:"//; s/"$//')
        [ -n "${v}" ] && dkg_rid="${v}"
        # DKG 记录里的 rid（base64 → hex），仅作交叉核对/兜底
        v=$(grep -ao '"rid":"[A-Za-z0-9+/]\{8,\}=\{0,2\}"' "${db}" 2>/dev/null | tail -1 | sed 's/.*:"//; s/"$//')
        if [ -n "${v}" ]; then
            rid_b64="${v}"
            if command -v base64 >/dev/null 2>&1 && command -v od >/dev/null 2>&1; then
                local decoded
                # od 必须带 -v：默认会把连续重复行折叠成一行 `*`，16 字节全相同就解不出正确的十六进制。
                decoded=$(printf '%s' "${v}" | base64 -d 2>/dev/null | od -An -v -tx1 2>/dev/null | tr -d ' \n')
                [ -n "${decoded}" ] && rid_hex="${decoded}"
            fi
        fi
        # 两条记录必须指向同一把钥（它们由同一次 DKG 派生）：不一致说明抠到了僵尸/过期页，
        # 此时"代次"不可信 —— 宁可报出来，也不要给一个看着像真的值。
        if [ "${dkg_rid}" != "unknown" ] && [ "${rid_hex}" != "unknown" ] && [ "${dkg_rid}" != "${rid_hex}" ]; then
            rid_mismatch="yes"
            log_warn "the two TSS records in ${db} disagree on the DKG rid (refresh record=${dkg_rid}, DKG record=${rid_hex}): the extracted generation is NOT trustworthy"
        fi
        # 口径：refresh 记录的 dkgRid（后写、且是 hex，不依赖 base64/od）
        [ "${dkg_rid}" != "unknown" ] && rid_hex="${dkg_rid}"
    fi
    # ridBase64 只在解不出十六进制时作为兜底证据带上（解得出就不必让清单里多一串 base64）
    if [ "${rid_hex}" = "unknown" ] && [ "${rid_b64}" != "unknown" ]; then
        printf 'dkgRid=%s rid=unknown ridBase64=%s refreshRecord=%s ridMismatch=%s\n' \
            "${dkg_rid}" "${rid_b64}" "${refresh_rec}" "${rid_mismatch}"
    else
        printf 'dkgRid=%s rid=%s refreshRecord=%s ridMismatch=%s\n' \
            "${dkg_rid}" "${rid_hex}" "${refresh_rec}" "${rid_mismatch}"
    fi
}

# tss_generation_lines <sources...>：对每个源试抠一代次，输出 manifest 头部行。
# 每个源一行 + 一个"是否一致"的判断：**几份 share 不是同一代**是必须一眼看见的坏包形态
# （恢复出来只有一部分节点能签名）。
tss_generation_lines() {
    local i label root db out line
    local -a rids=()
    for ((i = 0; i < ${#SOURCE_LABELS[@]}; i++)); do
        label="${SOURCE_LABELS[i]}"
        root="${SOURCE_PATHS[i]}"
        db="${root}/${TSS_DB_RELPATH_DEFAULT}"
        [ -f "${db}" ] || continue
        out=$(tss_rid_from_db "${db}")
        printf '# tss.%s.%s\n' "${label}" "${out}"
        rids+=("$(printf '%s' "${out}" | sed 's/.*rid=\([^ ]*\).*/\1/')")
    done
    if [ ${#rids[@]} -eq 0 ]; then
        printf '# tss.generation=unknown\n'
        printf '# tss.generation.distinct=0\n'
        return 0
    fi
    local uniq_count
    uniq_count=$(printf '%s\n' "${rids[@]}" | LC_ALL=C sort -u | wc -l | tr -d ' ')
    printf '# tss.generation=%s\n' "${rids[0]}"
    printf '# tss.generation.distinct=%s\n' "${uniq_count}"
    if [ "${uniq_count}" != "1" ]; then
        log_warn "the ${#rids[@]} share DB(s) in this package do NOT all report the same DKG generation (distinct=${uniq_count}): the package mixes generations, and a restore from it would leave some nodes unable to sign"
        printf '# tss.generation.WARNING=mixed\n'
    fi
}

# manifest_header <manifest> <key>：取 `# key=value` 里第一个匹配的值（key 里含 `.` 与 `=` 都安全）。
manifest_header() {
    local manifest="$1"
    local key="$2"
    grep -m1 "^# ${key}=" "${manifest}" 2>/dev/null | sed "s/^# ${key}=//"
}

# manifest_entries <manifest>：输出 file 行（供逐行解析）。用字面 TAB 的 BRE 模式（GNU/BSD grep 都认），
# 不依赖 grep -P（macOS 的 BSD grep 没有）。
manifest_entries() {
    grep "^file$(printf '\t')" "$1" 2>/dev/null || true
}

# extra 成员（`extra/<label>` 形式的单文件，见 BACKUP_EXTRA_FILES）不算任何一个"源"，因此凡是
# 按 label 遍历源的地方都要把它们排除掉。
manifest_source_labels() {
    manifest_entries "$1" | awk -F'\t' '$3 !~ /^extra\// {print $2}' | LC_ALL=C sort -u
}

manifest_extra_entries() {
    manifest_entries "$1" | awk -F'\t' '$3 ~ /^extra\// {print}'
}

# manifest_file_count <manifest>
manifest_file_count() {
    manifest_entries "$1" | wc -l | tr -d ' '
}

# verify_tree_against_manifest <manifest> <label> <dir>
# 逐文件核对 size+sha256，并检查"清单里有、目录里没有"与"目录里有、清单里没有"两种偏差。
# 这是"产物可读、校验和对得上"的唯一实现：备份自检与恢复后复核都用它。
verify_tree_against_manifest() {
    local manifest="$1"
    local label="$2"
    local dir="$3"
    local problems=0

    [ -d "${dir}" ] || die "verify: not a directory: ${dir}"

    # 目录里实际有什么
    local actual_list
    actual_list=$(mktemp) || die "mktemp failed"
    list_source_files "${dir}" >"${actual_list}"

    local expected_list
    expected_list=$(mktemp) || die "mktemp failed"
    awk -F'\t' -v l="${label}" '$1=="file" && $2==l && $3 !~ /^extra\// {print $3}' "${manifest}" |
        LC_ALL=C sort >"${expected_list}"

    local missing extra
    missing=$(comm -23 "${expected_list}" "${actual_list}")
    extra=$(comm -13 "${expected_list}" "${actual_list}")
    if [ -n "${missing}" ]; then
        log_err "verify(${label}): files listed in the manifest are MISSING:"
        printf '%s\n' "${missing}" >&2
        problems=$((problems + 1))
    fi
    if [ -n "${extra}" ]; then
        log_err "verify(${label}): files present but NOT in the manifest:"
        printf '%s\n' "${extra}" >&2
        problems=$((problems + 1))
    fi

    local n=0
    local range
    while IFS="${BACKUP_LIB_TAB}" read -r _ l rel size sha; do
        [ "${l}" = "${label}" ] || continue
        case "${rel}" in extra/*) continue ;; esac
        n=$((n + 1))
        [ -n "${rel}" ] || continue
        local abs="${dir}/${rel}"
        if [ ! -f "${abs}" ]; then
            log_err "verify(${label}): missing file ${rel}"
            problems=$((problems + 1))
            continue
        fi
        local got_size got_sha
        got_size=$(wc -c <"${abs}" | tr -d ' ')
        got_sha=$(sha256sum "${abs}" | awk '{print $1}')
        if [ "${got_size}" != "${size}" ]; then
            log_err "verify(${label}): size mismatch ${rel}: manifest=${size} actual=${got_size}"
            problems=$((problems + 1))
        fi
        if [ "${got_sha}" != "${sha}" ]; then
            log_err "verify(${label}): sha256 mismatch ${rel}: manifest=${sha} actual=${got_sha}"
            problems=$((problems + 1))
        fi
    done < <(manifest_entries "${manifest}")

    rm -f "${actual_list}" "${expected_list}"

    if [ "${n}" -eq 0 ]; then
        log_err "verify(${label}): the manifest lists no file for this label"
        problems=$((problems + 1))
    fi

    [ "${problems}" -eq 0 ] || return 1
    log_info "verify(${label}): OK (${n} file(s) match size+sha256)"
    return 0
}

# check_required_paths <dir-of-source> <label> ：关键成员存在性。REQUIRED_ENTRIES 形如 `label:relpath`。
# 目的：让"包不完整"在备份时就暴露（例如把 sidecar 卷错挂成 /data 之外的目录、node datadir 指错），
# 而不是等到恢复时才发现少了一半状态。
check_required_paths() {
    local root="$1"
    local label="$2"
    local entry rel problems=0 checked=0
    for entry in ${REQUIRED_ENTRIES+"${REQUIRED_ENTRIES[@]}"}; do
        case "${entry}" in
        "${label}:"*) rel="${entry#"${label}:"}" ;;
        *) continue ;;
        esac
        checked=$((checked + 1))
        if [ ! -e "${root}/${rel}" ]; then
            log_err "required member missing: ${label}:${rel} (under ${root})"
            problems=$((problems + 1))
        fi
    done
    [ "${checked}" -gt 0 ] || return 0
    [ "${problems}" -eq 0 ] || return 1
    return 0
}

# required_entries_for_label <label> ：输出该 label 的 required 相对路径（恢复后复核用）。
required_entries_for_label() {
    local label="$1"
    local entry
    for entry in ${REQUIRED_ENTRIES+"${REQUIRED_ENTRIES[@]}"}; do
        case "${entry}" in
        "${label}:"*) printf '%s\n' "${entry#"${label}:"}" ;;
        esac
    done
}
