#!/usr/bin/env bash

set -e
set -o pipefail

function save_seed_and_unlock() {
    local cli="${1}"
    ${cli} seed save -p 1314fuzamei -s "tortoise main civil member grace happy century convince father cage beach hip maid merry rib" >/dev/null
    ${cli} wallet unlock -p 1314fuzamei -t 0 >/dev/null
}

function import_key_idempotent() {
    # account import_key 对已存在的 label 直接报 ErrLabelHasUsed。原地续跑（不 reset 直接再跑
    # run，见 docker-compose.sh 里 setup_para_nodegroup_on_main 的幂等说明：btcd 的 regtest 链
    # 不能重建，想连续跑就必须能对同一条链重跑）时钱包卷还在、label 早已导入，这是**预期情况**，
    # 不该让整个 E2E 挂在这一步；但其它错误必须照旧报出来，否则“key 没导进去”会被静默放过，
    # 后续用例只会以更难懂的方式失败。
    local cli="${1}"
    local key="${2}"
    local label="${3}"
    local out
    if out=$(${cli} account import_key -k "${key}" -l "${label}" 2>&1); then
        return 0
    fi
    if echo "${out}" | grep -q "ErrLabelHasUsed"; then
        log_step "key ${label} already imported, skip (in-place rerun)"
        return 0
    fi
    fail "import_key ${label} failed: ${out}"
}

function import_default_keys() {
    local cli="${1}"
    import_key_idempotent "${cli}" 4257D8692EF7FE13C68B65D6A52F03933DB2FA5CE8FAF210B5B8B80C721CED01 minerAddr
    import_key_idempotent "${cli}" CC38546E9E659D15E6B4893F0AB32A06D103931A8230B0BDE71459D2B27D6944 returnAddr
}

function enable_mining() {
    local cli="${1}"
    ${cli} wallet auto_mine -f 1 >/dev/null
}
