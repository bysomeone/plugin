package commands

import (
	"encoding/hex"
	"strings"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 S2 的**可审计入口**（CLI）：命令是否挂上、参数校验是否在发 RPC 之前拦下、
// operationId 的十六进制口径是否与链上一致（32 字节）。
//
// 为什么值得测：审计流程是从 CLI 开始的（"这个 operationId 到底链上记了什么"）。CLI 若把
// id 解析错（少个字节、把 0x 当内容），查到的会是"没有这条记录"，把排查引向错误方向。

func findSubCmd(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, sub := range root.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	t.Fatalf("subcommand %q not found", name)
	return nil
}

// Test_operationCommandsAreRegistered S2 的三条查询命令挂在 rgbx 下，且带预期别名。
func Test_operationCommandsAreRegistered(t *testing.T) {
	root := Cmd()
	cases := []struct {
		name    string
		aliases []string
	}{
		{"getOperation", []string{"go"}},
		{"getOperationSupply", []string{"gos"}},
		{"listOperations", []string{"lo"}},
	}
	for _, tc := range cases {
		cmd := findSubCmd(t, root, tc.name)
		require.ElementsMatch(t, tc.aliases, cmd.Aliases, "%s 的别名", tc.name)
	}
}

// Test_listOperationsCommandDefaults flag 默认值（kind 默认铸造、count 默认 20、start 默认 0），
// 以及"必填 symbol"的约束存在。
func Test_listOperationsCommandDefaults(t *testing.T) {
	cmd := findSubCmd(t, Cmd(), "listOperations")

	kind, err := cmd.Flags().GetInt32("kind")
	require.NoError(t, err)
	require.Equal(t, rtypes.OperationKindMint, kind)
	start, err := cmd.Flags().GetInt64("start")
	require.NoError(t, err)
	require.Equal(t, int64(0), start)
	count, err := cmd.Flags().GetInt32("count")
	require.NoError(t, err)
	require.Equal(t, int32(20), count)
	require.NoError(t, cmd.MarkFlagRequired("symbol"))

	get := findSubCmd(t, Cmd(), "getOperation")
	require.NoError(t, get.MarkFlagRequired("operationId"))
}

// Test_parseOperationIDHex 口径：32 字节 hex（容错 0x 前缀 / 空白 / 大写），长度不对必须报错。
func Test_parseOperationIDHex(t *testing.T) {
	raw := strings.Repeat("ab", rtypes.OperationIDLen) // 32 字节
	want, err := hex.DecodeString(raw)
	require.NoError(t, err)

	for _, in := range []string{raw, "0x" + raw, " 0X" + strings.ToUpper(raw) + "\n"} {
		got, err := parseOperationIDHex(in)
		require.NoErrorf(t, err, "input=%q", in)
		require.Equal(t, want, got)
	}

	// 少一个字节 / 多一个字节：长度不对（错误信息必须说明"期望 32 字节"，否则排查会被引偏）
	for _, in := range []string{raw[:62], raw + "ab"} {
		_, err := parseOperationIDHex(in)
		require.Errorf(t, err, "input=%q", in)
		require.Contains(t, err.Error(), "32")
	}
	// 非 hex / 空串：解析错误原样上抛
	for _, in := range []string{"zz", ""} {
		_, err := parseOperationIDHex(in)
		require.Errorf(t, err, "input=%q", in)
	}
}

// Test_validateListOperationsArgs 列举参数校验。
func Test_validateListOperationsArgs(t *testing.T) {
	require.NoError(t, validateListOperationsArgs("XBTC", rtypes.OperationKindMint, 0, 20))
	require.NoError(t, validateListOperationsArgs("xbtc", rtypes.OperationKindBurn, 5, 1))

	require.Error(t, validateListOperationsArgs("", rtypes.OperationKindMint, 0, 20))
	require.Error(t, validateListOperationsArgs("XBTC", 0, 0, 20))
	require.Error(t, validateListOperationsArgs("XBTC", 99, 0, 20))
	require.Error(t, validateListOperationsArgs("XBTC", rtypes.OperationKindMint, -1, 20))
	require.Error(t, validateListOperationsArgs("XBTC", rtypes.OperationKindMint, 0, 0))
}
