package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino"
	"github.com/stretchr/testify/require"
)

/*
 * 导出侧护栏：只允许导出"能签名"的归档。
 *
 * 首次 refresh 成功后节点会抹掉 DKG 根 share，之后签名材料只剩 refresh 记录。
 * 因此导出的三种组合必须是：
 *   - DKG 带 share + 无 refresh  → 拒绝（尚未完成首次 refresh；归档不能签，还白泄露根 share）
 *   - DKG 无 share + 无 refresh  → 拒绝（材料不全，无法签名）
 *   - 有 refresh 记录            → 放行（无论 DKG 是否还带 share；兼容旧记录）
 */

// TestExportRefusesDKGShareWithoutRefresh
// DKG 仍带根 share 且没有 refresh 记录 ⇒ 拒绝导出，且不留下归档文件。
//
// 反向：去掉导出护栏后，exportTSS 会成功写出归档（本用例的 error/文件不存在断言变红）。
func TestExportRefusesDKGShareWithoutRefresh(t *testing.T) {
	dir := t.TempDir()
	dkg, _ := testDKG(t, 0x61)
	dkgJSON, err := json.Marshal(dkg)
	require.NoError(t, err)
	src := filepath.Join(dir, "source.db")
	createTestDB(t, src, map[string][]byte{neutrino.TSSDKGResultKey: dkgJSON})
	out := filepath.Join(dir, "archive.json")

	_, err = exportTSS(exportOptions{DBPath: src, OutPath: out, Warn: io.Discard})
	require.Error(t, err)
	require.Contains(t, err.Error(), "root share", "要说明拒绝理由：会泄露根 share")
	require.Contains(t, err.Error(), neutrino.TSSRefreshResultKey)
	_, statErr := os.Stat(out)
	require.True(t, os.IsNotExist(statErr), "被拒绝的导出不得留下归档文件")
}

// TestExportRefusesIncompleteMaterial
// DKG 无 share 且无 refresh 记录 ⇒ 拒绝导出（材料不全，归档无法签名）。
//
// 反向：去掉导出护栏后，decodeDKG 现在允许无 share（这是为兼容首次 refresh 后的记录），
// exportTSS 会成功导出 —— 本用例的 error 断言变红。
func TestExportRefusesIncompleteMaterial(t *testing.T) {
	dir := t.TempDir()
	dkg, _ := testDKG(t, 0x62)
	dkg.Share = nil
	dkgJSON, err := json.Marshal(dkg)
	require.NoError(t, err)
	src := filepath.Join(dir, "source.db")
	createTestDB(t, src, map[string][]byte{neutrino.TSSDKGResultKey: dkgJSON})
	out := filepath.Join(dir, "archive.json")

	_, err = exportTSS(exportOptions{DBPath: src, OutPath: out, Warn: io.Discard})
	require.Error(t, err)
	require.Contains(t, err.Error(), "incomplete")
	_, statErr := os.Stat(out)
	require.True(t, os.IsNotExist(statErr), "被拒绝的导出不得留下归档文件")
}

// TestExportAllowsStrippedDKGWithRefresh
// 首次 refresh 后的正常形态：DKG 记录无根 share、refresh 记录存在 ⇒ 照常导出，
// 且导出的归档能按原样导入（这是清掉 share 之后唯一的恢复路径）。
//
// 反向：若沿用改动前 decodeDKG 的 "record carries no share" 硬错误，本用例在 export 处就变红。
func TestExportAllowsStrippedDKGWithRefresh(t *testing.T) {
	dir := t.TempDir()
	dkg, wantPub := testDKG(t, 0x63)
	dkg.Share = nil
	dkgJSON, err := json.Marshal(dkg)
	require.NoError(t, err)
	refreshJSON := testRefresh(t, dkg.Rid)
	src := filepath.Join(dir, "source.db")
	createTestDB(t, src, map[string][]byte{
		neutrino.TSSDKGResultKey:     dkgJSON,
		neutrino.TSSRefreshResultKey: refreshJSON,
	})
	out := filepath.Join(dir, "archive.json")

	summary, err := exportTSS(exportOptions{DBPath: src, OutPath: out, Warn: io.Discard})
	require.NoError(t, err)
	require.Equal(t, wantPub, summary.GroupPubKey)
	require.Empty(t, summary.ShareID, "根 share 已抹掉，share id 不再可推导")

	dst := filepath.Join(dir, "restored.db")
	importSummary, err := importTSS(importOptions{FilePath: out, DBPath: dst, Warn: io.Discard})
	require.NoError(t, err)
	require.Equal(t, wantPub, importSummary.GroupPubKey)
	require.Equal(t, 2, importSummary.Entries)

	stored := readDBEntries(t, dst)
	require.Equal(t, dkgJSON, stored[neutrino.TSSDKGResultKey])
	require.Equal(t, refreshJSON, stored[neutrino.TSSRefreshResultKey])
}
