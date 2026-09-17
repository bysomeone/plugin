package neutrino

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/33cn/chain33/common/address"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
)

/*
 * 每用户 P2WSH BTC 充值地址（C2）。
 *
 * 派生规格是冻结的、纯函数：地址 = f(userID, tssPub)，userID 就是 chain33 充值地址串，
 * 链上执行器只凭 (depositAddress, CrossChainInfo.Pubkey) 重建同一个 program 来认定归属
 * （见 plugin/dapp/rgbx/types/p2wsh_deposit.go 与 executor/validate_proof.go）。
 * 因此**桥侧不需要任何"地址注册表"**：谁要地址就现算一个，算出来的永远是同一个。
 *
 * 桥唯一需要**记住**的东西是 watch 集（program ↔ userID）：钱包只有订阅了该脚本才能"看见"
 * 打进来的充值（analyzeTransaction 靠 program 反解归属），而 waddrmgr 对 watching-only 钱包
 * **不保存 witnessScript 本身**（见 waddrmgr.ImportWitnessScript 的注释），所以映射只能自己留一份。
 * 这份映射**不是权威**（权威是链上派生判定），丢了只会漏认充值、不会认错账，重建也只需用户再要一次地址。
 *
 * watch 集随用户数增长，代价与界（C0 §6④）：
 *   - 每 import 一个脚本 = 往 neutrino 的 watch 列表里加一个 34 字节 program（内存 + 每块 filter 匹配
 *     的常数项），**不增加任何逐地址的 RPC**（Go 侧走 neutrino 的本地 filter 匹配，不是侧车那种逐脚本
 *     searchrawtransactions）；
 *   - 真正的大头是**首次 import 触发的那次 rescan**：rescan 未在跑时 NotifyReceived 会从钱包生日起
 *     回扫一遍（在跑时只是 AddAddrs，不重扫）。所以"按需发放"的隐含成本 = 每个新用户一次 rescan；
 *     N 个用户分散发放 = N 次 rescan，批量发放（或先发地址后集中 import）能把它们并进一次。
 *   - 界：maxWatchedDepositScripts（配置键）是显式上限，超限**拒绝发新地址**（明确失败优于
 *     静默变慢/静默漏认）。默认 1e4；实测口径见 C2 汇报（本仓无 E2E 数据，量级判断：1e4 个 program
 *     的 watch 列表对 neutrino 的 filter 匹配是常数级开销，瓶颈在 rescan 次数而非集合大小）。
 */

const (
	// defaultMaxWatchedDepositScripts watch 集默认上限（配置键未设或为 0 时生效）。
	defaultMaxWatchedDepositScripts = 10000
	// depositScriptBucket 用户充值脚本 watch 集的持久化 bucket（neutrino.db）：
	// key = userID（chain33 地址串原始字节），value = pkScript（34 字节）。
	depositScriptBucket = "rgbx-deposit-scripts"
	// waddrmgrNamespaceKey 与 btcwallet wallet 包内同名常量一致（那边是私有的），
	// 导入脚本必须写进同一个命名空间。
	waddrmgrNamespaceKey = "waddrmgr"
	// depositScriptSyncTimeout 一次 watch 集下发的超时。1e4 条的集合也就百 KB 量级，给足余量。
	depositScriptSyncTimeout = 60 * time.Second
	// depositScriptSyncInterval 跨节点补齐的轮询间隔（见 syncDepositScriptsWithSidecar）。
	// 收敛延迟的期望值约为该间隔的一半；扫集/提现本身按需重试，所以这里不需要更密。
	depositScriptSyncInterval = 30 * time.Second
)

// depositScriptSet 用户充值脚本的 watch 集：program(pkScript) ↔ userID 双向索引。
// 读多写少（每条钱包通知读、发放地址时写），自持锁。
type depositScriptSet struct {
	mu        sync.RWMutex
	byProgram map[string]string // pkScript hex → userID
	byUser    map[string]string // userID → pkScript hex
}

func newDepositScriptSet() *depositScriptSet {
	return &depositScriptSet{
		byProgram: make(map[string]string),
		byUser:    make(map[string]string),
	}
}

