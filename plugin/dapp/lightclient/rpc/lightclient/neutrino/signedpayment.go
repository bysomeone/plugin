package neutrino

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcwallet/walletdb"
)

// signedPaymentBucket 持久记录"该 burn 已经产出过付款签名"。
//
// 为什么需要它：原先判断"有没有签过付款"只能看 withdraw state 的 sent/confirmed，而这两个状态
// 是在**广播成功之后**才写的——签名成功但广播失败（或进程在两者之间退出）时，本地没有任何
// 痕迹。放弃提现（Unlock）会因此把一笔已经存在签名的提现退回，签名交易一旦落地就是双付。
// 本记录在签名成功之后、返回/广播之前写入，作为"这笔不能再解锁"的后验事实。
//
// 它只写一次、永不清除：清除它等于抹掉"曾经签过"的证据。
var signedPaymentBucket = []byte("rgbx-signed-payment")

const signedPaymentMarker = "signed"

// signedPaymentKV 是持久记录的读写面。生产用 walletdb（与 withdraw state 同一引擎），
// 单测注入错误实现以验证 fail-closed。
type signedPaymentKV interface {
	Has(burnTxHash []byte) (bool, error)
	// PutOnce 幂等：已存在时不重复写。
	PutOnce(burnTxHash []byte) error
}

type walletSignedPaymentKV struct {
	db walletdb.DB
}

func (w *walletSignedPaymentKV) Has(burnTxHash []byte) (bool, error) {
	if w == nil || w.db == nil {
		return false, fmt.Errorf("signed payment store is not initialized")
	}
	var found bool
	err := walletdb.View(w.db, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(signedPaymentBucket)
		if bucket == nil {
			return nil
		}
		found = bucket.Get(burnTxHash) != nil
		return nil
	})
	return found, err
}

func (w *walletSignedPaymentKV) PutOnce(burnTxHash []byte) error {
	if w == nil || w.db == nil {
		return fmt.Errorf("signed payment store is not initialized")
	}
	return walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(signedPaymentBucket)
		if err != nil {
			return err
		}
		if bucket.Get(burnTxHash) != nil {
			return nil
		}
		return bucket.Put(burnTxHash, []byte(signedPaymentMarker))
	})
}

// signedPaymentStore 返回本节点使用的记录存储（测试可注入 n.signedPayments）。
func (n *neutrinoClient) signedPaymentStore() signedPaymentKV {
	if n.signedPayments != nil {
		return n.signedPayments
	}
	return &walletSignedPaymentKV{db: n.neutrinoCfg.Database}
}

// markSignedPaymentInMemory 记录本进程已经为某笔 burn 产出过签名（持久化失败时的兜底）。
func (n *neutrinoClient) markSignedPaymentInMemory(burnTxHash []byte) {
	n.signedPaymentMu.Lock()
	defer n.signedPaymentMu.Unlock()
	if n.signedPaymentMem == nil {
		n.signedPaymentMem = make(map[string]struct{})
	}
	n.signedPaymentMem[string(burnTxHash)] = struct{}{}
}

func (n *neutrinoClient) signedPaymentInMemory(burnTxHash []byte) bool {
	n.signedPaymentMu.Lock()
	defer n.signedPaymentMu.Unlock()
	_, ok := n.signedPaymentMem[string(burnTxHash)]
	return ok
}

// recordSignedPayment 在签名成功之后落盘。**调用方必须在返回/广播签名之前调用它，并传播错误**：
// 写失败时签名不得离开本节点（fail-closed）。
//
// 无论落盘成功与否都会在本进程内留下标记：落盘失败时签名可能已经产生（例如签名节点的分片
// 已通过协议交换），此时至少保证本进程之后拒绝该 burn 的 Unlock。
func (n *neutrinoClient) recordSignedPayment(burnTxHash []byte) error {
	if len(burnTxHash) == 0 {
		return fmt.Errorf("signed payment: empty burn tx hash")
	}
	n.markSignedPaymentInMemory(burnTxHash)
	if err := n.signedPaymentStore().PutOnce(burnTxHash); err != nil {
		return fmt.Errorf("signed payment: persist %s: %w", hex.EncodeToString(burnTxHash), err)
	}
	return nil
}

// hasSignedPayment 报告该 burn 是否已产出过付款签名。
//
// 判据 = 进程内标记 OR 持久记录。读失败一律按"已签名"处理（fail-closed）：
// 无法证明没有签名时，绝不能允许解锁。
func (n *neutrinoClient) hasSignedPayment(burnTxHash []byte) bool {
	if len(burnTxHash) == 0 {
		return true
	}
	if n.signedPaymentInMemory(burnTxHash) {
		return true
	}
	signed, err := n.signedPaymentStore().Has(burnTxHash)
	if err != nil {
		log.Error("hasSignedPayment read failed, treating as signed",
			"burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return true
	}
	return signed
}

// paymentSignatureMayExist 汇总两条"付款签名可能已经存在"的证据：
//   - 新增的持久记录（签名成功即写，覆盖"签名成功但广播失败"的窗口）；
//   - 旧版本的 withdraw state（sent / confirmed，覆盖升级前已经广播过的提现）。
//
// 任一条成立就不允许放弃提现，也不允许该节点再参与 Unlock 组签名。
func paymentSignatureMayExist(signedRecord bool, state []byte) bool {
	return signedRecord || withdrawHasPaymentSignature(state)
}

// recordSignerPayment 是**每个参与付款签名的节点**（协调者与签名节点）在签名成功之后、
// 把结果视为可用之前必须通过的落盘关卡。写失败 ⇒ 返回错误，调用方不得报告签名成功。
//
// 为什么每个节点都要写：解锁的安全性来自门限交集——付款签名用了 t 个节点，解锁也要 t 个节点，
// n=4/t=3 时两个集合至少交 2。只有交集中的节点各自持有"我签过这笔付款"的证据并在 Unlock 轮次
// 拒签，解锁才凑不齐门限。只写在协调者一处时，协调者丢库/换机/作恶即可绕过。
func (t *tssService) recordSignerPayment(burnTxHash []byte) error {
	if t == nil || t.client == nil {
		return fmt.Errorf("signed payment: no bridge to persist the record")
	}
	if err := t.client.recordSignedPayment(burnTxHash); err != nil {
		return fmt.Errorf("signed payment: refuse to release the signing result: %w", err)
	}
	return nil
}

// signAndRecordOnParticipant 执行一轮签名，并在成功之后、返回成功之前落盘记录。
// 签名失败不写记录；落盘失败则整个调用失败（调用方不得把签名结果当成可用）。
func (t *tssService) signAndRecordOnParticipant(burnTxHash []byte, sign func() error) error {
	if err := sign(); err != nil {
		return err
	}
	return t.recordSignerPayment(burnTxHash)
}

// ensureNoPaymentSignature 是签名节点在 Unlock 轮次的门：本节点只要产出过该 burn 的付款签名，
// 就拒绝参与解锁组签名（否则门限交集安全性被破坏，可能双花）。
func (t *tssService) ensureNoPaymentSignature(burnTxHash []byte) error {
	if t == nil || t.client == nil {
		return fmt.Errorf("unlock sign refused: no bridge to read the signed-payment record")
	}
	if paymentSignatureMayExist(t.client.hasSignedPayment(burnTxHash), t.client.getWithdrawState(burnTxHash)) {
		return fmt.Errorf("unlock sign refused: this node already produced a payment signature for %s",
			hex.EncodeToString(burnTxHash))
	}
	return nil
}
