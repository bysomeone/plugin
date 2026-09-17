package neutrino

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/33cn/chain33/types"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// 以下方法使 neutrinoClient 实现 rgb20.Chain33Bridge（提现侧）。

// TSSAddress 返回桥 TSS P2WPKH 地址（DKG 完成后可用）。config.changeAddress 留空时由
// rgb20 适配器据此自动填充，作为 RGB20 提现的找零地址。
func (n *neutrinoClient) TSSAddress() string {
	if n == nil || n.tss == nil || n.tss.tssAddress == nil {
		return ""
	}
	return n.tss.tssAddress.String()
}

// TSSPkScript 返回桥 TSS P2WPKH 输出的 pkScript（签名节点交叉核对提现 PSBT 用）。
func (n *neutrinoClient) TSSPkScript() []byte {
	if n == nil || n.tss == nil {
		return nil
	}
	return n.tss.pkScript
}

// IsUserDepositScript 判断某个输入脚本是否为桥已登记（发放过地址 + 在 watch 集里）的用户
// P2WSH 充值脚本，返回其 userID。签名节点交叉核对提现 PSBT 输入归属时用（C3）：
// 这类脚本的 program 也是 TSS 群钥的脚本，只有 GG18 签名才能花，因此必须被认作"受桥控制"。
func (n *neutrinoClient) IsUserDepositScript(pkScript []byte) (string, bool) {
	if n == nil {
		return "", false
	}
	return n.isWatchedDepositScript(pkScript)
}

// SubmitConfirm 提交 rgbx Confirm 交易（RGB20 提现确认销毁；合约 RGB20 分支跳过 commitment）。
func (n *neutrinoClient) SubmitConfirm(confirm *rtypes.ConfirmTx) error {
	_, err := n.submitMainChainTx(rtypes.RgbxX, rtypes.NameConfirmAction, confirm)
	return err
}

// SignPsbt 通过 TSS 组对 RGB20 提现 PSBT 签名：把提现上下文（chain33 提现哈希、金额、费率、
// 同步高度门槛、收款 invoice、consignment）一并下发，签名节点据此独立核对后参与 GG18（E11）。
func (n *neutrinoClient) SignPsbt(req *rgb20.WithdrawSignRequest) ([]byte, error) {
	return n.tss.signPsbt(req)
}

// SignPsbtTestOnly 仅供 E2E 的 sign-psbt 测试端点：无提现上下文，签名节点不做任何提现核对。
// 由本地配置 rgb20.testSignPsbt 显式开启；生产路径不经过这里。
func (n *neutrinoClient) SignPsbtTestOnly(psbtBytes []byte) ([]byte, error) {
	return n.tss.signPsbtTestOnly(psbtBytes)
}

// SignSweepPsbt 通过 TSS 组对一笔扫集 PSBT 签名（C4）。
//
// 扫集没有 chain33 上下文，签名节点能核对的是"钱有没有被挪出桥"：每个输入都是已登记的用户
// 充值脚本（且带自己的 witnessScript）、每个输出都回主池、手续费有上界。判据全部来自 PSBT 与
// 本节点自己的事实（见 rgb20.Adapter.ValidateSweepPsbt），不采信协调者的声称值。
func (n *neutrinoClient) SignSweepPsbt(psbtBytes []byte) ([]byte, error) {
	return n.tss.signSweepPsbt(psbtBytes)
}

// BroadcastRawTx 广播一笔已定稿的原始交易（扫集用）。
//
// 广播是幂等的：本笔可能已经在本节点可见（上一次广播其实成功了但回包丢失），节点会以
// "已在 mempool/已存在"拒绝，但钱已经在路上 —— 与 BroadcastTx 同一口径，把它算成功。
func (n *neutrinoClient) BroadcastRawTx(rawTx []byte, txid string) error {
	if n == nil || n.bw == nil {
		return fmt.Errorf("btc wallet not started")
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(rawTx)); err != nil {
		return fmt.Errorf("decode sweep tx: %w", err)
	}
	if txid != "" && tx.TxHash().String() != txid {
		return fmt.Errorf("sweep tx hash %s != expected %s", tx.TxHash().String(), txid)
	}
	var lookup txLookup
	if n.bw.rpcClient != nil {
		lookup = n.bw.rpcClient
	}
	if txKnownToNode(lookup, txid) {
		log.Info("BroadcastRawTx sweep already known to the node, skip broadcast", "btcTxid", txid)
		return nil
	}
	if err := n.bw.broadcastTransaction(&tx, txid); err != nil {
		if !broadcastOutcomeIsSuccess(lookup, txid, err) {
			return err
		}
		log.Warn("BroadcastRawTx sweep broadcast failed but the tx is known to the node, treating as success",
			"btcTxid", txid, "err", err)
	}
	return nil
}

// WithdrawState 返回该笔提现落盘的本地状态（空 = 从未处理到广播）。
// 供 rgb20 适配器做与 BTC 侧同构的状态门（见 rgb20.Withdraw 里的 sticky 检查）。
func (n *neutrinoClient) WithdrawState(chain33TxHash []byte) []byte {
	return n.getWithdrawState(chain33TxHash)
}

