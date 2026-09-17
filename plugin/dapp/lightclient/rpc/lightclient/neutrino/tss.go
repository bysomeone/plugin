package neutrino

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/33cn/chain33/system/crypto/tss"
	"github.com/33cn/chain33/system/crypto/tss/gg18"
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
	"google.golang.org/protobuf/proto"
)

const (
	moduleName = "dapp-lightclient-neutrino"
	// TSS pubsub topic - notification only
	tssSignNotifyTopic = "rgbx/tssSignNotify/1.0"

	// transactionTypeRgb20Withdraw RGB20 提现 PSBT 的签名通知类型：Payload = 提现上下文
	// （rgb20.WithdrawSignPayload JSON）、Psbt + Consignment。与 BTC 提现的
	// transactionTypeWithdraw（Payload = chain33 提现哈希、无 PSBT）分开是刻意的：
	// 通知类型决定签名节点跑哪一套校验，靠"PSBT 是否为空"之类的旁证来分派正是 E11
	// （空 Payload 旁路成了生产路径）的成因。
	transactionTypeRgb20Withdraw = "rgb20-withdraw"
	// transactionTypeTestSign 只给 E2E 的 sign-psbt 测试端点（见 signPsbtTestOnly）：不带任何
	// 提现上下文、签名节点不做提现核对。必须由本地配置 rgb20.testSignPsbt 显式打开，
	// 生产提现路径永远发不出这个类型。
	transactionTypeTestSign = "test-sign"
	// transactionTypeRgb20WithdrawReject 签名节点拒签回执（确定性拒签时回传协调者）。
	transactionTypeRgb20WithdrawReject = "rgb20-withdraw-reject"

	// Database bucket and keys
	tssBucketName  = "rgbx-tss"
	dkgResultKey   = "dkg-result"
	dkgSessionName = "rgbx-btc-dkg"
)

type signTask struct {
	idx         int
	sigHash     []byte
	sessionName string
	signers     []string
	result      chan *signResult
}

type signResult struct {
	sig []byte
	err error
}

type tssService struct {
	client *neutrinoClient
	cfg    tssConfig

	// TSS related
	dkgResult    *tss.DKGResult
	tssPublicKey *btcec.PublicKey
	tssAddress   btcutil.Address
	pkScript     []byte
	dkgCompleted atomic.Bool

	// P2P channels
	subChan    chan *types.TopicData
	selfPeerId string
	signTaskCh chan *signTask

	// signMu 保护下面两张表（协调者侧的签名轮次状态）。
	signMu sync.Mutex
	// signRoundSigners 本节点作为协调者时，每笔 RGB20 提现（chain33 哈希 hex）所选定的签名
	// 节点集合。拒签回执是 P2P 广播、任何人都能发，用它核对回执来源确实是本轮签名节点。
	signRoundSigners map[string][]string
	// signRejections 其它签名节点对本节点发起的提现签名轮次的**确定性拒签**
	// （chain33 哈希 hex → 拒签回执）。拒签是终态（重试不会变好），因此记录保留到进程结束：
	// 后续重试据此立刻短路并归类为不可恢复，不再等 GG18 超时、也不再每秒重试刷屏。
	signRejections map[string]rgb20.WithdrawSignReject
}

func newTssService(n *neutrinoClient) *tssService {
	t := &tssService{
		client:           n,
		subChan:          make(chan *types.TopicData, 100),
		signTaskCh:       make(chan *signTask, 1024),
		signRoundSigners: make(map[string][]string),
		signRejections:   make(map[string]rgb20.WithdrawSignReject),
	}
	t.cfg = n.cfg.Tss
	return t
}

func (t *tssService) start() {
	log.Debug("tssService start")

	// Subscribe to TSS sign notification topic
	go t.subTopic(tssSignNotifyTopic)

	// Handle subscribed messages
	go t.handleSubMsg()

	// Dedicated signing worker.
	for i := 0; i < runtime.NumCPU(); i++ {
		go t.handleSignTask()
	}

	// Ensure DKG is completed
	go t.init()
}

func (t *tssService) handleSignTask() {
	for {
		select {
		case <-t.client.ctx.Done():
			return
		case task := <-t.signTaskCh:
			task.result <- t.signMsg(task.sigHash, task.sessionName, task.signers)
		}
	}
}

func (t *tssService) init() {
	// Phase 5 DKG 修复：本地已有 DKG 结果（重启 / 数据目录保留）时**不重跑** GG18 DKG
	// —— 但载入之后**必须**继续做链上核对与提交（见 ensureDKGOnChain）。
	//
	// 为什么不能载入即 return（踩过一次）：para 首启时 nodegroup 往往还没 approve，
	// CommitDKG 必被拒，本进程会一直卡在 commitDKGToChain；环境就绪后重启，才轮到这次"载入 + 提交"。
	// 载入即 return 会跳过提交 ⇒ 链上永远没有 CrossChainInfo，而且客户端一路"看起来正常"
	// （E2E 表现为 wait_auto_dkg_commit 超时，para 侧连一行 "commitDKG submitted" 都没有）。
	//
	// DB 优先（而不是先查主链）是因为主链 grpc 的 QueryChain 在并发/时序下可能永久 hang，
	// 卡在 tss init 第一步会拖住 dkgCompleted → client.Start → rgb20.Start/serveHTTP(17000)。
	if err := t.loadDKGFromDB(); err == nil {
		log.Info("ensureDKG load from local db")
		t.ensureDKGOnChain()
		t.dkgCompleted.Store(true)
		// Phase 5 修复：loadDKGFromDB 分支也必须设置 selfPeerId，否则 handleSignNotify 的
		// isSigner 检查永远失败（selfPeerId 空），该节点不参与 GG18 签名（sign-psbt 超时）。
		t.waitSelfPeerId()
		return
	}

	// Wait for cross chain info to be available
	info := t.client.getCrossChainInfo(rtypes.BTCSymbol)
	for info == nil {
		time.Sleep(3 * time.Second)
		log.Debug("ensureDKG getCrossChainInfo wait 3 seconds...")
		info = t.client.getCrossChainInfo(rtypes.BTCSymbol)
	}

	if info.GetTssAddress() != "" {
		log.Debug("ensureDKG already exist on chain, loading from database")
		err := t.loadDKGFromDB()
		if err == nil {
			// 与上方同理：链上已有记录仍要走一遍核对/提交（幂等：pubkey 相符即刻返回）。
			t.ensureDKGOnChain()
			t.dkgCompleted.Store(true)
			t.waitSelfPeerId()
			return
		}
		// DB 中无 DKG 结果（容器重建/数据目录清空）：不 panic，继续走下方重新 DKG。
		log.Warn("ensureDKG loadDKGFromDB error, redo DKG", "err", err)
	}
	log.Info("init tssService starting new DKG process")

	// Perform DKG process with retry
	var dkgResult *tss.DKGResult
	var err error
	for {
		dkgResult, err = gg18.ProcessDKG(t.cfg.Peers, t.cfg.Threshold, t.cfg.Rank, dkgSessionName)
		if err == nil {
			break
		}
		log.Error("init tssService ProcessDKG retry", "err", err)
		time.Sleep(time.Minute)
	}

	t.dkgResult = dkgResult

	// Extract public key from DKG result (PubX, PubY coordinates)
	pubkey, err := tss.ParseBtcecPublicKey(dkgResult)
	if err != nil {
		log.Error("init tssService ParseBtcecPublicKey error", "err", err)
		return
	}
	t.tssPublicKey = pubkey

	// Generate Bitcoin address from public key
	err = t.generateTssAddress()
	if err != nil {
		log.Error("init tssService generateTssAddress error", "err", err)
		return
	}

	// Save DKG result to database with retry
	t.saveDKGToDB()

	t.ensureDKGOnChain()
	t.dkgCompleted.Store(true)
	t.waitSelfPeerId()
}

