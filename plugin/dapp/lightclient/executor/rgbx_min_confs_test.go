package executor

import (
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/types"
	"github.com/stretchr/testify/require"
)

/*
 * N（链上最小确认数，B8）的单一真相测试。
 *
 * N 只配一份（[exec.sub.rgbx].minBtcConfirmations），由本执行器把它暴露成只读查询
 * Query_GetRgbxMinBtcConfirmations，中继读它、不再自己镜像一份 —— 这里钉住取值口径
 * （与 rgbx 执行器 initCfg 一致：正数用它，未配置/非正数取默认 6）。
 */

func TestRgbxMinBtcConfirmationsFromSub(t *testing.T) {
	tests := []struct {
		name string
		sub  string
		want int64
	}{
		{name: "configured", sub: `{"minBtcConfirmations":9}`, want: 9},
		{name: "configured with other keys", sub: `{"commitAddress":"x","minBtcConfirmations":3}`, want: 3},
		{name: "empty section", sub: `{}`, want: rgbxMinBtcConfirmationsDefault},
		{name: "absent section", sub: "", want: rgbxMinBtcConfirmationsDefault},
		{name: "zero falls back (fail-closed)", sub: `{"minBtcConfirmations":0}`, want: rgbxMinBtcConfirmationsDefault},
		{name: "negative falls back (fail-closed)", sub: `{"minBtcConfirmations":-4}`, want: rgbxMinBtcConfirmationsDefault},
		{name: "malformed falls back", sub: `not json`, want: rgbxMinBtcConfirmationsDefault},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, rgbxMinBtcConfirmationsFromSub([]byte(tc.sub)))
		})
	}

	// 默认值与 rgbx 执行器的 defaultMinBtcConfirmations 是同一个约定（默认 6）。
	require.Equal(t, int64(6), rgbxMinBtcConfirmationsDefault)

	// 没有链配置（绕过 Init 直接构造的调用方）时同样给默认值，不 panic。
	require.Equal(t, rgbxMinBtcConfirmationsDefault, rgbxMinBtcConfirmations(nil))
}

// TestQueryGetRgbxMinBtcConfirmations 走真实链路：从 chain33 配置串里取 [exec.sub.rgbx] 段，
// 经查询返回 N —— 这正是中继（neutrino 的 RgbxMinBtcConfirmations）拿到 N 的路径。
func TestQueryGetRgbxMinBtcConfirmations(t *testing.T) {
	tests := []struct {
		name string
		sub  string
		want int64
	}{
		{name: "configured", sub: "\n[exec.sub.rgbx]\ncommitAddress=\"x\"\nminBtcConfirmations=9\n", want: 9},
		{name: "absent section", sub: "", want: rgbxMinBtcConfirmationsDefault},
		{name: "zero falls back", sub: "\n[exec.sub.rgbx]\nminBtcConfirmations=0\n", want: rgbxMinBtcConfirmationsDefault},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := types.NewChain33Config(types.GetDefaultCfgstring() + tc.sub)
			cli := newLightclient().(*lightclient)
			api := &mocks.QueueProtocolAPI{}
			api.On("GetConfig").Return(cfg)
			cli.SetAPI(api)

			msg, err := cli.Query_GetRgbxMinBtcConfirmations(&types.ReqNil{})
			require.NoError(t, err)
			reply, ok := msg.(*types.Int64)
			require.True(t, ok)
			require.Equal(t, tc.want, reply.GetData())
		})
	}
}

// TestQueryGetRgbxMinBtcConfirmationsWithoutAPI 执行器 API 未注入（例如单元测试里直接构造）时
// 退化为默认值，而不是 panic。
func TestQueryGetRgbxMinBtcConfirmationsWithoutAPI(t *testing.T) {
	cli := newLightclient().(*lightclient)
	msg, err := cli.Query_GetRgbxMinBtcConfirmations(&types.ReqNil{})
	require.NoError(t, err)
	require.Equal(t, rgbxMinBtcConfirmationsDefault, msg.(*types.Int64).GetData())
}