// lookupUser 按输出脚本反解充值归属：返回该 program 对应的 userID（= chain33 充值地址串）。
// **这是充值归因的唯一判据**（不再依赖 OP_RETURN）：能反解出 userID，说明这笔输出付给了
// 按 (userID, tssPub) 派生出的充值脚本，链上执行器会用同一份派生认定同一笔归属。
func (s *depositScriptSet) lookupUser(pkScript []byte) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	userID, ok := s.byProgram[hex.EncodeToString(pkScript)]
	return userID, ok
}

// watched 该 userID 是否已经在 watch 集里。
func (s *depositScriptSet) watched(userID string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.byUser[userID]
	return ok
}

// size watch 集当前条目数。
func (s *depositScriptSet) size() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byUser)
}

// add 记入 watch 集（幂等）。
func (s *depositScriptSet) add(userID string, pkScript []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byProgram == nil {
		s.byProgram = make(map[string]string)
	}
	if s.byUser == nil {
		s.byUser = make(map[string]string)
	}
	program := hex.EncodeToString(pkScript)
	s.byProgram[program] = userID
	s.byUser[userID] = program
}

// entries 返回 watch 集的全部条目（userID ↔ pkScript），供下发/对账用。
func (s *depositScriptSet) entries() []depositScriptEntry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]depositScriptEntry, 0, len(s.byUser))
	for userID, program := range s.byUser {
		pkScript, err := hex.DecodeString(program)
		if err != nil {
			continue
		}
		out = append(out, depositScriptEntry{userID: userID, pkScript: pkScript})
	}
	return out
}

// depositScriptEntry watch 集的一条（userID + 该用户的 34 字节 P2WSH program）。
type depositScriptEntry struct {
	userID   string
	pkScript []byte
}

// depositTssPubKey 本地 DKG 群公钥（33 字节压缩）。取不到（DKG 还没完成）返回 nil。
//
// 直接读 tss 服务而不是缓存到 wallet：tssPublicKey 只在 DKG 完成时写一次，且写在
// dkgCompleted 置位之前，而发放地址/监听交易都发生在 dkgCompleted 之后（client.Start 里
// waitDKGCompleted → bw.start → HTTP/通知循环）。
func (n *neutrinoClient) depositTssPubKey() []byte {
	if n == nil || n.tss == nil || n.tss.tssPublicKey == nil {
		return nil
	}
	return n.tss.tssPublicKey.SerializeCompressed()
}

// maxWatchedDepositScripts watch 集上限（配置未设则用默认值）。
func (n *neutrinoClient) maxWatchedDepositScripts() int {
	if n == nil || n.cfg.MaxWatchedDepositScripts <= 0 {
		return defaultMaxWatchedDepositScripts
	}
	return n.cfg.MaxWatchedDepositScripts
}

// ensureUserDepositScript 按需把一个用户的 P2WSH 充值脚本纳入 watch 集，返回该用户的 BTC 充值地址。
//
// 幂等：同一 userID 重复请求只 import 一次，返回同一个地址（派生是纯函数）。
// 失败一律返回 error（不返回"尽力而为"的地址）：地址发出去就意味着用户会往那儿打钱。
func (b *btcWallet) ensureUserDepositScript(userID string) (string, error) {
	if err := validateDepositUserID(userID); err != nil {
		return "", err
	}
	pub := b.client.depositTssPubKey()
	if len(pub) == 0 {
		return "", fmt.Errorf("local tss group pubkey is not ready (dkg not completed)")
	}
	// 先算地址：派生失败（userID 超长 / pubkey 非法）就没有必要动 watch 集。
	addr, err := rtypes.DeriveDepositAddress(userID, pub, &b.chainParams)
	if err != nil {
		return "", fmt.Errorf("derive deposit address: %w", err)
	}
	witnessScript, err := rtypes.DeriveDepositWitnessScript(userID, pub)
	if err != nil {
		return "", fmt.Errorf("derive deposit witness script: %w", err)
	}
	pkScript, err := rtypes.DeriveDepositPkScript(userID, pub)
	if err != nil {
		return "", fmt.Errorf("derive deposit pk script: %w", err)
	}
	if b.client.deposits.watched(userID) {
		return addr, nil
	}
	if size, limit := b.client.deposits.size(), b.client.maxWatchedDepositScripts(); size >= limit {
		return "", fmt.Errorf("deposit watch set is full (%d >= maxWatchedDepositScripts %d), "+
			"refusing to hand out a new deposit address: a script the wallet does not watch would make the "+
			"user's BTC invisible to the bridge", size, limit)
	}
	if _, err := b.importDepositWitnessScript(witnessScript); err != nil {
		return "", err
	}
	// 先落盘再入内存：import 已经发生（钱包确实在 watch 了），映射丢了只会漏认，不会认错。
	if err := b.client.saveDepositScript(userID, pkScript); err != nil {
		log.Error("ensureUserDepositScript persist watch entry failed",
			"userID", userID, "address", addr, "err", err)
	}
	b.client.deposits.add(userID, pkScript)
	log.Info("ensureUserDepositScript watch set grown", "userID", userID, "address", addr,
		"watchSetSize", b.client.deposits.size(), "limit", b.client.maxWatchedDepositScripts())
	// 立刻下发（不等下一轮轮询）：地址已经发给用户了，签名节点/侧车越早拿到登记，
	// 那笔充值上账后能立刻被花掉。
	b.client.syncDepositScriptsWithSidecar()
	return addr, nil
}

