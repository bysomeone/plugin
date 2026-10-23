package neutrino

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * 链上桥配置（rgbx 治理的 BridgeConfig）在桥侧的读取入口。
 *
 * 治理实现见 plugin/dapp/rgbx/executor/bridgeconfig.go：配置的权威是**链上共识状态**，
 * 通过 autonomy 治理的 apply/approve 两步生效，查询入口是 rgbx 执行器的
 * Query_GetBridgeConfig（参数 types.ReqString，空串 = 全局记录）。桥侧原先把这些值放在
 * 本地 TOML（userDepositSweep.maxFeeRate、rgb20.contracts[].withdrawFee），本地配置的问题
 * 是每个签名节点各有一份，运维漏配/配错会让节点之间对同一笔交易给出不同结论。改成读链上
 * 之后，所有节点看到的是同一个值。
 *
 * 读取口径（三处消费方共用同一个 reader）：
 *   - 查询成功 → 用链上值；
 *   - 查询失败且**从未成功过** → 返回错误，由调用方回退到内置默认值（WARN）；
 *   - 曾成功过、之后查询失败 → 用最后一次成功值（WARN）；
 *   - 链上值为 0（治理未设置）→ 由调用方回退到内置默认值。
 *
 * 失败绝不向调用方返回"一个看起来正常但未经治理认可的高费率"：调用方拿到的要么是链上
 * （或最近一次链上）的值，要么是错误 —— 由它按各自的安全默认值处理。签名端因此不会把治理
 * 查询故障升级成拒签：RGB20 手续费回退为 0（跳过金额精确比对），扫集上界回退为与协调者
 * 相同的内置默认上限。
 */

const (
	// defaultBridgeConfigTTL 链上配置的缓存有效期。治理变更不会每轮都发生，30s 足以让
	// 桥跟上治理节奏，又不会在每个提现/扫集轮次里都打一次主链查询。
	defaultBridgeConfigTTL = 30 * time.Second
	// bridgeConfigQueryTimeout 单次链上查询的超时。主链 grpc 的 QueryChain 在并发/时序下
	// 可能永久 hang（本仓库有据可查），所以桥侧查询一律带超时。
	bridgeConfigQueryTimeout = 8 * time.Second
)

// bridgeConfigQueryFn 读取一条生效的桥配置记录（symbol 为空 = 全局记录）。
// 抽成函数类型是为了让缓存与回退逻辑可以脱离主链单测。
type bridgeConfigQueryFn func(symbol string) (*rtypes.BridgeConfigApply, error)

// bridgeConfigEntry 某个 symbol 的缓存项。
//
// last 是**最后一次成功**的值（查询失败时不覆盖它，所以"曾经成功过"这个条件不依赖 TTL）；
// lastErr 是最近一次查询的失败结果；checkedAt 是最近一次查询的时间（成功或失败都更新），
// 用来做 TTL 判定 —— 失败也缓存一个 TTL，避免主链不可用时每个调用点都把查询重打一遍。
type bridgeConfigEntry struct {
	last      *rtypes.BridgeConfigApply
	lastErr   error
	checkedAt time.Time
}

// bridgeConfigReader 带 TTL 缓存的桥配置读取器。并发安全。
type bridgeConfigReader struct {
	ttl   time.Duration
	query bridgeConfigQueryFn
	// now 可注入，单测用它推进时钟验证 TTL 与"失败后用上次成功值"。
	now func() time.Time

	mu    sync.Mutex
	cache map[string]*bridgeConfigEntry
}

func newBridgeConfigReader(query bridgeConfigQueryFn, ttl time.Duration) *bridgeConfigReader {
	if ttl <= 0 {
		ttl = defaultBridgeConfigTTL
	}
	return &bridgeConfigReader{
		ttl:   ttl,
		query: query,
		now:   time.Now,
		cache: make(map[string]*bridgeConfigEntry),
	}
}

// get 返回 symbol 的生效配置。
//
// 缓存命中（含失败结果的缓存）直接返回；否则查询一次链上：
//   - 成功：写入 last，清掉 lastErr；
//   - 失败：保留 last。有 last 时返回 (last, nil)（WARN），没有时返回 (nil, err)（WARN）。
//
// 返回值语义：err == nil 时 cfg 是链上当前值或最后一次成功值；err != nil 时 cfg 一定为 nil，
// 表示"从未读到过链上值"，调用方应回退到内置默认值。
func (r *bridgeConfigReader) get(symbol string) (*rtypes.BridgeConfigApply, error) {
	if r == nil || r.query == nil {
		return nil, fmt.Errorf("bridge config reader not configured")
	}

	r.mu.Lock()
	entry := r.cache[symbol]
	if entry != nil && r.now().Sub(entry.checkedAt) < r.ttl {
		last, lastErr := entry.last, entry.lastErr
		r.mu.Unlock()
		if lastErr != nil && last == nil {
			return nil, lastErr
		}
		return last, nil
	}
	r.mu.Unlock()

	cfg, err := r.query(symbol)
	if err != nil {
		r.mu.Lock()
		// 重新取一次：查询期间可能有另一个 goroutine 更新过（成功或失败）这个 symbol。
		cur := r.cache[symbol]
		if cur == nil {
			cur = &bridgeConfigEntry{}
			r.cache[symbol] = cur
		}
		cur.lastErr = err
		cur.checkedAt = r.now()
		last := cur.last
		r.mu.Unlock()
		if last != nil {
			log.Warn("bridge config query failed, using the last successful value",
				"symbol", symbol, "err", err)
			return last, nil
		}
		log.Warn("bridge config query failed and no successful value was ever cached, "+
			"falling back to the built-in default", "symbol", symbol, "err", err)
		return nil, err
	}

	if cfg == nil {
		cfg = &rtypes.BridgeConfigApply{}
	}
	r.mu.Lock()
	r.cache[symbol] = &bridgeConfigEntry{last: cfg, checkedAt: r.now()}
	r.mu.Unlock()
	return cfg, nil
}