// ensureDKGOnChain 让链上每个 symbol 的 CrossChainInfo 达到"存在、且群公钥 == 本地群公钥"，阻塞到达成。
//
// 首次 DKG 之后与"从 DB 载入 DKG"之后都要走这一遍，且两处**不能只做一次**：本节点可能
// 之前提交过但被拒（nodegroup 未 approve / 钱包未解锁），重启后才轮得到成功提交。
//
// 成功判据是**链上状态**而不是"提交没报错"，载荷与核对逻辑见 tss_commit.go。
func (t *tssService) ensureDKGOnChain() {
	t.ensureDKGOnChainWith(t.client.ctx, t.client.shareCheckSymbols(),
		t.client.queryCrossChainInfoBounded, t.submitDKGToMainChain)
}

// ensureDKGOnChainWith 是 ensureDKGOnChain 的实现，symbol 集合与两个依赖显式传入（便于单测）。
//
// symbols 用 shareCheckSymbols()：BTC + 配置里的每个 RGB20 合约，去重且跳过空 symbol ——
// 与自检（checkTssShareAgainstChain）覆盖**同一集合**，两处不能各说各话。
func (t *tssService) ensureDKGOnChainWith(ctx context.Context, symbols []string,
	query func(symbol string) (*rtypes.CrossChainInfo, error),
	submit func(exec, action string, payload *rtypes.CommitDKG) (string, error)) {

	for _, symbol := range symbols {
		// 与 BTC 同一套判据（提交后核对链上 pubkey）：4 个 guardian 都会提交，先到者创建记录，
		// 其余节点拿到 ErrDuplicateDKGCommit —— 但"被拒为重复"本身不是成功的判据，链上那把钥
		// 必须逐字节等于本地群公钥，否则该 symbol 的充值/提现（RGB20 的 thresholdSig 校验）
		// 或 BTC 充值地址（P2WSH = f(userID, tssPub)）会静默失灵。
		t.commitDKGToChainWith(ctx, t.buildCommitDKGPayload(symbol), query, submit)
		log.Info("ensureDKGOnChain commitDKG", "symbol", symbol)
	}
}

// waitSelfPeerId 等待 P2P 自节点 peer id 就绪（GG18 签名时 handleSignNotify 的 isSigner 检查依赖它）。
func (t *tssService) waitSelfPeerId() {
	for {
		peers, err := tss.FetchConnectedPeers(t.client.qclient, time.Second*3)
		if err == nil && len(peers) > 0 && peers[len(peers)-1].Self {
			t.selfPeerId = peers[len(peers)-1].Name
			break
		}
		log.Debug("init tssService waitForSelfPeerId FetchConnectedPeers retry", "err", err)
		time.Sleep(time.Second * 3)
	}
}

func (t *tssService) loadDKGFromDB() error {
	var dkgData []byte

	err := walletdb.View(t.client.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket([]byte(tssBucketName))
		if bucket == nil {
			return walletdb.ErrBucketNotFound
		}

		// Load DKG result
		dkgData = bucket.Get([]byte(dkgResultKey))
		if dkgData == nil {
			return types.ErrNotFound
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Decode DKG result
	var dkgResult tss.DKGResult
	err = types.Decode(dkgData, &dkgResult)
	if err != nil {
		return err
	}
	t.dkgResult = &dkgResult

	// Extract public key from DKG result coordinates
	pubKey, err := tss.ParseBtcecPublicKey(&dkgResult)
	if err != nil {
		log.Error("loadDKGFromDB ParseBtcecPublicKey error", "err", err)
		return err
	}
	t.tssPublicKey = pubKey

	// Generate Bitcoin address
	return t.generateTssAddress()
}

// saveDKGToDB saves DKG result to database with retry until success
func (t *tssService) saveDKGToDB() {
	// Encode DKG result
	dkgData := types.Encode(t.dkgResult)

	for {
		err := walletdb.Update(t.client.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
			bucket, err := tx.CreateTopLevelBucket([]byte(tssBucketName))
			if err != nil {
				return err
			}

			// Save DKG result
			return bucket.Put([]byte(dkgResultKey), dkgData)
		})

		if err == nil {
			log.Debug("saveDKGToDB success")
			return
		}

		log.Error("saveDKGToDB retry", "err", err)
		time.Sleep(time.Second * 3)
	}
}

// generateBitcoinAddress generates Bitcoin address from TSS public key
func (t *tssService) generateTssAddress() error {
	if t.tssPublicKey == nil {
		return types.ErrInvalidParam
	}

	// Generate Bitcoin address (P2WPKH format)
	chainParams := &t.client.neutrinoCfg.ChainParams
	pubKeyHash := btcutil.Hash160(t.tssPublicKey.SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, chainParams)
	if err != nil {
		log.Error("generateTssAddress NewAddressWitnessPubKeyHash", "pubkey", hex.EncodeToString(t.tssPublicKey.SerializeCompressed()), "err", err)
		return err
	}

	t.tssAddress = addr
	t.pkScript, err = txscript.PayToAddrScript(addr)
	if err != nil {
		panic("generateTssAddress payToAddrScript error , address: " + addr.String() + " err: " + err.Error())
	}
	log.Info("generateTssAddress", "address", addr.String())

	return nil
}

func (t *tssService) waitForSufficientSigners() []string {
	var signers []string
	t.client.waitUntilDone("waitForSufficientSigners", func() bool {
		signers = tss.GetValidPeerCombination(t.client.qclient, t.cfg.Threshold, t.dkgResult.Bks)
		return len(signers) > 0
	}, time.Second*3)
	return signers
}

// processSignBtcTx processes a Bitcoin transaction using TSS protocol
// This is called by the main node to initiate TSS signing
func (t *tssService) processSignBtcTx(tx *wire.MsgTx, txType string, inputAmounts []int64, payload []byte) error {

	signers := t.waitForSufficientSigners()
	buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSizeStripped()))
	err := tx.SerializeNoWitness(buf)
	if err != nil {
		log.Error("processSignBtcTx SerializeNoWitness", "err", err)
		return err
	}
	notify := &ltypes.TssSignNotify{
		InputAmounts: inputAmounts,
		TxType:       txType,
		Payload:      payload,
		BtcTxData:    buf.Bytes(),
		Signers:      signers,
	}
	// Publish notification to all TSS nodes
	t.pubMsg(tssSignNotifyTopic, types.Encode(notify))
	log.Debug("signMsg published notification", "txType", txType, "payload", hex.EncodeToString(payload), "signers", signers)
	return t.signBtcTx(tx, inputAmounts, signers)
}

func (t *tssService) signMsg(msg []byte, sessionName string, signers []string) *signResult {
	result := &signResult{}
	sigResult, err := gg18.ProcessSign(signers, msg, t.dkgResult, sessionName)
	if err != nil {
		log.Error("signMsg ProcessSign", "err", err)
		result.err = err
		return result
	}

	signature, err := gg18.AliceToBtcecSignature(sigResult)
	if err != nil {
		log.Error("signMsg AliceToBtcecSignature", "err", err)
		result.err = err
		return result
	}
	signatureBytes := signature.Serialize()
	log.Debug("signMsg success", "signature", hex.EncodeToString(signatureBytes))
	result.sig = signatureBytes
	return result
}