// validateDepositUserID 校验充值 userID：必须是 chain33 地址串（它同时是 P2WSH 派生里的 userID）。
//
// 与链上 checkDeposit 同一口径：拒绝 UTXO 形态（<btc txid>:<idx>，旧 fromUtxo 承诺路径的遗留），
// 并要求 address.CheckAddress 通过 —— 链上会用这个串去派生脚本，串写错了钱就落到没人认领的脚本里。
func validateDepositUserID(userID string) error {
	if userID == "" {
		return fmt.Errorf("empty deposit user id")
	}
	if rtypes.IsUtxoAddress(userID) {
		return fmt.Errorf("deposit user id must be a chain33 address, not a utxo form (%s)", userID)
	}
	if address.CheckAddress(userID, -1) != nil {
		return fmt.Errorf("invalid chain33 address: %s", userID)
	}
	return nil
}

// importDepositWitnessScript 把用户充值脚本导入钱包地址管理器（watching-only 的 imported account），
// 并让链客户端订阅它 —— 这样钱包才能看见打进来的充值。返回该脚本对应的 P2WSH 地址。
//
// 上游 btcwallet 在 Wallet 层**没有** ImportScript/ImportWitnessScript 包装（只有 waddrmgr 层有，
// 见 C0 §7.2），这里照 Wallet.ImportTaprootScript 的写法自己写一个：写 waddrmgr 命名空间 +
// chainClient.NotifyReceived。
//
// bs 取"钱包当前已同步到的块"：它只影响地址管理器的 start block，而**不允许把这个块往回拉**
// （拉回去等于让钱包从更早的块开始重扫，主网代价极大）。历史充值由 NotifyReceived 触发的
// neutrino rescan（从钱包生日起）覆盖，不依赖这里。
//
// 幂等：重复导入（ErrDuplicateAddress）视为成功，订阅与记账照走。
func (b *btcWallet) importDepositWitnessScript(witnessScript []byte) (btcutil.Address, error) {
	addrs, err := b.importDepositWitnessScripts([][]byte{witnessScript})
	if err != nil {
		return nil, err
	}
	return addrs[0], nil
}

