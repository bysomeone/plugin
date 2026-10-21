package neutrino

import (
	"fmt"
	"strings"
	"time"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcwallet/walletdb"
)

/*
 * 链上充值地址登记的桥侧同步（#78）。
 *
 * 用户先在链上登记自己的 BTC 充值地址（rgblight 的 RegisterDepositScript；地址由链上
 * CrossChainInfo.pubkey 现场派生并写入登记表），桥侧再把每一条登记转成 "把这个用户的充值脚本
 * 纳入钱包 watch 集"（ensureUserDepositScript：派生 + import + 订阅 + 落盘，幂等）。
 *
 * 为什么需要轮询而不是只靠 HTTP：登记是链上事实，用户不再经过某个节点的 HTTP 入口；任何官方节点
 * 重启后都必须能自己补回用户在本进程生命周期之外登记过的地址，否则那些地址上的充值对桥不可见。
 *
 * 去重不靠 processed 集合：ensureUserDepositScript 本身就是幂等的（同一 userID 只 import 一次），
 * 所以游标只负责"不要每次从头扫"。唯一需要额外记住的是**导入失败**的 userID（典型是 watch 集已满）：
 * 游标越过它们以后，如果只留在进程内存里，重启就会永久丢失这条登记 —— 那正是"用户地址无人监听、
 * 充值悄悄丢失"的形态，所以失败集合落盘，修复配置/重启后继续重试。
 */

const (
	// depositRegistryPollInterval 链上充值地址登记的轮询间隔。
	depositRegistryPollInterval = 10 * time.Second
	// depositRegistryQueryCount 单页查询条数（与 abandon/pending 查询同一量级）。
	depositRegistryQueryCount = 128
	// depositRegistryHeightScanWindow 单次查询覆盖的高度窗口。链上实现每次最多遍历 1000 个高度，
	// 这里取更小的窗口，不依赖那个上限，也避免"窗口内没有登记 ⇒ 返回空"被误判为"已经扫完"。
	depositRegistryHeightScanWindow = 500
	// depositRegistryMaxWindowsPerPoll 单轮最多扫多少个高度窗口：游标丢失时从头追赶，
	// 不让一次 poll 长时间占住。
	depositRegistryMaxWindowsPerPoll = 200
)

var (
	// depositRegistryCursorBucket 保存登记扫描游标（height:index），重启后从断点继续。
	depositRegistryCursorBucket = []byte("rgbx-deposit-registry-cursor")
	depositRegistryCursorKey    = []byte("cursor")
	// depositRegistryRetryBucket 保存导入失败的 userID（key = userID，value 为空），
	// 修复配置/重启后继续重试（见文件头）。
	depositRegistryRetryBucket = []byte("rgbx-deposit-registry-retry")
)

// depositRegistryProcessor 按高度区间拉取链上登记并逐条导入充值脚本。
//
// query 与 ensure 都可注入（单测不依赖链/钱包）；retrying 是本进程内待重试的 userID。
type depositRegistryProcessor struct {
	query  func(startHeight, startIndex, endHeight int64) (*rtypes.DepositScriptList, error)
	ensure func(userID string) error
	// markFailed / markDone 持久化失败集合；nil 表示只保留在进程内（单测用）。
	markFailed func(userID string) error
	markDone   func(userID string) error

	nextHeight int64
	nextIndex  int64
	// retrying 导入失败的 userID，按发现顺序重试（数量小，线性去重即可）。
	retrying []string
}

// seedRetries 装载重启后仍需重试的 userID（读库结果）。
func (p *depositRegistryProcessor) seedRetries(userIDs []string) {
	for _, userID := range userIDs {
		p.addRetry(userID)
	}
}

// addRetry 登记一条待重试的 userID（去重）。
func (p *depositRegistryProcessor) addRetry(userID string) {
	if userID == "" {
		return
	}
	for _, u := range p.retrying {
		if u == userID {
			return
		}
	}
	p.retrying = append(p.retrying, userID)
}