func (t *tssService) signBtcTx(tx *wire.MsgTx, inputAmounts []int64, signers []string) error {
	if len(tx.TxIn) != len(inputAmounts) {
		return fmt.Errorf("input count mismatch: tx=%d inputAmounts=%d", len(tx.TxIn), len(inputAmounts))
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(make(map[wire.OutPoint]*wire.TxOut, len(tx.TxIn)))
	for idx, txIn := range tx.TxIn {
		prevOutFetcher.AddPrevOut(txIn.PreviousOutPoint, &wire.TxOut{
			Value:    inputAmounts[idx],
			PkScript: t.pkScript,
		})
	}

	txSigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	txHash := tx.TxID()
	pubKeyBytes := t.tssPublicKey.SerializeCompressed()
	sigTasks := make([]*signTask, 0, len(tx.TxIn))
	for idx := range tx.TxIn {
		// 计算签名哈希（使用预计算的脚本）
		sigHash, err := txscript.CalcWitnessSigHash(t.pkScript, txSigHashes, txscript.SigHashAll, tx, idx, inputAmounts[idx])
		if err != nil {
			return fmt.Errorf("calc sig hash failed for input %d: %w", idx, err)
		}

		sigTask := &signTask{
			idx:         idx,
			sigHash:     sigHash,
			sessionName: fmt.Sprintf("btctx-%s-%d", txHash, idx),
			signers:     signers,
			result:      make(chan *signResult, 1),
		}
		sigTasks = append(sigTasks, sigTask)
		t.signTaskCh <- sigTask
	}

	for _, sigTask := range sigTasks {
		result := <-sigTask.result
		if result.err != nil {
			return fmt.Errorf("signMsg failed for input %d: %w", sigTask.idx, result.err)
		}
		sigWithHashType := append(result.sig, byte(txscript.SigHashAll))
		tx.TxIn[sigTask.idx].Witness = wire.TxWitness{sigWithHashType, pubKeyBytes}
		log.Debug("signBtcTx applied signature to input", "idx", sigTask.idx)
	}

	return nil
}

// normalizeLowS 将高-S 签名归一化为低-S（BIP146 规范），保证广播前签名合法。
// 注意：下游 Sig.Serialize() 自己也会归一化（decred secp256k1 v4.4.0 ecdsa/signature.go），
// 此处不是 low-S 不变量的唯一保障——别因此以为它可以删，也别以为摘掉它广播仍一定安全。
func normalizeLowS(sig *ecdsa.Signature) *ecdsa.Signature {
	if sig != nil {
		r := sig.R()
		s := sig.S()
		if s.IsOverHalfOrder() {
			sNeg := new(btcec.ModNScalar).NegateVal(&s)
			return ecdsa.NewSignature(&r, sNeg)
		}
	}
	return sig
}

// computeRgb20DepositMsg 计算 RGB20 充值 TSS 签名的消息：
// C = sha256(types.Encode(DepositAsset{thresholdSig:nil}))，与 rgbx 合约 computeDepositSignMessage 一致。
func computeRgb20DepositMsg(dep *rtypes.DepositAsset) []byte {
	d := proto.Clone(dep).(*rtypes.DepositAsset)
	d.ThresholdSig = nil
	h := sha256.Sum256(types.Encode(d))
	return h[:]
}

// signPsbt 解析 PSBT，逐输入签名（GG18），写 partial_sigs，广播前 low-S 归一化。
// 侧车 BuildWithdrawal 产出的 PSBT 已固定输入/输出与 RGB 承诺锚点，TSS 只签 witness（Spike 2 §5 结论）。
// psbtSignFunc 对单个输入的 sigHash 进行签名，返回 GG18 阈值签名（DER）。
type psbtSignFunc func(sigHash []byte, sessionName string) *signResult

// signPsbt 协调者侧发起 RGB20 提现签名：发布 rgb20-withdraw 签名通知（**带完整提现上下文**，
// E11）让其它签名节点独立核对后入场，再本地参与 GG18。
//
// 上下文（chain33 提现哈希 + 金额/费率/高度门槛/收款 invoice + consignment）是签名节点唯一
// 的核对依据：不带上下文的签名通知一律被拒签（签名节点无从执行 ValidateWithdrawPsbt），
// 测试用的无上下文签名走 signPsbtTestOnly 那条独立路径。
func (t *tssService) signPsbt(req *rgb20.WithdrawSignRequest) ([]byte, error) {
	if req == nil || len(req.Psbt) == 0 || len(req.Chain33TxHash) == 0 || len(req.Consignment) == 0 {
		return nil, fmt.Errorf("invalid rgb20 withdraw sign request: psbt/hash/consignment required")
	}
	// 本笔已收到确定性拒签回执 ⇒ 直接按不可恢复返回：不再发通知、不再等 GG18 超时，
	// 也不再被协调者的重试循环每秒重复一遍（见 handleRgb20WithdrawReject）。
	if err := t.takeSignRejection(req.Chain33TxHash); err != nil {
		return nil, err
	}
	p, err := psbt.NewFromRawBytes(bytes.NewReader(req.Psbt), false)
	if err != nil {
		return nil, fmt.Errorf("decode psbt: %w", err)
	}
	payload, err := json.Marshal(&req.WithdrawSignPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal withdraw sign payload: %w", err)
	}
	signers := t.waitForSufficientSigners()
	if len(signers) > 0 {
		t.setSignRoundSigners(req.Chain33TxHash, signers)
		notify := &ltypes.TssSignNotify{
			TxType:      transactionTypeRgb20Withdraw,
			Payload:     payload,
			Psbt:        req.Psbt,
			Consignment: req.Consignment,
			Signers:     signers,
		}
		t.pubMsg(tssSignNotifyTopic, types.Encode(notify))
	}
	signed, err := t.signPsbtWithSigners(p, signers, func(sigHash []byte, sessionName string) *signResult {
		return t.signMsg(sigHash, sessionName, signers)
	})
	if err != nil {
		// 组签名失败：若期间收到了确定性拒签回执，按拒签原因归类（不可恢复），而不是把
		// "缺签名"当成可重试失败——否则协调者会每秒重试、无限刷屏。
		if rerr := t.takeSignRejection(req.Chain33TxHash); rerr != nil {
			return nil, rerr
		}
		return nil, err
	}
	return signed, nil
}

// signPsbtTestOnly 仅 E2E 的 sign-psbt 测试端点：无提现上下文 ⇒ 签名节点无法做任何提现核对，
// 因此必须由本地配置 rgb20.testSignPsbt 显式打开（默认关闭），且走独立的通知类型
// （transactionTypeTestSign）——生产提现路径永远发不出这个类型。
func (t *tssService) signPsbtTestOnly(psbtBytes []byte) ([]byte, error) {
	if !t.testSignEnabled() {
		return nil, fmt.Errorf("test sign-psbt disabled: set rgb20.testSignPsbt to enable it in test environments")
	}
	p, err := psbt.NewFromRawBytes(bytes.NewReader(psbtBytes), false)
	if err != nil {
		return nil, fmt.Errorf("decode psbt: %w", err)
	}
	signers := t.waitForSufficientSigners()
	if len(signers) > 0 {
		notify := &ltypes.TssSignNotify{
			TxType:  transactionTypeTestSign,
			Psbt:    psbtBytes,
			Signers: signers,
		}
		t.pubMsg(tssSignNotifyTopic, types.Encode(notify))
	}
	log.Warn("signPsbtTestOnly signing psbt WITHOUT any withdrawal validation (test-only)", "psbtLen", len(psbtBytes))
	return t.signPsbtWithSigners(p, signers, func(sigHash []byte, sessionName string) *signResult {
		return t.signMsg(sigHash, sessionName, signers)
	})
}

// testSignEnabled 本地是否允许无上下文的测试签名（rgb20.testSignPsbt，默认关闭）。
func (t *tssService) testSignEnabled() bool {
	return t.client != nil && t.client.cfg.Rgb20.TestSignPsbt
}

// setSignRoundSigners 记录本节点为某笔提现选定的签名节点（拒签回执的来源核对用）。
func (t *tssService) setSignRoundSigners(chain33Hash []byte, signers []string) {
	t.signMu.Lock()
	defer t.signMu.Unlock()
	t.signRoundSigners[hex.EncodeToString(chain33Hash)] = signers
}

// expectedSigners 取本节点为某笔提现选定的签名节点集合。
func (t *tssService) expectedSigners(chain33Hash []byte) []string {
	t.signMu.Lock()
	defer t.signMu.Unlock()
	return t.signRoundSigners[hex.EncodeToString(chain33Hash)]
}

// takeSignRejection 取出某笔提现已记录的确定性拒签回执并还原成不可恢复错误；无记录返回 nil。
func (t *tssService) takeSignRejection(chain33Hash []byte) error {
	t.signMu.Lock()
	r, ok := t.signRejections[hex.EncodeToString(chain33Hash)]
	t.signMu.Unlock()
	if !ok {
		return nil
	}
	return rgb20.NewUnrecoverableWithdrawError(r.Class,
		fmt.Errorf("signer %s rejected the withdrawal: %s", r.Rejector, r.Reason))
}

// recordSignRejection 记录拒签回执。只接受来自**本轮签名节点**的回执：回执走 P2P 广播，
// 任何人都能发，否则伪造一份回执就能让一笔合法提现被永久判为不可恢复（DoS）。
//
// 两道核对（缺一不可）：
//   - Rejector 必须是本节点为这笔提现选定的签名节点之一；
//   - Rejector 必须与 P2P 报文的来源（TopicData.From，libp2p 记录的发布者）一致，
//     否则任何节点都能冒名顶替某个签名节点发回执。
//
// 仍然无法区分"签名节点真的拒签"与"该签名节点谎称拒签"——但后者本来就能靠不参与签名达到
// 同样的阻断效果（组签名超时），所以这里不引入新的信任假设。
func (t *tssService) recordSignRejection(reject *rgb20.WithdrawSignReject, from string) {
	if reject == nil || len(reject.Chain33TxHash) == 0 {
		return
	}
	if from != "" && from != reject.Rejector {
		log.Error("recordSignRejection rejector does not match the publisher",
			"rejector", reject.Rejector, "from", from,
			"chain33Hash", hex.EncodeToString(reject.Chain33TxHash))
		return
	}
	signers := t.expectedSigners(reject.Chain33TxHash)
	isSigner := false
	for _, s := range signers {
		if s == reject.Rejector {
			isSigner = true
			break
		}
	}
	if !isSigner {
		log.Error("recordSignRejection not from a signer of this round",
			"rejector", reject.Rejector, "from", from, "signers", signers,
			"chain33Hash", hex.EncodeToString(reject.Chain33TxHash))
		return
	}
	t.signMu.Lock()
	defer t.signMu.Unlock()
	if _, ok := t.signRejections[hex.EncodeToString(reject.Chain33TxHash)]; ok {
		return // 已有记录（本笔是终态），不覆盖
	}
	t.signRejections[hex.EncodeToString(reject.Chain33TxHash)] = *reject
	log.Error("recordSignRejection stop retrying this withdraw",
		"chain33Hash", hex.EncodeToString(reject.Chain33TxHash),
		"class", reject.Class, "reason", reject.Reason)
}

// publishSignRejection 签名节点把**确定性拒签**回传协调者。
//
// 不这样做的话，本节点拒签只表现为"组签名少了一个 signer ⇒ 超时"，协调者会把这笔当可重试、
// 每秒重试刷屏（rgb20.UnrecoverableWithdrawError 的设计目的正是让这类失败被识别为终态）。
// 只回传可分类的拒签：其余失败（pending 查不到、consignment 校验暂时失败等）是可重试的，
// 回传会把"暂时失败"误判成终态。
func (t *tssService) publishSignRejection(payloadBytes []byte, err error) {
	class, unrecoverable := rgb20.IsUnrecoverableWithdraw(err)
	if !unrecoverable {
		return
	}
	payload := &rgb20.WithdrawSignPayload{}
	if jsonErr := json.Unmarshal(payloadBytes, payload); jsonErr != nil || len(payload.Chain33TxHash) == 0 {
		return
	}
	reject := &rgb20.WithdrawSignReject{
		Chain33TxHash: payload.Chain33TxHash,
		Class:         class,
		Reason:        err.Error(),
		Rejector:      t.selfPeerId,
	}
	body, jsonErr := json.Marshal(reject)
	if jsonErr != nil {
		log.Error("publishSignRejection marshal", "err", jsonErr)
		return
	}
	t.pubMsg(tssSignNotifyTopic, types.Encode(&ltypes.TssSignNotify{
		TxType:  transactionTypeRgb20WithdrawReject,
		Payload: body,
	}))
}

// signPsbtInternal 签名节点参与 GG18 PSBT 签名（不发布通知，避免递归）。
func (t *tssService) signPsbtInternal(psbtBytes []byte) ([]byte, error) {
	p, err := psbt.NewFromRawBytes(bytes.NewReader(psbtBytes), false)
	if err != nil {
		return nil, fmt.Errorf("decode psbt: %w", err)
	}
	signers := t.waitForSufficientSigners()
	return t.signPsbtWithSigners(p, signers, func(sigHash []byte, sessionName string) *signResult {
		return t.signMsg(sigHash, sessionName, signers)
	})
}

// signPsbtWithSigners 对 PSBT 逐输入签名并写 partial_sigs（signFn 可注入，便于单测）。
// 侧车 BuildWithdrawal 产出的 PSBT 已固定输入/输出与 RGB 承诺锚点，TSS 只签 witness（Spike 2 §5 结论）。
func (t *tssService) signPsbtWithSigners(p *psbt.Packet, signers []string, signFn psbtSignFunc) ([]byte, error) {
	if p == nil || p.UnsignedTx == nil {
		return nil, fmt.Errorf("psbt invalid: missing unsigned tx")
	}
	if len(p.UnsignedTx.TxIn) != len(p.Inputs) {
		return nil, fmt.Errorf("psbt invalid: tx=%d inputs=%d", len(p.UnsignedTx.TxIn), len(p.Inputs))
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(make(map[wire.OutPoint]*wire.TxOut, len(p.Inputs)))
	for i := range p.Inputs {
		op := p.UnsignedTx.TxIn[i].PreviousOutPoint
		if p.Inputs[i].WitnessUtxo != nil {
			prevOutFetcher.AddPrevOut(op, p.Inputs[i].WitnessUtxo)
		} else if p.Inputs[i].NonWitnessUtxo != nil {
			if int(op.Index) >= len(p.Inputs[i].NonWitnessUtxo.TxOut) {
				return nil, fmt.Errorf("input %d non-witness utxo out of range", i)
			}
			prevOutFetcher.AddPrevOut(op, p.Inputs[i].NonWitnessUtxo.TxOut[op.Index])
		} else {
			return nil, fmt.Errorf("input %d missing witness/non-witness utxo", i)
		}
	}
	txSigHashes := txscript.NewTxSigHashes(p.UnsignedTx, prevOutFetcher)
	pubKeyBytes := t.tssPublicKey.SerializeCompressed()
	txHash := p.UnsignedTx.TxHash()
	sessions := make([]string, len(p.Inputs))
	sigHashes := make([][]byte, len(p.Inputs))
	// p2wshScripts[i] 非空 = 第 i 个输入是原生 P2WSH，值是该输入的 witnessScript（= scriptCode）。
	// **逐输入承载**：同一笔 tx 的不同输入可以属于不同 witnessScript（扫集/提现会把多个用户的
	// 充值 UTXO 与主池 UTXO 拼在一起），绝不能假设"整笔只有一个脚本"。每个输入的 sighash
	// 用它自己的 scriptCode 算（BIP143 就是逐输入定义），签名轮次也按输入下标独立
	// （sessions[i] = "psbt-<txid>-<i>"，见下）。
	p2wshScripts := make([][]byte, len(p.Inputs))
	for i := range p.Inputs {
		op := p.UnsignedTx.TxIn[i].PreviousOutPoint
		prevOut := prevOutFetcher.FetchPrevOutput(op)
		scriptCode, witnessScript, err := resolvePsbtInputScriptCode(&p.Inputs[i], prevOut, i)
		if err != nil {
			return nil, err
		}
		p2wshScripts[i] = witnessScript
		sigHashType := p.Inputs[i].SighashType
		if sigHashType == 0 {
			sigHashType = txscript.SigHashAll // 缺省 SIGHASH_ALL（PSBT_IN_SIGHASH_TYPE）
		}
		sigHash, err := txscript.CalcWitnessSigHash(scriptCode, txSigHashes, sigHashType, p.UnsignedTx, i, prevOut.Value)
		if err != nil {
			return nil, fmt.Errorf("calc sig hash for input %d: %w", i, err)
		}
		sigHashes[i] = sigHash
		sessions[i] = fmt.Sprintf("psbt-%s-%d", txHash, i)
	}
	for i := range p.Inputs {
		result := signFn(sigHashes[i], sessions[i])
		if result == nil || result.err != nil {
			if result == nil {
				return nil, fmt.Errorf("sign psbt input %d: nil result", i)
			}
			return nil, fmt.Errorf("sign psbt input %d: %w", i, result.err)
		}
		// low-S 归一化（广播前检查）
		sig, err := ecdsa.ParseDERSignature(result.sig)
		if err != nil {
			return nil, fmt.Errorf("parse sig input %d: %w", i, err)
		}
		sig = normalizeLowS(sig)
		sigHashType := p.Inputs[i].SighashType
		if sigHashType == 0 {
			sigHashType = txscript.SigHashAll
		}
		sigWithHash := append(sig.Serialize(), byte(sigHashType))
		p.Inputs[i].PartialSigs = append(p.Inputs[i].PartialSigs, &psbt.PartialSig{
			PubKey:    pubKeyBytes,
			Signature: sigWithHash,
		})
		log.Debug("signPsbt applied partial sig", "input", i, "p2wsh", len(p2wshScripts[i]) > 0)
	}
	// Phase 5 修复：签完所有输入后 finalize（partial sig → final witness）。
	// 根因：sign-psbt 之前返回只含 partial_sigs 的未 finalize PSBT，侧车 extract_tx()
	// 会丢弃 partial sig 得到无 witness 交易，广播报 "Witness program hash mismatch"。
	//
	// P2WSH 输入必须**自建** final witness，不能交给 psbt.MaybeFinalizeAll：上游 finalizer 对
	// P2WSH 只支持多签（finalizer.go 顶部注释 "p2sh (legacy) and p2wsh currently support only
	// multisig and no other custom script"），我们的充值脚本 `<push userID> OP_DROP
	// <push tssPub> OP_CHECKSIG` 走上去会 ErrUnsupportedScriptType。
	// 先自建 P2WSH 的（MaybeFinalize 对已 finalized 的输入直接返回成功），再把剩下的交给上游，
	// 这样"一笔 tx 里同时有 P2WSH 与 P2WPKH 输入"也能各自走对路径。
	for i := range p.Inputs {
		if len(p2wshScripts[i]) == 0 {
			continue
		}
		if err := finalizeP2WSHInput(&p.Inputs[i], p2wshScripts[i]); err != nil {
			return nil, fmt.Errorf("finalize p2wsh input %d: %w", i, err)
		}
	}
	if err := psbt.MaybeFinalizeAll(p); err != nil {
		return nil, fmt.Errorf("finalize psbt: %w", err)
	}
	var buf bytes.Buffer
	if err := p.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize psbt: %w", err)
	}
	return buf.Bytes(), nil
}

// resolvePsbtInputScriptCode 求某个 PSBT 输入在 BIP143 下计算 sighash 所需的 **scriptCode**，
// 并返回"这个输入是不是原生 P2WSH"（非 nil 的第二个返回值即该输入的 witnessScript）。
//
//   - 原生 P2WSH（prevout = `OP_0 <sha256(witnessScript)>`）：scriptCode **就是 witnessScript**，
//     取自 PSBT_IN_WITNESS_SCRIPT。
//   - 其余（P2WPKH）：scriptCode = prevout.PkScript —— BIP143 对 P2WPKH 要求的正是 p2pkh 形态
//     脚本，btcd 的 CalcWitnessSigHash 内部会由 pubkey hash 重建，直接传 pkScript 即可。
//
// 传错 scriptCode 不是"轻微偏差"而是**签名必废**：同一条 witnessScript 与它的 output program
// （34 字节的 `00 20 || program`）会算出两个不同的 sighash，用后者签出的签名在链上必被
// OP_CHECKSIG 拒（可复现证明见 rgbx/types/p2wsh_deposit_spend_test.go）。
//
// 三条 fail-closed 校验都在签名之前做：
//   - P2WSH 输入必须带 witnessScript（缺了就没有正确的 scriptCode，签出来的只会是废签名）；
//   - **witnessScript 的 sha256 必须等于 prevout 的 program** —— 这条把"被签的消息"钉死在这笔
//     UTXO 上。没有它，一份构造过的 PSBT 就能让 TSS 组对任意脚本（乃至任意语义的 scriptCode）
//     出签名，等于把签名轮次变成通用签名预言机；
//   - 不得携带 redeemScript（原生 P2WSH 没有嵌套 P2SH 那一层，带了说明形态不是我们要签的）。
func resolvePsbtInputScriptCode(in *psbt.PInput, prevOut *wire.TxOut, idx int) (scriptCode, witnessScript []byte, err error) {
	if in == nil || prevOut == nil {
		return nil, nil, fmt.Errorf("input %d: prevout unavailable", idx)
	}
	if !txscript.IsPayToWitnessScriptHash(prevOut.PkScript) {
		return prevOut.PkScript, nil, nil
	}
	if len(in.WitnessScript) == 0 {
		return nil, nil, fmt.Errorf("input %d is p2wsh but the psbt carries no witness script: "+
			"BIP143 scriptCode must be the witnessScript itself, not the 34-byte output program %x",
			idx, prevOut.PkScript)
	}
	if len(in.RedeemScript) != 0 {
		return nil, nil, fmt.Errorf("input %d is native p2wsh but carries a redeem script", idx)
	}
	sum := sha256.Sum256(in.WitnessScript)
	if !bytes.Equal(sum[:], witnessProgramOf(prevOut.PkScript)) {
		return nil, nil, fmt.Errorf("input %d witness script does not hash to the prevout program "+
			"(refusing to sign an unbound scriptCode)", idx)
	}
	return in.WitnessScript, in.WitnessScript, nil
}

// witnessProgramOf 取 `OP_0 <push 32> <program>` 里的 program（调用方已用
// txscript.IsPayToWitnessScriptHash 确认过形态，这里只做边界防御）。
func witnessProgramOf(pkScript []byte) []byte {
	if len(pkScript) < 34 {
		return nil
	}
	return pkScript[2:]
}

// finalizeP2WSHInput 为**原生 P2WSH** 输入自建 final witness（partial sig → witness 栈）。
//
// witness 栈就是 [sig||sighashType, witnessScript] 两项：userID 在脚本里只被 push 后立即
// OP_DROP，是纯标签、不需要花费方提供（见 rgbx/types/p2wsh_deposit.go 与
// p2wsh_deposit_spend_test.go 的 CleanStack 断言）。
//
// 为什么不能交给 psbt.MaybeFinalizeAll：上游 finalizer 对 P2WSH 只认多签，非多签脚本会
// ErrUnsupportedScriptType；即便强行走通也会在栈顶塞一个 CHECKMULTISIG 的 dummy nil，
// 产出非标准的 [nil, sig, witnessScript]（语义上仍能过验证，但属于靠运气）。
//
// 只接受**恰好 1 个** partial sig：多签/多钥形态不在本方案里，宁可失败也不猜。
func finalizeP2WSHInput(in *psbt.PInput, witnessScript []byte) error {
	if in == nil {
		return fmt.Errorf("nil input")
	}
	if len(in.PartialSigs) != 1 {
		return fmt.Errorf("expected exactly 1 partial sig for the p2wsh script, got %d", len(in.PartialSigs))
	}
	var buf bytes.Buffer
	// psbt.WriteTxWitness 就是 PSBT final_script_witness 的线格式（varint 项数 + 逐项 varbytes），
	// 与上游 finalizer 写 final witness 用的是同一个函数。
	if err := psbt.WriteTxWitness(&buf, wire.TxWitness{in.PartialSigs[0].Signature, witnessScript}); err != nil {
		return fmt.Errorf("write witness: %w", err)
	}
	in.FinalScriptWitness = buf.Bytes()
	// 与上游 Finalize 的收尾一致：final witness 落定后清掉中间态（partial sig / sighash type），
	// 避免同一份 PSBT 里"已 finalize"与"待 finalize"两种状态并存被下游误读。
	// witnessScript 与 witness_utxo 保留：前者是审计线索（脚本原文），后者广播/侧车提取仍要用，
	// 且 psbt.MaybeFinalizeAll 对已 finalized 的输入直接跳过，不会因为 witnessScript 在场而重跑。
	in.PartialSigs = nil
	in.SighashType = 0
	return nil
}

// processSignRgb20Deposit 发起 rgb20-deposit TSS 签名轮次（BL-3/HR-6）：
// 下发 txType=rgb20-deposit 通知（payload=DepositSignPayload JSON），签名节点独立验证后签 C；
// 主节点本地也参与签名并返回阈值签名。
func (t *tssService) processSignRgb20Deposit(payload *rgb20.DepositSignPayload) ([]byte, error) {
	if payload == nil || payload.Deposit == nil {
		return nil, types.ErrInvalidParam
	}
	signers := t.waitForSufficientSigners()
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal deposit payload: %w", err)
	}
	notify := &ltypes.TssSignNotify{
		TxType:  transactionTypeRgb20Deposit,
		Payload: payloadBytes,
		Signers: signers,
	}
	t.pubMsg(tssSignNotifyTopic, types.Encode(notify))
	log.Debug("processSignRgb20Deposit published", "receiveId", payload.ReceiveID,
		"sessionId", payload.SessionID, "signers", signers)

	// 主节点本地签名（GG18 组内产生同一阈值签名）
	msg := computeRgb20DepositMsg(payload.Deposit)
	res := t.signMsg(msg, payload.SessionID, signers)
	if res.err != nil {
		return nil, res.err
	}
	return res.sig, nil
}