// importDepositWitnessScripts 批量导入用户充值脚本，并**只订阅一次**（NotifyReceived 一次一批）。
//
// 为什么批量：rescan 未在跑时每次 NotifyReceived 都会触发一次回扫，N 个脚本分 N 次调用就是 N 次
// 回扫（C2 注释里的"按需发放的隐含成本 = 每个新用户一次 rescan"）。跨节点补齐一次可能带回来成
// 百上千条（见 syncDepositScriptsWithSidecar），逐条调用会把代价放大 N 倍。
func (b *btcWallet) importDepositWitnessScripts(witnessScripts [][]byte) ([]btcutil.Address, error) {
	if len(witnessScripts) == 0 {
		return nil, nil
	}
	manager, err := b.ensureScopedKeyManager(waddrmgr.KeyScopeBIP0084)
	if err != nil {
		return nil, fmt.Errorf("fetch bip84 scoped key manager: %w", err)
	}
	bs := b.Wallet.Manager.SyncedTo()
	addrs := make([]btcutil.Address, 0, len(witnessScripts))
	err = walletdb.Update(b.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket([]byte(waddrmgrNamespaceKey))
		if ns == nil {
			return fmt.Errorf("waddrmgr namespace not found in wallet db")
		}
		for _, witnessScript := range witnessScripts {
			// witnessVersion=0（v0 P2WSH）、isSecretScript=false：watch-only 钱包只能以非机密脚本
			// 导入（脚本用公开密钥加密存储；watching-only 下 waddrmgr 不保留 witnessScript 原文，
			// 需要时由 (userID, tssPub) 重新派生，见本文件开头）。
			managed, ierr := manager.ImportWitnessScript(ns, witnessScript, &bs, 0, false)
			switch {
			case ierr == nil:
				addrs = append(addrs, managed.Address())
			case waddrmgr.IsError(ierr, waddrmgr.ErrDuplicateAddress):
				// 已导入过：重新算出地址（P2WSH 地址 = bech32(sha256(witnessScript))，与派生同源）。
				derived, derr := btcutil.NewAddressWitnessScriptHash(sha256Sum(witnessScript), &b.chainParams)
				if derr != nil {
					return fmt.Errorf("rebuild deposit address after duplicate import: %w", derr)
				}
				addrs = append(addrs, derived)
			default:
				return ierr
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("import deposit witness scripts: %w", err)
	}
	if err := b.notifyAddrs(addrs); err != nil {
		return nil, fmt.Errorf("subscribe %d deposit addresses: %w", len(addrs), err)
	}
	return addrs, nil
}

// notifyAddrs 通知链客户端订阅地址（默认走 chainClient.NotifyReceived，测试可注入）。
func (b *btcWallet) notifyAddrs(addrs []btcutil.Address) error {
	if b.notifyFn != nil {
		return b.notifyFn(addrs)
	}
	if b.chainClient == nil {
		return fmt.Errorf("chain client not started")
	}
	return b.chainClient.NotifyReceived(addrs)
}

// loadDepositScripts 从 neutrino.db 载入 watch 集，并**按当前群公钥重新派生**每条记录的 program：
// 对不上（旧世代 DKG 的残留 / 记录损坏）就跳过并告警 —— 那种脚本上的 BTC 已经不可花费
// （群公钥一变所有地址作废，见 C0 §1.6），继续 watch 只会把不可认领的充值误认成充值。
//
// 载入时**顺带重新导入**钱包地址管理器（幂等）：脚本的权威存放处是 btcwallet.db，而 watch 集记在
// neutrino.db —— 两个库可能不同步（钱包恢复/重建），重新导入让"记在集合里的脚本一定真的被 watch"
// 这条不变式自愈，代价只是启动时一次 AddAddrs（rescan 已在跑时）或一次 rescan。
func (n *neutrinoClient) loadDepositScripts() {
	pub := n.depositTssPubKey()
	if len(pub) == 0 || n.deposits == nil || n.neutrinoCfg.Database == nil {
		return
	}
	type entry struct{ userID, program string }
	var entries []entry
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket([]byte(depositScriptBucket))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			entries = append(entries, entry{userID: string(k), program: hex.EncodeToString(v)})
			return nil
		})
	})
	if err != nil {
		log.Error("loadDepositScripts read bucket", "err", err)
		return
	}
	skipped := 0
	// 本轮新载入的脚本（钱包不在 watch 的节点只维护登记，不导入）。
	var loaded [][]byte
	var adds []depositScriptEntry
	for _, e := range entries {
		pkScript, derr := rtypes.DeriveDepositPkScript(e.userID, pub)
		if derr != nil || hex.EncodeToString(pkScript) != e.program {
			skipped++
			log.Warn("loadDepositScripts stale watch entry (derivation mismatch), skip",
				"userID", e.userID, "storedProgram", e.program, "err", derr)
			continue
		}
		// 只有真的在 watch 的节点才需要把脚本导入钱包：非官方节点不跑交易监听，导入了也看不见
		// 东西（而且它的链客户端没启动，NotifyReceived 会失败）。它只需要"知道这个脚本是充值
		// 脚本"来给签名做归属核对。
		if n.depositImporter != nil {
			witnessScript, werr := rtypes.DeriveDepositWitnessScript(e.userID, pub)
			if werr != nil {
				skipped++
				log.Error("loadDepositScripts derive witness script failed, skip", "userID", e.userID, "err", werr)
				continue
			}
			loaded = append(loaded, witnessScript)
		}
		adds = append(adds, depositScriptEntry{userID: e.userID, pkScript: pkScript})
	}
	// 一次导入 + 一次订阅（见 importDepositWitnessScripts 的注释：逐条调用 = 逐条 rescan）。
	if len(loaded) > 0 {
		if err := n.depositImporter(loaded); err != nil {
			log.Error("loadDepositScripts re-import failed, keep the watch set unchanged",
				"count", len(loaded), "err", err)
			return
		}
	}
	for _, a := range adds {
		n.deposits.add(a.userID, a.pkScript)
	}
	log.Info("loadDepositScripts done", "loaded", n.deposits.size(), "skipped", skipped,
		"walletWatching", n.depositImporter != nil)
	// 启动即下发一次：本节点可能不是发放地址的那个（重启前在别处发过），下发顺带把侧车持有的
	// 并集拉回来（见 syncDepositScriptsWithSidecar）。
	n.syncDepositScriptsWithSidecar()
}