// handleEntry 导入一条登记。失败不影响其余条目：记 ERROR/告警、进重试集合，扫描继续。
func (p *depositRegistryProcessor) handleEntry(userID string) {
	if err := p.ensure(userID); err != nil {
		// 告警级：watch 集已满/地址非法/群公钥未就绪都会让该用户的充值对桥不可见，
		// 这是"钱打进来却没人认"的形态，不能只留一条 Debug 或被静默跳过。
		log.Error("ALERT deposit registry entry cannot be imported: deposits to this user's BTC address "+
			"stay invisible to the bridge until it is fixed (raise neutrino.maxWatchedDepositScripts, "+
			"fix the registered userID, or restore the TSS group key)",
			"userID", userID, "err", err)
		if userID != "" {
			p.addRetry(userID)
			if p.markFailed != nil {
				if perr := p.markFailed(userID); perr != nil {
					log.Error("deposit registry: persist retry entry failed", "userID", userID, "err", perr)
				}
			}
		}
		return
	}
}

// retryPending 重试上一轮失败的登记；成功即移出集合。
func (p *depositRegistryProcessor) retryPending() {
	if len(p.retrying) == 0 {
		return
	}
	kept := p.retrying[:0]
	for _, userID := range p.retrying {
		if err := p.ensure(userID); err != nil {
			log.Error("ALERT deposit registry retry still failing: deposits to this user's BTC address "+
				"stay invisible to the bridge", "userID", userID, "err", err)
			kept = append(kept, userID)
			continue
		}
		log.Info("deposit registry retry succeeded", "userID", userID)
		if p.markDone != nil {
			if err := p.markDone(userID); err != nil {
				log.Error("deposit registry: clear retry entry failed", "userID", userID, "err", err)
			}
		}
	}
	p.retrying = kept
}

// pollOnce 从当前游标扫到 endHeight（每轮最多 depositRegistryMaxWindowsPerPoll 个高度窗口）。
// 查询失败不推进游标，下个周期重试。
func (p *depositRegistryProcessor) pollOnce(endHeight int64) {
	for windows := 0; windows < depositRegistryMaxWindowsPerPoll && p.nextHeight <= endHeight; windows++ {
		windowEnd := p.nextHeight + depositRegistryHeightScanWindow - 1
		if windowEnd > endHeight {
			windowEnd = endHeight
		}
		list, err := p.query(p.nextHeight, p.nextIndex, windowEnd)
		if err != nil {
			log.Warn("depositRegistry list registrations failed, retry next round",
				"startHeight", p.nextHeight, "startIndex", p.nextIndex, "endHeight", windowEnd, "err", err)
			return
		}
		items := list.GetItems()
		for _, item := range items {
			p.handleEntry(strings.TrimSpace(item.GetUserID()))
		}
		// A full page may end in the middle of the window: continue from the last item.
		if len(items) >= depositRegistryQueryCount {
			last := items[len(items)-1]
			if last.GetHeight() >= p.nextHeight {
				p.nextHeight = last.GetHeight()
				p.nextIndex = int64(last.GetIndex()) + 1
				continue
			}
		}
		// Otherwise the whole window was scanned (empty windows included): move past it.
		p.nextHeight = windowEnd + 1
		p.nextIndex = 0
	}
}

// loadDepositRegistryCursor 读登记扫描断点。缺失/损坏一律从头开始（重复处理是幂等的）。
func (n *neutrinoClient) loadDepositRegistryCursor() (int64, int64) {
	if n.neutrinoCfg.Database == nil {
		return 0, 0
	}
	var height, index int64
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(depositRegistryCursorBucket)
		if bucket == nil {
			return nil
		}
		value := bucket.Get(depositRegistryCursorKey)
		if value == nil {
			return nil
		}
		_, err := fmt.Sscanf(string(value), "%d:%d", &height, &index)
		return err
	})
	if err != nil {
		log.Warn("depositRegistry read cursor failed, restarting from height 0", "err", err)
		return 0, 0
	}
	if height < 0 {
		height = 0
	}
	if index < 0 {
		index = 0
	}
	return height, index
}