func (t *tssService) parseTxFromNotify(notify *ltypes.TssSignNotify) (*wire.MsgTx, []int64, error) {
	if notify == nil {
		return nil, nil, types.ErrInvalidParam
	}
	if len(notify.BtcTxData) == 0 {
		return nil, nil, fmt.Errorf("empty BtcTxData")
	}
	if len(notify.InputAmounts) == 0 {
		return nil, nil, fmt.Errorf("empty input amounts")
	}
	var tx wire.MsgTx
	if err := tx.DeserializeNoWitness(bytes.NewReader(notify.BtcTxData)); err != nil {
		return nil, nil, fmt.Errorf("deserialize tx failed: %w", err)
	}
	if len(tx.TxIn) != len(notify.InputAmounts) {
		return nil, nil, fmt.Errorf("input count mismatch: tx=%d inputAmounts=%d", len(tx.TxIn), len(notify.InputAmounts))
	}
	return &tx, notify.InputAmounts, nil
}

func (t *tssService) validateWithdrawTx(tx *wire.MsgTx, inputAmounts []int64, req *withdrawRequest) error {

	btcAddr, err := btcutil.DecodeAddress(req.toAddress, &t.client.neutrinoCfg.ChainParams)
	if err != nil {
		log.Error("validateWithdrawTx decode address", "err", err, "address", req.toAddress,
			"withdrawTxHash", hex.EncodeToString(req.chain33WithdrawHash))
		return fmt.Errorf("decode address failed")
	}
	btcAddrScript, err := txscript.PayToAddrScript(btcAddr)
	if err != nil {
		log.Error("validateWithdrawTx pay to addr script", "address", req.toAddress,
			"withdrawTxHash", hex.EncodeToString(req.chain33WithdrawHash), "err", err)
		return fmt.Errorf("pay to addr script failed")
	}
	var withdrawAmount, changeAmount int64
	for _, output := range tx.TxOut {
		if len(output.PkScript) > 0 && output.PkScript[0] == txscript.OP_RETURN {
			continue
		}
		if len(t.pkScript) > 0 && bytes.Equal(output.PkScript, t.pkScript) {
			changeAmount += output.Value
			continue
		}
		if !bytes.Equal(output.PkScript, btcAddrScript) {
			log.Error("validateWithdrawTx unexpected output script", "address", req.toAddress,
				"expected", hex.EncodeToString(btcAddrScript), "actual", hex.EncodeToString(output.PkScript),
				"withdrawTxHash", hex.EncodeToString(req.chain33WithdrawHash))
			return fmt.Errorf("unexpected output script")
		}
		withdrawAmount += output.Value
	}

	var totalInput int64
	for _, amount := range inputAmounts {
		totalInput += amount
	}
	var totalOutput int64
	for _, out := range tx.TxOut {
		totalOutput += out.Value
	}
	fee := totalInput - totalOutput
	expectedFee := int64(estimateBtcFee(tx, btcutil.Amount(req.feeRate)))
	// 控制手续费在合理范围内
	if fee > 2*expectedFee || fee < 0 {
		log.Error("validateWithdrawSignNotify invalid fee", "fee", fee, "expected", expectedFee,
			"withdrawTxHash", hex.EncodeToString(req.chain33WithdrawHash))
		return fmt.Errorf("invalid fee")
	}
	// 验证提现的关键是总支出不能超过提现金额，允许的最大磨损不能超过最小找零金额
	if totalInput-changeAmount > int64(req.amount)+minChangeAmount {
		log.Error("validateWithdrawSignNotify withdraw overflowed",
			"actualWithdraw", withdrawAmount, "changeAmount", changeAmount,
			"totalInput", totalInput, "expectWithdraw", int64(req.amount),
			"withdrawTxHash", hex.EncodeToString(req.chain33WithdrawHash))
		return fmt.Errorf("withdraw overflowed")
	}
	if withdrawAmount > int64(req.amount) || withdrawAmount < minChangeAmount {
		log.Error("validateWithdrawSignNotify invalid withdraw amount", "actualWithdraw", withdrawAmount,
			"expectWithdraw", int64(req.amount), "withdrawTxHash", hex.EncodeToString(req.chain33WithdrawHash))
		return fmt.Errorf("invalid withdraw amount")
	}
	return nil
}