// txLookup 是「该 txid 在本节点是否可见」的最小查询面。生产用 btcd 全节点 RPC 的
// *rpcclient.Client（与 BuildSpvProof 同源，需要节点开 --txindex）；单测注入假实现。
type txLookup interface {
	GetRawTransactionVerbose(hash *chainhash.Hash) (*btcjson.TxRawResult, error)
}

// txKnownToNode 判断 txid 是否已经在本节点可见（mempool 或链上）。
// 查询能力不可用（没有全节点 RPC，例如纯 neutrino 部署）时返回 false：那只是退回到
// 「按广播结果判定」的既有行为，不会把真正的失败误判成成功。
func txKnownToNode(lookup txLookup, txid string) bool {
	if lookup == nil || txid == "" {
		return false
	}
	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return false
	}
	_, err = lookup.GetRawTransactionVerbose(hash)
	return err == nil
}

// broadcastOutcomeIsSuccess 广播的结果是否应当算成功：广播没报错自然算成功；报了错时，只有
// 「节点其实已经知道这笔交易」才算成功——上一次广播的回包可能在中途丢失（超时/连接重置），
// 节点已经收下并转发，只是这次重试被以「已在 mempool / 已存在」拒绝。其余错误照旧失败。
func broadcastOutcomeIsSuccess(lookup txLookup, txid string, broadcastErr error) bool {
	if broadcastErr == nil {
		return true
	}
	return txKnownToNode(lookup, txid)
}

// BroadcastTx 从已签 PSBT 提取交易并广播，同时登记到 btcwallet pending 缓存用于确认跟踪
// （确认后经 withdrawChan → processWithdrawConfirm 提交 rgbx Confirm，合约 RGB20 分支跳过 commitment）。
//
// 广播是**幂等**的：本笔可能已经在本节点可见——上一次广播其实成功了但回包丢失（连接超时/重置，
// 见 E9 的事故形态），或侧车按 input_seals 重放出了同一笔交易。这两种情况下节点会以「已在
// mempool / 已存在」拒绝，但钱已经在路上：把它算成功，重试才能收敛（否则重试永远失败，链上那笔
// burn 会一直挂着）。判定依据是「节点是否已经知道这个 txid」，而不是错误文本，避免依赖节点措辞。
func (n *neutrinoClient) BroadcastTx(psbtSigned []byte, txid string) error {
	p, err := psbt.NewFromRawBytes(bytes.NewReader(psbtSigned), false)
	if err != nil {
		return fmt.Errorf("decode signed psbt: %w", err)
	}
	// 若侧车 send_end 未 finalize，这里补 finalize（P2WPKH partial sig → witness）。
	if err := psbt.MaybeFinalizeAll(p); err != nil {
		return fmt.Errorf("finalize psbt: %w", err)
	}
	tx, err := psbt.Extract(p)
	if err != nil {
		return fmt.Errorf("extract tx: %w", err)
	}
	// 通过 txid↔chain33 提现哈希映射找到 pending（H4：弃用 OP_RETURN correlation）。
	var chain33Hash []byte
	if n.rgb20 != nil {
		chain33Hash, _ = n.rgb20.GetChain33HashByTxid(txid)
	}

	var lookup txLookup
	if n.bw != nil && n.bw.rpcClient != nil {
		lookup = n.bw.rpcClient
	}
	switch {
	case txKnownToNode(lookup, txid):
		log.Info("BroadcastTx rgb20 withdraw already known to the node, skip broadcast",
			"btcTxid", txid, "chain33Hash", hex.EncodeToString(chain33Hash))
	default:
		if err := n.bw.broadcastTransaction(tx, txid); err != nil {
			if !broadcastOutcomeIsSuccess(lookup, txid, err) {
				return err
			}
			log.Warn("BroadcastTx rgb20 withdraw broadcast failed but the tx is known to the node, treating as success",
				"btcTxid", txid, "chain33Hash", hex.EncodeToString(chain33Hash), "err", err)
		}
	}

	n.bw.addPendingTx(&btcPendingTx{
		tx:                    tx,
		submitTime:            types.Now(),
		confirmations:         0,
		blockHeight:           -1,
		txHash:                tx.TxHash(),
		txType:                transactionTypeWithdraw,
		chain33WithdrawTxHash: chain33Hash,
	})
	// 状态门（与 BTC 侧 processWithdrawRequest 的同名落盘对齐）：广播成功后落 withdrawStatusSent，
	// 配合 rgb20 适配器的「state 非空且 sticky 为空则停下」护栏，以及后续的 Confirm/不可恢复状态。
	if len(chain33Hash) > 0 {
		if err := n.setWithdrawState(chain33Hash, withdrawStatusSent); err != nil {
			log.Error("BroadcastTx rgb20 setWithdrawState", "btcTxid", txid,
				"chain33Hash", hex.EncodeToString(chain33Hash), "err", err)
		}
	}
	log.Info("BroadcastTx rgb20 withdraw tracked", "btcTxid", txid, "chain33Hash", hex.EncodeToString(chain33Hash))
	return nil
}