// saveDepositRegistryCursor 记录登记扫描断点（只在查询成功后推进）。
func (n *neutrinoClient) saveDepositRegistryCursor(height, index int64) {
	if n.neutrinoCfg.Database == nil {
		return
	}
	err := walletdb.Update(n.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(depositRegistryCursorBucket)
		if err != nil {
			return err
		}
		return bucket.Put(depositRegistryCursorKey, []byte(fmt.Sprintf("%d:%d", height, index)))
	})
	if err != nil {
		log.Error("depositRegistry save cursor failed", "height", height, "index", index, "err", err)
	}
}

// loadDepositRegistryRetries 读回导入失败的 userID（重启后继续重试）。
func (n *neutrinoClient) loadDepositRegistryRetries() []string {
	if n.neutrinoCfg.Database == nil {
		return nil
	}
	var userIDs []string
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(depositRegistryRetryBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, _ []byte) error {
			userIDs = append(userIDs, string(k))
			return nil
		})
	})
	if err != nil {
		log.Error("depositRegistry load retry entries failed", "err", err)
		return nil
	}
	return userIDs
}

// markDepositRegistryRetry 记录/清除一条导入失败的 userID。
func (n *neutrinoClient) markDepositRegistryRetry(userID string, failed bool) error {
	if n.neutrinoCfg.Database == nil {
		return nil
	}
	return walletdb.Update(n.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(depositRegistryRetryBucket)
		if err != nil {
			return err
		}
		if failed {
			return bucket.Put([]byte(userID), nil)
		}
		return bucket.Delete([]byte(userID))
	})
}

// listDepositRegistrations 按高度区间查询链上已登记的充值地址。
//
// 用字符串函数名调用：链上查询（并行实现中）可能暂时不存在，但桥侧不应因此编译不过；
// 查询失败会在下个周期重试。
func (n *neutrinoClient) listDepositRegistrations(startHeight, startIndex, endHeight int64) (*rtypes.DepositScriptList, error) {
	req := &rtypes.ReqListPendingTx{
		StartHeight: startHeight,
		StartIndex:  startIndex,
		EndHeight:   endHeight,
		Count:       depositRegistryQueryCount,
	}
	reply, err := n.mainChainGrpc.QueryChain(n.ctx, &types.ChainExecutor{
		Driver:   rtypes.RgbxX,
		FuncName: "ListDepositRegistrations",
		Param:    types.Encode(req),
	})
	if err != nil {
		return nil, err
	}
	if !reply.GetIsOk() {
		return nil, fmt.Errorf("ListDepositRegistrations not ok: %s", string(reply.GetMsg()))
	}
	list := &rtypes.DepositScriptList{}
	if err := types.Decode(reply.GetMsg(), list); err != nil {
		return nil, err
	}
	return list, nil
}

// depositRegistryWorker 运行在官方节点（持有钱包、真正 watch 充值脚本的那个节点）上：
// 每轮先把上一轮失败的重试一遍，再从持久游标增量拉取新登记并导入。
func (n *neutrinoClient) depositRegistryWorker() {
	if n == nil || n.bw == nil {
		return
	}
	p := &depositRegistryProcessor{
		query: n.listDepositRegistrations,
		ensure: func(userID string) error {
			_, err := n.bw.ensureUserDepositScript(userID)
			return err
		},
		markFailed: func(userID string) error { return n.markDepositRegistryRetry(userID, true) },
		markDone:   func(userID string) error { return n.markDepositRegistryRetry(userID, false) },
	}
	p.nextHeight, p.nextIndex = n.loadDepositRegistryCursor()
	p.seedRetries(n.loadDepositRegistryRetries())
	log.Info("depositRegistryWorker start", "startHeight", p.nextHeight, "startIndex", p.nextIndex,
		"pendingRetries", len(p.retrying))

	ticker := time.NewTicker(depositRegistryPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			// 先重试旧的失败项：本轮新失败的条目留到下个周期，避免同一轮重复报错。
			p.retryPending()
			endHeight := n.getMainChainHeight()
			if endHeight <= 0 {
				continue
			}
			prevHeight, prevIndex := p.nextHeight, p.nextIndex
			p.pollOnce(endHeight)
			if p.nextHeight != prevHeight || p.nextIndex != prevIndex {
				n.saveDepositRegistryCursor(p.nextHeight, p.nextIndex)
			}
		}
	}
}