func (t *tssService) checkNonOfficialWithdrawSign(chain33WithdrawHash []byte) (*rtypes.PendingTx, error) {

	txHash := hex.EncodeToString(chain33WithdrawHash)
	pendingTx, err := t.client.getRgbxPendingTxByHash(chain33WithdrawHash)
	if err != nil {
		log.Error("checkNonOfficialWithdrawSign getRgbxPendingTxByHash", "txHash", txHash, "err", err)
		return nil, err
	}
	if pendingTx.GetConfirmed() {
		return nil, fmt.Errorf("withdraw already confirmed")
	}
	return pendingTx, nil
}

func (t *tssService) checkStickyInput(chain33WithdrawHash []byte, tx *wire.MsgTx) error {
	if len(tx.TxIn) == 0 {
		return fmt.Errorf("withdraw tx has no inputs")
	}
	stickyOutPoint := tx.TxIn[len(tx.TxIn)-1].PreviousOutPoint.String()
	// 验证绑定的哈希是否一致
	expectedHash := t.client.getExpectedWithdrawHash(stickyOutPoint)
	if len(expectedHash) > 0 && !bytes.Equal(expectedHash, chain33WithdrawHash) {
		log.Error("checkStickyInput sticky input mismatch", "expected", hex.EncodeToString(expectedHash),
			"actual", hex.EncodeToString(chain33WithdrawHash), "stickyOutPoint", stickyOutPoint)
		return fmt.Errorf("invalid sticky input")
	}

	// 如果本地已记录过成功签名的绑定utxo，后续请求必须一致
	expectUTXO := t.client.getWithdrawStickyUTXO(chain33WithdrawHash)
	if expectUTXO != nil && expectUTXO.OutPoint.String() != stickyOutPoint {
		log.Error("checkStickyInput sticky input changed", "expected", expectUTXO.OutPoint.String(),
			"actual", stickyOutPoint, "chain33WithdrawHash", hex.EncodeToString(chain33WithdrawHash))
		return fmt.Errorf("invalid sticky input")
	}
	return nil
}

