package neutrino

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/33cn/chain33/system/crypto/tss"
	"github.com/33cn/chain33/system/crypto/tss/gg18"
	"github.com/33cn/chain33/types"
	"github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
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
}

func newTssService(n *neutrinoClient) *tssService {
	t := &tssService{
		client:     n,
		subChan:    make(chan *types.TopicData, 100),
		signTaskCh: make(chan *signTask, 1024),
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
	// Wait for cross chain info to be available
	info := t.client.getCrossChainInfo()
	for info == nil {
		time.Sleep(3 * time.Second)
		log.Debug("ensureDKG getCrossChainInfo wait 3 seconds...")
		info = t.client.getCrossChainInfo()
	}

	if info.GetTssAddress() != "" {
		log.Debug("ensureDKG already exist, loading from database")
		err := t.loadDKGFromDB()
		if err != nil {
			panic("ensureDKG loadDKGFromDB error: " + err.Error())
		}
		t.dkgCompleted.Store(true)
		return
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

	// Commit DKG result to main chain with retry
	commitDKG := &rtypes.CommitDKG{
		AssetSymbol: rtypes.BTCSymbol,
		DkgAddress:  t.tssAddress.EncodeAddress(),
		PkScript:    t.pkScript,
	}
	t.client.submitMainchainTxUntilSuccess(rtypes.RgbxX, rtypes.NameCommitDKGAction, commitDKG)
	// RGB20 补 CommitDKG（H6）：对每个注册的 RGB20 合约提交带 pubkey 的 CommitDKG，
	// 否则 checkDeposit/Exec_Deposit 的 RGB20 分支拿不到 CrossChainInfo.Pubkey，无法验 threshold_sig。
	if t.client.rgb20 != nil {
		for _, symbol := range t.client.rgb20.Registry().Symbols() {
			rgbCommitDKG := &rtypes.CommitDKG{
				AssetSymbol: symbol,
				DkgAddress:  t.tssAddress.EncodeAddress(),
				PkScript:    t.pkScript,
				Pubkey:      t.tssPublicKey.SerializeCompressed(),
			}
			t.client.submitMainchainTxUntilSuccess(rtypes.RgbxX, rtypes.NameCommitDKGAction, rgbCommitDKG)
			log.Info("init tssService rgb20 commitDKG", "symbol", symbol, "tssAddress", t.tssAddress.EncodeAddress())
		}
	}
	t.dkgCompleted.Store(true)
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
	notify := &lighttypes.TssSignNotify{
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

func (t *tssService) signMsg(msg []byte, seesionName string, signers []string) *signResult {
	result := &signResult{}
	sigResult, err := gg18.ProcessSign(signers, msg, t.dkgResult, seesionName)
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
// C = sha256(types.Encode(DepositAsset{threshold_sig:nil}))，与 rgbx 合约 computeDepositSignMessage 一致。
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

func (t *tssService) signPsbt(psbtBytes []byte) ([]byte, error) {
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
	for i := range p.Inputs {
		op := p.UnsignedTx.TxIn[i].PreviousOutPoint
		prevOut := prevOutFetcher.FetchPrevOutput(op)
		sigHashType := p.Inputs[i].SighashType
		if sigHashType == 0 {
			sigHashType = txscript.SigHashAll // 缺省 SIGHASH_ALL（PSBT_IN_SIGHASH_TYPE）
		}
		sigHash, err := txscript.CalcWitnessSigHash(prevOut.PkScript, txSigHashes, sigHashType, p.UnsignedTx, i, prevOut.Value)
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
		log.Debug("signPsbt applied partial sig", "input", i)
	}
	var buf bytes.Buffer
	if err := p.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize psbt: %w", err)
	}
	return buf.Bytes(), nil
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
	notify := &lighttypes.TssSignNotify{
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

func (t *tssService) parseTxFromNotify(notify *lighttypes.TssSignNotify) (*wire.MsgTx, []int64, error) {
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
			"withdrawTxHash", hex.EncodeToString(req.chain33WithDrawHash))
		return fmt.Errorf("decode address failed")
	}
	btcAddrScript, err := txscript.PayToAddrScript(btcAddr)
	if err != nil {
		log.Error("validateWithdrawTx pay to addr script", "address", req.toAddress,
			"withdrawTxHash", hex.EncodeToString(req.chain33WithDrawHash), "err", err)
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
				"withdrawTxHash", hex.EncodeToString(req.chain33WithDrawHash))
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
			"withdrawTxHash", hex.EncodeToString(req.chain33WithDrawHash))
		return fmt.Errorf("invalid fee")
	}
	// 验证提现的关键是总支出不能超过提现金额，允许的最大磨损不能超过最小找零金额
	if totalInput-changeAmount > int64(req.amount)+minChangeAmount {
		log.Error("validateWithdrawSignNotify withdraw overflowed",
			"actualWithdraw", withdrawAmount, "changeAmount", changeAmount,
			"totalInput", totalInput, "expectWithdraw", int64(req.amount),
			"withdrawTxHash", hex.EncodeToString(req.chain33WithDrawHash))
		return fmt.Errorf("withdraw overflowed")
	}
	if withdrawAmount > int64(req.amount) || withdrawAmount < minChangeAmount {
		log.Error("validateWithdrawSignNotify invalid withdraw amount", "actualWithdraw", withdrawAmount,
			"expectWithdraw", int64(req.amount), "withdrawTxHash", hex.EncodeToString(req.chain33WithDrawHash))
		return fmt.Errorf("invalid withdraw amount")
	}
	return nil
}

func (t *tssService) checkNonOfficialWithdrawSign(chain33WithDrawHash []byte) (*rtypes.PendingTx, error) {

	txHash := hex.EncodeToString(chain33WithDrawHash)
	pendingTx, err := t.client.getRgbxPendingTxByHash(chain33WithDrawHash)
	if err != nil {
		log.Error("checkNonOfficialWithdrawSign getRgbxPendingTxByHash", "txHash", txHash, "err", err)
		return nil, err
	}
	if pendingTx.GetConfirmed() {
		return nil, fmt.Errorf("withdraw already confirmed")
	}
	return pendingTx, nil
}

func (t *tssService) checkStickyInput(chain33WithDrawHash []byte, tx *wire.MsgTx) error {
	if len(tx.TxIn) == 0 {
		return fmt.Errorf("withdraw tx has no inputs")
	}
	stickyOutPoint := tx.TxIn[len(tx.TxIn)-1].PreviousOutPoint.String()
	// 验证绑定的哈希是否一致
	expectedHash := t.client.getExpectedWithdrawHash(stickyOutPoint)
	if len(expectedHash) > 0 && !bytes.Equal(expectedHash, chain33WithDrawHash) {
		log.Error("checkStickyInput sticky input mismatch", "expected", hex.EncodeToString(expectedHash),
			"actual", hex.EncodeToString(chain33WithDrawHash), "stickyOutPoint", stickyOutPoint)
		return fmt.Errorf("invalid sticky input")
	}

	// 如果本地已记录过成功签名的绑定utxo，后续请求必须一致
	expectUTXO := t.client.getWithdrawStickyUTXO(chain33WithDrawHash)
	if expectUTXO != nil && expectUTXO.OutPoint.String() != stickyOutPoint {
		log.Error("checkStickyInput sticky input changed", "expected", expectUTXO.OutPoint.String(),
			"actual", stickyOutPoint, "chain33WithDrawHash", hex.EncodeToString(chain33WithDrawHash))
		return fmt.Errorf("invalid sticky input")
	}
	return nil
}

// handleSignNotify handles incoming TSS sign notifications
// All nodes (including main node) receive this and participate in signing
func (t *tssService) handleSignNotify(msg []byte) {

	defer func() {
		if r := recover(); r != nil {
			log.Error("handleSignNotify panic", "err", r)
		}
	}()
	if !t.dkgCompleted.Load() {
		log.Error("handleSignNotify", "err", "DKG not completed")
		return
	}

	notify := &lighttypes.TssSignNotify{}
	err := types.Decode(msg, notify)
	if err != nil {
		log.Error("handleSignNotify Decode", "err", err)
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

	// RGB20 分支：rgb20-deposit 签名轮次 / RGB20 提现 PSBT 签名。
	switch notify.TxType {
	case transactionTypeRgb20Deposit:
		if err := t.handleRgb20DepositSign(notify); err != nil {
			log.Error("handleSignNotify handleRgb20DepositSign", "err", err)
		}
		return
	case transactionTypeWithdraw:
		if len(notify.Psbt) > 0 {
			if err := t.handleRgb20WithdrawSign(notify); err != nil {
				log.Error("handleSignNotify handleRgb20WithdrawSign", "err", err)
			}
			return
		}
	}

	tx, inputAmounts, err := t.parseTxFromNotify(notify)
	if err != nil {
		log.Error("handleSignNotify parseTxFromNotify", "type", notify.TxType, "err", err)
		return
	}

	if notify.TxType == transactionTypeWithdraw {
		chain33WithDrawHash := notify.Payload
		if err = t.checkStickyInput(chain33WithDrawHash, tx); err != nil {
			log.Error("handleSignNotify checkStickyInput", "err", err,
				"withDrawHash", hex.EncodeToString(chain33WithDrawHash), "btcHash", tx.TxHash().String())
			return
		}
		pendingTx, err := t.checkNonOfficialWithdrawSign(chain33WithDrawHash)
		if err != nil {
			log.Error("handleSignNotify checkNonOfficialWithdrawSign", "type", notify.TxType,
				"withDrawHash", hex.EncodeToString(chain33WithDrawHash), "btcHash", tx.TxHash().String(), "err", err)
			return
		}
		req := pending2WithdrawRequest(pendingTx)
		err = t.validateWithdrawTx(tx, inputAmounts, req)
		if err != nil {
			log.Error("handleSignNotify validateWithdrawTx", "err", err,
				"withdrawHash", hex.EncodeToString(chain33WithDrawHash), "btcHash", tx.TxHash().String())
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
// 独立验证（去重 + 地址绑定 + 金额 + 侧车 ValidateConsignment + 同步高度门槛），通过后签 C。
func (t *tssService) handleRgb20DepositSign(notify *lighttypes.TssSignNotify) error {
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

// handleRgb20WithdrawSign 签名节点处理 RGB20 提现 PSBT：交叉核对（BL-4/HR-3）后 signPsbt。
func (t *tssService) handleRgb20WithdrawSign(notify *lighttypes.TssSignNotify) error {
	if t.client.rgb20 == nil {
		return fmt.Errorf("rgb20 adapter not configured")
	}
	pendingTx, err := t.checkNonOfficialWithdrawSign(notify.Payload)
	if err != nil {
		return err
	}
	valReq := &rgb20.ValidateWithdrawRequest{
		Psbt:            notify.Psbt,
		Consignment:     notify.Consignment,
		ExpectedAmount:  pendingTx.GetAmount(),
		MinSyncedHeight: uint64(pendingTx.GetTxBlockHeight()),
	}
	if err := t.client.rgb20.ValidateWithdrawPsbt(valReq); err != nil {
		return fmt.Errorf("validate rgb20 withdrawal: %w", err)
	}
	if _, err := t.signPsbt(notify.Psbt); err != nil {
		return fmt.Errorf("sign rgb20 withdrawal psbt: %w", err)
	}
	log.Debug("handleRgb20WithdrawSign signed psbt", "chain33Hash", hex.EncodeToString(notify.Payload))
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
				t.handleSignNotify(data.GetData())
			}
		}
	}
}

// isDKGCompleted checks if DKG is completed using atomic operation
func (t *tssService) isDKGCompleted() bool {
	return t.dkgCompleted.Load()
}