// sidecarClient 侧车客户端（未配置/未连接返回 nil）。
func (n *neutrinoClient) sidecarClient() *rgb20.Sidecar {
	if n == nil || n.rgb20 == nil || !n.rgb20.IsConnected() {
		return nil
	}
	return n.rgb20.Sidecar()
}

// syncDepositScriptsWithSidecar 把本地 watch 集下发给（本地节点连的）侧车，并把侧车返回的
// **并集**并入本地。幂等、可重入：两侧都按 userID/program 去重。
//
// 失败只告警不返回错误：这一步是"让别处也知道"的传播，不是本节点发放地址的前置条件 —— 但**必须
// 解释后果**（用户充值会花不掉），所以日志要写到能排障的程度。
func (n *neutrinoClient) syncDepositScriptsWithSidecar() {
	sc := n.sidecarClient()
	if sc == nil {
		return
	}
	local := n.deposits.entries()
	req := &pb.RegisterDepositScriptsRequest{Scripts: make([]*pb.DepositScriptEntry, 0, len(local))}
	for _, e := range local {
		req.Scripts = append(req.Scripts, &pb.DepositScriptEntry{UserId: e.userID, PkScript: e.pkScript})
	}
	ctx, cancel := context.WithTimeout(context.Background(), depositScriptSyncTimeout)
	defer cancel()
	resp, err := sc.RegisterDepositScripts(ctx, req)
	if err != nil {
		log.Warn("syncDepositScriptsWithSidecar register failed, retry next round",
			"pushed", len(req.Scripts), "err", err)
		return
	}
	n.mergeRemoteDepositScripts(resp.GetScripts())
}

// depositScriptSyncWorker 周期性地下发/补齐 watch 集（所有节点都跑，见 syncDepositScriptsWithSidecar）。
// DKG 未完成时没有群公钥可派生，跳过本轮（等 TSS 就绪）。
func (n *neutrinoClient) depositScriptSyncWorker() {
	if n == nil || n.deposits == nil {
		return
	}
	ticker := time.NewTicker(depositScriptSyncInterval)
	defer ticker.Stop()
	loaded := false
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			// DKG 未完成时没有群公钥可派生，跳过本轮（等 TSS 就绪）。
			if len(n.depositTssPubKey()) == 0 {
				continue
			}
			// 官方节点在钱包起来时已经载入过（见 waitAndImportTSSAddress）；其余节点在这里补一次
			// （载入是幂等的，且它顺带把本地 neutrino.db 里的登记恢复出来）。
			if !loaded {
				n.loadDepositScripts()
				loaded = true
			}
			n.syncDepositScriptsWithSidecar()
		}
	}
}