// handleSignNotify handles incoming TSS sign notifications
// All nodes (including main node) receive this and participate in signing
func (t *tssService) handleSignNotify(data *types.TopicData) {

	defer func() {
		if r := recover(); r != nil {
			log.Error("handleSignNotify panic", "err", r)
		}
	}()
	if !t.dkgCompleted.Load() {
		log.Error("handleSignNotify", "err", "DKG not completed")
		return
	}

	notify := &ltypes.TssSignNotify{}
	err := types.Decode(data.GetData(), notify)
	if err != nil {
		log.Error("handleSignNotify Decode", "err", err)
		return
	}

	// 拒签回执是广播事实（不属于任何签名轮次的"参与"），必须在下面的 isSigner 门槛之前处理：
	// 回执的 Signers 为空，过不了 isSigner 检查。
	if notify.TxType == transactionTypeRgb20WithdrawReject {
		reject := &rgb20.WithdrawSignReject{}
		if err := json.Unmarshal(notify.Payload, reject); err != nil {
			log.Error("handleSignNotify decode sign reject", "err", err)
			return
		}
		t.recordSignRejection(reject, data.GetFrom())
		return
	}

	isSigner := false
	for _, signer := range notify.Signers {
		if signer == t.selfPeerId {
			isSigner = true
			break
		}
	}
	if !isSigner {
		log.Debug("handleSignNotify not signer", "signers", notify.Signers, "selfPeerId", t.selfPeerId, "type", notify.TxType)
		return
	}

	// RGB20 分支：rgb20-deposit 签名轮次 / RGB20 提现 PSBT 签名 / 无上下文的测试签名。
	// 分派只看 TxType：生产提现必须走 transactionTypeRgb20Withdraw 并携带完整上下文。
	switch notify.TxType {
	case transactionTypeRgb20Deposit:
		if err := t.handleRgb20DepositSign(notify); err != nil {
			log.Error("handleSignNotify handleRgb20DepositSign", "err", err)
		}
		return
	case transactionTypeRgb20Withdraw:
		if err := t.handleRgb20WithdrawSign(notify); err != nil {
			log.Error("handleSignNotify handleRgb20WithdrawSign", "err", err)
			// 确定性拒签回传协调者（否则只表现为组签名超时 ⇒ 协调者每秒重试刷屏）。
			t.publishSignRejection(notify.Payload, err)
		}
		return
	case transactionTypeTestSign:
		if err := t.handleTestSignNotify(notify); err != nil {
			log.Error("handleSignNotify handleTestSignNotify", "err", err)
		}
		return
	}

	tx, inputAmounts, err := t.parseTxFromNotify(notify)
	if err != nil {
		log.Error("handleSignNotify parseTxFromNotify", "type", notify.TxType, "err", err)
		return
	}

	if notify.TxType == transactionTypeWithdraw {
		chain33WithdrawHash := notify.Payload
		if err = t.checkStickyInput(chain33WithdrawHash, tx); err != nil {
			log.Error("handleSignNotify checkStickyInput", "err", err,
				"withDrawHash", hex.EncodeToString(chain33WithdrawHash), "btcHash", tx.TxHash().String())
			return
		}
		pendingTx, err := t.checkNonOfficialWithdrawSign(chain33WithdrawHash)
		if err != nil {
			log.Error("handleSignNotify checkNonOfficialWithdrawSign", "type", notify.TxType,
				"withDrawHash", hex.EncodeToString(chain33WithdrawHash), "btcHash", tx.TxHash().String(), "err", err)
			return
		}
		req := pending2WithdrawRequest(pendingTx)
		err = t.validateWithdrawTx(tx, inputAmounts, req)
		if err != nil {
			log.Error("handleSignNotify validateWithdrawTx", "err", err,
				"withdrawHash", hex.EncodeToString(chain33WithdrawHash), "btcHash", tx.TxHash().String())
			return
		}
	}

	if err = t.signBtcTx(tx, inputAmounts, notify.Signers); err != nil {
		log.Error("handleSignNotify signBtcTx", "type", notify.TxType, "err", err)
		return
	}
	if notify.TxType == transactionTypeWithdraw {
		stickyUTXO := &UTXO{
			OutPoint: tx.TxIn[len(tx.TxIn)-1].PreviousOutPoint,
		}
		if err = t.client.setWithdrawStickyUTXO(notify.Payload, stickyUTXO); err != nil {
			log.Error("handleSignNotify setWithdrawStickyUTXO", "err", err,
				"withdrawHash", hex.EncodeToString(notify.Payload), "btcHash", tx.TxHash().String())
		}
	}
	log.Debug("handleSignNotify success", "txType", notify.TxType,
		"payload", hex.EncodeToString(notify.Payload), "btcHash", tx.TxHash().String())
}

