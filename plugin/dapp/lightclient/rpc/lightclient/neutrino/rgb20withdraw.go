package neutrino

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/33cn/chain33/types"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil/psbt"
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

// BroadcastTx 从已签 PSBT 提取交易并广播，同时登记到 btcwallet pending 缓存用于确认跟踪
// （确认后经 withdrawChan → processWithdrawConfirm 提交 rgbx Confirm，合约 RGB20 分支跳过 commitment）。
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
	if err := n.bw.broadcastTransaction(tx, txid); err != nil {
		return err
	}
	// 通过 txid↔chain33 提现哈希映射找到 pending（H4：弃用 OP_RETURN correlation）。
	var chain33Hash []byte
	if n.rgb20 != nil {
		chain33Hash, _ = n.rgb20.GetChain33HashByTxid(txid)
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
	log.Info("BroadcastTx rgb20 withdraw tracked", "btcTxid", txid, "chain33Hash", hex.EncodeToString(chain33Hash))
	return nil
}
