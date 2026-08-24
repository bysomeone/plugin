package neutrino

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/33cn/chain33/common/merkle"
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// 以下方法使 neutrinoClient 实现 rgb20.Chain33Bridge（充值侧）。

// GetMainchainHeight 返回主链最新高度。
func (n *neutrinoClient) GetMainchainHeight() int64 {
	return n.getMainchainHeight()
}

// GetBtcTipHeight 返回本地 lightclient 已知的 BTC 链尖高度。
func (n *neutrinoClient) GetBtcTipHeight() int64 {
	return int64(n.getBestBlockHeight())
}

// BuildSpvProof 构造 RGB 充值付款交易的存在性证明（SPV，对 lightclient 头）。
// 优先用 BTC 全节点 RPC（GetRawTransactionVerbose 定位区块），否则回退到钱包 pending 缓存。
func (n *neutrinoClient) BuildSpvProof(txid string) (*rgb20.SpvProof, error) {
	if txid == "" {
		return nil, fmt.Errorf("empty txid")
	}
	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return nil, fmt.Errorf("invalid txid: %w", err)
	}
	// 1) 优先：全节点 RPC 定位交易所在区块。
	if n.bw != nil && n.bw.rpcClient != nil {
		if spv, err := n.buildSpvFromRPC(hash); err == nil {
			return spv, nil
		}
	}
	// 2) 回退：钱包 pending 缓存（充值交易若被跟踪）。
	if n.bw != nil {
		if pending, ok := n.bw.pendingTxs[*hash]; ok && pending.tx != nil {
			return n.buildSpvFromPending(pending)
		}
	}
	return nil, fmt.Errorf("build spv proof: rgb deposit tx %s not found (full-node RPC or pending cache required)", txid)
}

func (n *neutrinoClient) buildSpvFromRPC(txHash *chainhash.Hash) (*rgb20.SpvProof, error) {
	verboseTx, err := n.bw.rpcClient.GetRawTransactionVerbose(txHash)
	if err != nil {
		return nil, err
	}
	if verboseTx.BlockHash == "" {
		return nil, fmt.Errorf("tx not confirmed")
	}
	blockHash, err := chainhash.NewHashFromStr(verboseTx.BlockHash)
	if err != nil {
		return nil, err
	}
	block, err := n.bw.rpcClient.GetBlockVerbose(blockHash)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(verboseTx.Hex)
	if err != nil {
		return nil, err
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSizeStripped()))
	if err := tx.SerializeNoWitness(buf); err != nil {
		return nil, err
	}
	txs, txIndex, err := buildTxHashesFromVerbose(block.Tx, txHash.String())
	if err != nil {
		return nil, err
	}
	spv := buildBtcSpv(txHash.String(), verboseTx.BlockHash, verboseTx.Time, uint64(block.Height), txs, txIndex)
	return &rgb20.SpvProof{
		TxData:      buf.Bytes(),
		BlockHash:   verboseTx.BlockHash,
		BlockHeight: uint64(block.Height),
		TxIndex:     txIndex,
		MerkleProof: spv.BranchProof,
	}, nil
}

func (n *neutrinoClient) buildSpvFromPending(pending *btcPendingTx) (*rgb20.SpvProof, error) {
	spv, err := n.bw.buildTxExistenceProof(pending)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(make([]byte, 0, pending.tx.SerializeSizeStripped()))
	if err := pending.tx.SerializeNoWitness(buf); err != nil {
		return nil, err
	}
	return &rgb20.SpvProof{
		TxData:      buf.Bytes(),
		BlockHash:   pending.blockHash.String(),
		BlockHeight: uint64(pending.blockHeight),
		TxIndex:     spv.GetTxIndex(),
		MerkleProof: spv.GetBranchProof(),
	}, nil
}

// VerifyDepositSpv 签名节点独立验证充值 SPV 证明（对 lightclient 头）。
// 与 rgbx 执行器 validateBtcTxProof 的校验口径一致，供签名节点在签 C 前确认付款交易上链。
func (n *neutrinoClient) VerifyDepositSpv(proof *rtypes.BtcTxProof) error {
	if proof == nil || len(proof.GetTxData()) == 0 {
		return fmt.Errorf("empty spv proof")
	}
	var btcTx wire.MsgTx
	if err := btcTx.DeserializeNoWitness(bytes.NewReader(proof.GetTxData())); err != nil {
		return fmt.Errorf("decode spv tx: %w", err)
	}
	header, err := n.getLightBtcHeader(proof.GetBlockHeight())
	if err != nil {
		return err
	}
	if header.GetHash() != proof.GetBlockHash() || header.GetHeight() != proof.GetBlockHeight() {
		return fmt.Errorf("spv header mismatch: expect hash=%s height=%d", proof.GetBlockHash(), proof.GetBlockHeight())
	}
	txID := btcTx.TxHash()
	merkleRoot := merkle.GetMerkleRootFromBranch(proof.GetMerkleProof(), txID.CloneBytes(), proof.GetTxIndex())
	headerMerkleRoot, err := chainhash.NewHashFromStr(header.GetMerkleRoot())
	if err != nil {
		return fmt.Errorf("invalid header merkle root: %w", err)
	}
	if !bytes.Equal(merkleRoot, headerMerkleRoot.CloneBytes()) {
		return fmt.Errorf("spv merkle root mismatch")
	}
	return nil
}

// getLightBtcHeader 从 lightclient 合约查询指定高度 BTC 头。
func (n *neutrinoClient) getLightBtcHeader(height uint64) (*ltypes.BtcHeader, error) {
	reply, err := n.mainChainGrpc.QueryChain(n.ctx, &types.ChainExecutor{
		Driver:   ltypes.LightclientX,
		FuncName: "GetBtcHeader",
		Param:    types.Encode(&ltypes.ReqGetBtcHeader{Height: height}),
	})
	if err != nil {
		return nil, err
	}
	header := &ltypes.BtcHeader{}
	if err := types.Decode(reply.GetMsg(), header); err != nil {
		return nil, err
	}
	return header, nil
}

// SubmitDeposit 提交 rgbx Deposit 交易（RGB20 分支由合约验 threshold_sig 后铸造）。
func (n *neutrinoClient) SubmitDeposit(dep *rtypes.DepositAsset) error {
	_, err := n.submitMainchainTx(rtypes.RgbxX, rtypes.NameDepositAssetAction, dep)
	return err
}

// SignDepositMessage 执行 rgb20-deposit TSS 签名轮次，返回阈值签名（DER）。
func (n *neutrinoClient) SignDepositMessage(payload *rgb20.DepositSignPayload) ([]byte, error) {
	return n.tss.processSignRgb20Deposit(payload)
}