// handleRgb20DepositSign 签名节点处理 rgb20-deposit 轮次：
// 独立验证（本地 receive 去重 + 地址绑定 + 金额 + 侧车 ValidateConsignment + 同步高度门槛），
// 通过后签 C。
func (t *tssService) handleRgb20DepositSign(notify *ltypes.TssSignNotify) error {
	if t.client.rgb20 == nil {
		return fmt.Errorf("rgb20 adapter not configured")
	}
	payload := &rgb20.DepositSignPayload{}
	if err := json.Unmarshal(notify.Payload, payload); err != nil {
		return fmt.Errorf("decode rgb20-deposit payload: %w", err)
	}
	if payload.Deposit == nil {
		return fmt.Errorf("invalid rgb20-deposit payload: nil deposit")
	}
	// 签名节点独立验证（BL-3）
	if err := t.client.rgb20.ValidateDepositConsignment(payload); err != nil {
		return fmt.Errorf("validate rgb20 deposit: %w", err)
	}
	msg := computeRgb20DepositMsg(payload.Deposit)
	res := t.signMsg(msg, payload.SessionID, notify.Signers)
	if res.err != nil {
		return res.err
	}
	log.Debug("handleRgb20DepositSign signed", "receiveId", payload.ReceiveID, "sessionId", payload.SessionID)
	return nil
}

// handleRgb20WithdrawSign 签名节点处理 RGB20 提现 PSBT：
//  1. 解出协调者下发的提现上下文（**声称值**）；
//  2. 按 chain33 提现哈希取链上 pending（**真值**）并逐项比对声称值（payload 是协调者给的，
//     不能只信它）；
//  3. ValidateWithdrawPsbt 独立交叉核对（BL-4/HR-3：金额覆盖 S1、费用区间、dust cap、同步
//     高度、closed-seal 非 pending-mint），并在**同一份已验证事实**上做 sticky seal 核对（E9-B）；
//  4. 全部通过才参与 GG18 签名；sticky 记录只在签名成功之后写（一次失败签名不得锁死合法重试）。
//
// 注意这里**没有**"Payload 为空就跳过校验"的旁路：无上下文的签名走 transactionTypeTestSign
// 那条独立路径，且必须由本地配置显式开启。
func (t *tssService) handleRgb20WithdrawSign(notify *ltypes.TssSignNotify) error {
	payload, psbtBytes, consignment, err := parseRgb20WithdrawNotify(notify)
	if err != nil {
		return err
	}
	if t.client.rgb20 == nil {
		return fmt.Errorf("rgb20 adapter not configured")
	}
	pendingTx, err := t.checkNonOfficialWithdrawSign(payload.Chain33TxHash)
	if err != nil {
		return err
	}
	spentSeals, err := t.validateRgb20WithdrawSign(payload, psbtBytes, consignment, pendingTx)
	if err != nil {
		return err
	}
	if _, err := t.signPsbtInternal(psbtBytes); err != nil {
		return fmt.Errorf("sign rgb20 withdrawal psbt: %w", err)
	}
	// sticky 记录只在签名成功之后写（镜像 BTC 侧 handleSignNotify → setWithdrawStickyUTXO）：
	// 签名前写会把一次失败签名变成对该笔提现的永久拒绝。
	if err := t.client.rgb20.SetStickySeal(payload.Chain33TxHash, spentSeals); err != nil {
		log.Error("handleRgb20WithdrawSign SetStickySeal", "err", err,
			"chain33Hash", hex.EncodeToString(payload.Chain33TxHash))
	}
	log.Debug("handleRgb20WithdrawSign signed psbt", "chain33Hash", hex.EncodeToString(payload.Chain33TxHash))
	return nil
}