// mergeRemoteDepositScripts 把侧车回带的条目（可能来自别的节点）并进本地 watch 集。
//
// 每一条都**在本地重新派生一遍**再比对（不因为"侧车说它登记过"就收下）：判据始终是
// (userID, 本节点的 tssPub) → program，与链上执行器的判据同源。对不上就跳过并告警 ——
// 那说明本节点与对方/侧车的群公钥不是同一把（换过钥，或配置错），收下反而会认错账。
func (n *neutrinoClient) mergeRemoteDepositScripts(entries []*pb.DepositScriptEntry) {
	pub := n.depositTssPubKey()
	if len(pub) == 0 || n.deposits == nil {
		return
	}
	var witnessScripts [][]byte
	var adds []depositScriptEntry
	skipped, full := 0, false
	for _, e := range entries {
		if e.GetUserId() == "" || len(e.GetPkScript()) == 0 {
			skipped++
			continue
		}
		if n.deposits.watched(e.GetUserId()) {
			continue
		}
		if err := validateDepositUserID(e.GetUserId()); err != nil {
			skipped++
			log.Warn("mergeRemoteDepositScripts invalid user id, skip", "userID", e.GetUserId(), "err", err)
			continue
		}
		pkScript, err := rtypes.DeriveDepositPkScript(e.GetUserId(), pub)
		if err != nil || !bytes.Equal(pkScript, e.GetPkScript()) {
			skipped++
			log.Warn("mergeRemoteDepositScripts derivation mismatch, skip", "userID", e.GetUserId(),
				"remotePkScript", hex.EncodeToString(e.GetPkScript()), "err", err)
			continue
		}
		if size, limit := n.deposits.size(), n.maxWatchedDepositScripts(); size >= limit {
			full = true
			break
		}
		witnessScript, err := rtypes.DeriveDepositWitnessScript(e.GetUserId(), pub)
		if err != nil {
			skipped++
			log.Warn("mergeRemoteDepositScripts derive witness script, skip", "userID", e.GetUserId(), "err", err)
			continue
		}
		witnessScripts = append(witnessScripts, witnessScript)
		adds = append(adds, depositScriptEntry{userID: e.GetUserId(), pkScript: pkScript})
	}
	if full {
		log.Error("mergeRemoteDepositScripts watch set is full, stopping the merge "+
			"(raise neutrino.maxWatchedDepositScripts, or the rest of the registry stays unknown here)",
			"size", n.deposits.size(), "limit", n.maxWatchedDepositScripts())
	}
	if len(adds) == 0 {
		return
	}
	// 只有真的在 watch 的节点才需要把脚本导入钱包：非官方节点不跑交易监听，导入了也看不见东西，
	// 而且它的链客户端没启动（NotifyReceived 会失败）。它只需要"知道这个脚本是充值脚本"来给
	// 签名做归属核对。
	if n.depositImporter != nil {
		if err := n.depositImporter(witnessScripts); err != nil {
			log.Error("mergeRemoteDepositScripts import into wallet failed, keep the watch set unchanged",
				"count", len(witnessScripts), "err", err)
			return
		}
	}
	for _, a := range adds {
		if err := n.saveDepositScript(a.userID, a.pkScript); err != nil {
			log.Error("mergeRemoteDepositScripts persist watch entry failed", "userID", a.userID, "err", err)
		}
		n.deposits.add(a.userID, a.pkScript)
	}
	log.Info("mergeRemoteDepositScripts merged remote deposit scripts", "merged", len(adds),
		"watchSetSize", n.deposits.size(), "skipped", skipped, "walletWatching", n.depositImporter != nil)
}

// saveDepositScript 持久化一条 watch 记录（重启后不必等用户再要一次地址）。
func (n *neutrinoClient) saveDepositScript(userID string, pkScript []byte) error {
	if n == nil || n.neutrinoCfg.Database == nil {
		return nil
	}
	return walletdb.Update(n.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket([]byte(depositScriptBucket))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(userID), pkScript)
	})
}

// isWatchedDepositScript 目标脚本是不是已发放的用户充值脚本（提现/扫集的输入归属核对用）。
func (n *neutrinoClient) isWatchedDepositScript(pkScript []byte) (string, bool) {
	return n.deposits.lookupUser(pkScript)
}

// mustPkScript 仅供日志/派生用：把地址转成 pkScript；失败返回 nil（调用方只用于日志）。
func mustPkScript(addr btcutil.Address) []byte {
	if addr == nil {
		return nil
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil
	}
	return script
}

// sha256Sum 返回 sha256(witnessScript)，即 P2WSH 的 program。
func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