// queryBridgeConfig 通过链上 rgbx 执行器的 Query_GetBridgeConfig 读取一条配置记录。
func (n *neutrinoClient) queryBridgeConfig(symbol string) (*rtypes.BridgeConfigApply, error) {
	if n == nil || n.mainChainGrpc == nil {
		return nil, fmt.Errorf("main chain client not configured")
	}
	ctx := n.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, bridgeConfigQueryTimeout)
	defer cancel()

	reply, err := n.mainChainGrpc.QueryChain(ctx, &types.ChainExecutor{
		Driver:   rtypes.RgbxX,
		FuncName: "GetBridgeConfig",
		Param:    types.Encode(&types.ReqString{Data: symbol}),
	})
	if err != nil {
		return nil, err
	}
	if !reply.GetIsOk() {
		return nil, fmt.Errorf("query bridge config: %s", string(reply.GetMsg()))
	}
	cfg := &rtypes.BridgeConfigApply{}
	if err := types.Decode(reply.GetMsg(), cfg); err != nil {
		return nil, fmt.Errorf("decode bridge config: %w", err)
	}
	return cfg, nil
}

// withdrawMaxFeeRateSatPerVbyte 返回扫集/提现的费率上限（sat/vB）。
//
// 读链上全局记录的 withdrawMaxFeeRateSatPerVbyte（BTC 提现与 RGB20 的 BTC 付费通道共用，
// 因此是全局而非按资产的旋钮）。以下情况回退到内置默认值 defaultSweepMaxFeeRate：
//   - 查询失败且从未成功过（reader 已打 WARN）；
//   - 链上值为 0（治理未设置）。
//
// 回退值是**有界的安全默认**：它只决定"估算费率超过多少就跳过本轮"，取默认值不会让桥用
// 一个未经治理认可的高费率去扫集。
func (n *neutrinoClient) withdrawMaxFeeRateSatPerVbyte() uint32 {
	if n == nil || n.bridgeConfig == nil {
		return defaultSweepMaxFeeRate
	}
	cfg, err := n.bridgeConfig.get("")
	if err != nil {
		return defaultSweepMaxFeeRate
	}
	if rate := cfg.GetWithdrawMaxFeeRateSatPerVbyte(); rate > 0 {
		return rate
	}
	log.Debug("bridge config withdrawMaxFeeRateSatPerVbyte is unset, using the built-in default",
		"default", defaultSweepMaxFeeRate)
	return defaultSweepMaxFeeRate
}

// rgb20WithdrawFeeForSymbol 返回某链上资产符号的 RGB20 提现手续费（该资产最小单位）。
//
// 读该资产自己的链上配置记录里的 rgb20WithdrawFee 表（按 symbol 匹配，忽略大小写）。返回 0 表示
// "未配置"：不收取，也不做"实际发给用户 == 链上 pending − 手续费"的精确比对（保持旧的宽松口径）。
// 查询失败且从未成功过同样返回 0 —— 签名节点不会因为治理查询不可用而拒签合法提现。
func (n *neutrinoClient) rgb20WithdrawFeeForSymbol(symbol string) int64 {
	if n == nil || n.bridgeConfig == nil || symbol == "" {
		return 0
	}
	cfg, err := n.bridgeConfig.get(symbol)
	if err != nil {
		return 0
	}
	for _, fee := range cfg.GetRgb20WithdrawFee() {
		if strings.EqualFold(fee.GetSymbol(), symbol) {
			return fee.GetFeeMinUnits()
		}
	}
	return 0
}

// rgb20WithdrawFeeLockVbytes 返回 RGB20 的 BTC 付费通道估算 xBTC 锁定上限时采用的保守交易
// 体积（vB），读链上全局记录的 rgb20WithdrawFeeLockVbytes。
//
// 当前桥侧还没有消费方（锁定上限逻辑尚未落地），这里先提供与另两项同源的读取入口，避免
// 后续再写一份查询/缓存。返回 0 表示治理未设置或查询从未成功过。
func (n *neutrinoClient) rgb20WithdrawFeeLockVbytes() uint32 {
	if n == nil || n.bridgeConfig == nil {
		return 0
	}
	cfg, err := n.bridgeConfig.get("")
	if err != nil {
		return 0
	}
	return cfg.GetRgb20WithdrawFeeLockVbytes()
}