// parseRgb20WithdrawNotify 解出提现签名通知里的提现上下文与材料，并强制三样东西都在：
// 上下文（payload）、PSBT、consignment。缺任何一项都拒签——没有上下文就没有可核对的东西，
// 签名节点不可能"只签名不核对"（那正是 E11 的漏洞形态）。
func parseRgb20WithdrawNotify(notify *ltypes.TssSignNotify) (*rgb20.WithdrawSignPayload, []byte, []byte, error) {
	if notify == nil || len(notify.Psbt) == 0 {
		return nil, nil, nil, fmt.Errorf("rgb20 withdraw sign notify without psbt")
	}
	if len(notify.Payload) == 0 {
		return nil, nil, nil, fmt.Errorf("rgb20 withdraw sign notify without payload")
	}
	if len(notify.Consignment) == 0 {
		return nil, nil, nil, fmt.Errorf("rgb20 withdraw sign notify without consignment")
	}
	payload := &rgb20.WithdrawSignPayload{}
	if err := json.Unmarshal(notify.Payload, payload); err != nil {
		return nil, nil, nil, fmt.Errorf("decode rgb20-withdraw payload: %w", err)
	}
	if len(payload.Chain33TxHash) == 0 {
		return nil, nil, nil, fmt.Errorf("invalid rgb20-withdraw payload: empty chain33 withdraw hash")
	}
	return payload, notify.Psbt, notify.Consignment, nil
}

// validateRgb20WithdrawSign 是签名节点对一笔提现签名请求的**全部判定**（不签名、不发布、不落盘）：
//
//	payload（协调者声称值） vs pendingTx（链上真值）→ ValidateWithdrawPsbt（BL-4/HR-3 交叉核对
//	+ S1 金额覆盖 + 费用区间 + dust cap + 同步高度 + closed-seal 非 pending-mint）
//	→ 同一份已验证事实上的 sticky seal 核对（E9-B）
//
// 返回本笔实际花掉且被 consignment 关闭的 RGB seal 集合（签名成功后由调用方落盘）。
// 任一环节失败都返回错误 ⇒ 拒签。
func (t *tssService) validateRgb20WithdrawSign(payload *rgb20.WithdrawSignPayload, psbtBytes, consignment []byte,
	pendingTx *rtypes.PendingTx) ([]string, error) {
	// 信任边界：payload 只是协调者的声称值，必须与链上 pending（真值）逐项一致才继续。
	if err := checkWithdrawClaims(payload, pendingTx); err != nil {
		return nil, err
	}
	var spentSeals []string
	valReq := &rgb20.ValidateWithdrawRequest{
		Psbt:        psbtBytes,
		Consignment: consignment,
		// 金额/费率/高度门槛一律取链上 pending（真值），不取 payload 的声称值。
		ExpectedAmount:  pendingTx.GetAmount(),
		MinSyncedHeight: uint64(pendingTx.GetTxBlockHeight()),
		FeeRate:         pendingTx.GetFeeRate(),
		// sticky seal 核对（E9-B）：一笔 chain33 burn 只能绑定一组 RGB seal。核对失败
		// （含"算不出绑定的 seal"）会作为本次校验的错误上抛 ⇒ 拒签 + 回执。
		CheckSpentSeals: func(seals []string) error {
			spentSeals = seals
			return t.client.rgb20.CheckStickySeal(payload.Chain33TxHash, seals)
		},
	}
	if err := t.client.rgb20.ValidateWithdrawPsbt(valReq); err != nil {
		return nil, fmt.Errorf("validate rgb20 withdrawal: %w", err)
	}
	return spentSeals, nil
}

// checkWithdrawClaims 核对协调者声称的提现上下文与链上 pending（真值）是否一致。
// 任何一项不一致都拒签：签名节点签的是"链上这笔提现"，而它只能通过自己的链上视图确认这一点。
func checkWithdrawClaims(payload *rgb20.WithdrawSignPayload, pendingTx *rtypes.PendingTx) error {
	if payload.Amount != pendingTx.GetAmount() {
		return fmt.Errorf("withdraw amount mismatch: claim=%d chain=%d", payload.Amount, pendingTx.GetAmount())
	}
	if payload.FeeRate != pendingTx.GetFeeRate() {
		return fmt.Errorf("withdraw fee rate mismatch: claim=%d chain=%d", payload.FeeRate, pendingTx.GetFeeRate())
	}
	if payload.TxBlockHeight != pendingTx.GetTxBlockHeight() {
		return fmt.Errorf("withdraw block height mismatch: claim=%d chain=%d",
			payload.TxBlockHeight, pendingTx.GetTxBlockHeight())
	}
	if payload.RecipientInvoice != pendingTx.GetTargetAddress() {
		return fmt.Errorf("withdraw recipient invoice mismatch: claim=%q chain=%q",
			payload.RecipientInvoice, pendingTx.GetTargetAddress())
	}
	return nil
}

// handleTestSignNotify 仅 E2E 的 sign-psbt 测试端点使用：不带提现上下文 ⇒ 不做任何提现核对，
// 只参与 GG18 组签名。必须由本地配置 rgb20.testSignPsbt 显式开启（默认关闭），否则拒签。
func (t *tssService) handleTestSignNotify(notify *ltypes.TssSignNotify) error {
	if !t.testSignEnabled() {
		return fmt.Errorf("test sign notify refused: rgb20.testSignPsbt is not enabled on this node")
	}
	if len(notify.Psbt) == 0 {
		return fmt.Errorf("test sign notify without psbt")
	}
	log.Warn("handleTestSignNotify signing psbt WITHOUT any withdrawal validation (test-only)",
		"psbtLen", len(notify.Psbt))
	if _, err := t.signPsbtInternal(notify.Psbt); err != nil {
		return err
	}
	return nil
}

// subTopic subscribes to a P2P topic
func (t *tssService) subTopic(topic string) {
	data := &types.SubTopic{Topic: topic, Module: moduleName}

	for {
		err := t.sendP2PMsg(types.EventSubTopic, data)
		if err == nil {
			log.Info("subTopic success", "topic", topic)
			break
		}
		log.Debug("subTopic", "topic", topic, "err", err)
		time.Sleep(time.Second)
	}
}

// pubMsg publishes a message to a P2P topic
func (t *tssService) pubMsg(topic string, msg []byte) {
	data := &types.PublishTopicMsg{Topic: topic, Msg: msg}
	tryCount := 0

	for {
		tryCount++
		err := t.sendP2PMsg(types.EventPubTopicMsg, data)
		if err == nil || tryCount >= 3 {
			break
		}
		log.Error("pubMsg", "topic", topic, "tryCount", tryCount, "err", err)
		time.Sleep(time.Second)
	}
}

func (t *tssService) sendP2PMsg(ty int64, data interface{}) error {
	msg := t.client.qclient.NewMessage("p2p", ty, data)
	err := t.client.qclient.Send(msg, true)
	if err != nil {
		return err
	}

	resp, err := t.client.qclient.WaitTimeout(msg, time.Second*5)
	if err != nil {
		return err
	}

	reply, ok := resp.GetData().(*types.Reply)
	if !ok {
		return types.ErrTypeAsset
	}

	if !reply.GetIsOk() {
		return types.ErrInvalidParam
	}

	return nil
}

// handleSubMsg handles subscribed messages from P2P network
func (t *tssService) handleSubMsg() {
	for {
		select {
		case <-t.client.ctx.Done():
			return

		case data := <-t.subChan:
			if data.Topic == tssSignNotifyTopic {
				t.handleSignNotify(data)
			}
		}
	}
}

// isDKGCompleted checks if DKG is completed using atomic operation
func (t *tssService) isDKGCompleted() bool {
	return t.dkgCompleted.Load()
}
