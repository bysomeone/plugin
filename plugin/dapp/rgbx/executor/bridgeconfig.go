package executor

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	manager "github.com/33cn/chain33/system/dapp/manage/types"
	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/golang/protobuf/proto"
)

/*
 * Governance-driven bridge configuration.
 *
 * The executor itself never decides who may change the bridge limits. A change is a two-step
 * process mirrored from the manage executor's apply/approve flow:
 *
 *  1. bridgeConfigApply stores a complete snapshot as a pending record keyed by the apply
 *     transaction hash. The proposer then references that id in an autonomy item.
 *  2. bridgeConfigApprove asks the on-chain autonomy executor whether (itemId, applyId) was
 *     approved. Only then is the snapshot promoted to the effective config state.
 *
 * Effective config lives in consensus state, so every node reaches the same verdict for a
 * given block. When no config was ever approved the bridge is unlimited (all switches off),
 * which keeps the existing behaviour unchanged.
 */

const (
	// bridgeConfigKeyPrefix is the effective (approved) config key prefix.
	// The global record has the fixed suffix "global"; per-asset records use the normalized
	// (upper-case) symbol. Symbols are validated to be ASCII and normalized with
	// formatSymbol, so a symbol can never collide with the lower-case "global" suffix.
	bridgeConfigKeyPrefix    = KeyPrefixStateDB + "bridge-config-"
	bridgeConfigGlobalSuffix = "global"

	// bridgeConfigApplyKeyPrefix prefixes the pending apply records: key = prefix + apply id,
	// where apply id is the hex form of the apply transaction hash.
	bridgeConfigApplyKeyPrefix = KeyPrefixStateDB + "bridge-config-apply-"

	// bridgeWindowKeyPrefix prefixes the rolling window counters used by the numeric limits.
	bridgeWindowKeyPrefix = KeyPrefixStateDB + "bridge-window-"

	// Apply record status values.
	bridgeConfigStatusApply    = int32(1)
	bridgeConfigStatusApproved = int32(2)
)

// Window sizes are derived from the chain's block interval, never from wall-clock time:
// the verdict must be reproducible by every node from consensus data alone. The interval
// below is the assumed bridge parachain block interval; change it here if the deployed
// chain runs at a different pace (no state migration is needed, counters are per bucket).
//
// The 1h window is the current hourly bucket. The 24h window is a rolling sum of the last
// 24 hourly buckets, so it slides forward every hour. Buckets reset on their boundary; a
// true per-block sliding sum would need an unbounded number of state reads.
const (
	assumedBlockIntervalSeconds = int64(6)
	blocksPerHour               = 3600 / assumedBlockIntervalSeconds
	// h24WindowBuckets is how many hourly buckets make up the 24h rolling window.
	h24WindowBuckets = int64(24)
)

// bridgeWindowKind distinguishes the two rate-limited directions.
type bridgeWindowKind byte

const (
	bridgeWindowWithdraw bridgeWindowKind = 'w'
	bridgeWindowMint     bridgeWindowKind = 'm'
)

// bridgeConfigRecord is the pending apply record. It is deliberately defined here (instead
// of in rgbx.proto) because the wire contract for the actions is frozen; the record is
// executor-private state and never crosses the transaction boundary.
type bridgeConfigRecord struct {
	Snapshot []byte `protobuf:"bytes,1,opt,name=snapshot,proto3"`
	Status   int32  `protobuf:"varint,2,opt,name=status,proto3"`
	Proposer string `protobuf:"bytes,3,opt,name=proposer,proto3"`
	Height   int64  `protobuf:"varint,4,opt,name=height,proto3"`
}

func (m *bridgeConfigRecord) Reset()         { *m = bridgeConfigRecord{} }
func (m *bridgeConfigRecord) String() string { return proto.CompactTextString(m) }
func (*bridgeConfigRecord) ProtoMessage()    {}

// formatBridgeConfigKey returns the effective config key. An empty symbol addresses the
// global record (master switch + pause flags); anything else addresses that asset.
func formatBridgeConfigKey(symbol string) []byte {
	if symbol == "" {
		return []byte(bridgeConfigKeyPrefix + bridgeConfigGlobalSuffix)
	}
	return []byte(bridgeConfigKeyPrefix + formatSymbol(symbol))
}

func formatBridgeConfigApplyKey(id string) []byte {
	return []byte(bridgeConfigApplyKeyPrefix + strings.ToLower(id))
}

// formatBridgeWindowKey indexes one hourly counter bucket for (symbol, direction).
func formatBridgeWindowKey(symbol string, kind bridgeWindowKind, bucket int64) []byte {
	return []byte(fmt.Sprintf("%s%s-%c-%020d", bridgeWindowKeyPrefix, formatSymbol(symbol), byte(kind), bucket))
}

// bridgeWindowBucket maps a block height onto an hourly bucket.
func bridgeWindowBucket(height int64) int64 {
	if height < 0 {
		return 0
	}
	return height / blocksPerHour
}

// loadBridgeConfig reads an effective config record. A missing record is reported as
// (nil, nil): "not configured" means "no restriction", not an error.
func (r *rgbx) loadBridgeConfig(symbol string) (*rtypes.BridgeConfigApply, error) {
	if r.GetStateDB() == nil {
		return nil, nil
	}
	value, err := r.GetStateDB().Get(formatBridgeConfigKey(symbol))
	if errors.Is(err, types.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := &rtypes.BridgeConfigApply{}
	if err = types.Decode(value, cfg); err != nil {
		elog.Error("loadBridgeConfig decode", "symbol", symbol, "err", err)
		return nil, err
	}
	return cfg, nil
}

// loadBridgeWindowCounter reads one hourly bucket; a missing bucket counts as zero.
func (r *rgbx) loadBridgeWindowCounter(symbol string, kind bridgeWindowKind, bucket int64) (int64, error) {
	if r.GetStateDB() == nil {
		return 0, nil
	}
	value, err := r.GetStateDB().Get(formatBridgeWindowKey(symbol, kind, bucket))
	if errors.Is(err, types.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	counter := &types.Int64{}
	if err = types.Decode(value, counter); err != nil {
		elog.Error("loadBridgeWindowCounter decode", "symbol", symbol, "bucket", bucket, "err", err)
		return 0, err
	}
	return counter.GetData(), nil
}

// bumpBridgeWindowCounter returns the receipt KV that adds amount to the current bucket.
func (r *rgbx) bumpBridgeWindowCounter(symbol string, kind bridgeWindowKind, amount int64) (*types.KeyValue, error) {
	bucket := bridgeWindowBucket(r.GetHeight())
	current, err := r.loadBridgeWindowCounter(symbol, kind, bucket)
	if err != nil {
		return nil, err
	}
	return &types.KeyValue{
		Key:   formatBridgeWindowKey(symbol, kind, bucket),
		Value: types.Encode(&types.Int64{Data: current + amount}),
	}, nil
}

// exceedsLimit reports whether existing+amount goes over limit. limit <= 0 means unlimited.
// The comparison is written to avoid overflowing int64 for absurd accumulations.
func exceedsLimit(existing, amount, limit int64) bool {
	if limit <= 0 {
		return false
	}
	if existing >= limit {
		return amount > 0
	}
	return amount > limit-existing
}

// checkBridgeConfigApply validates the payload shape and the referenced asset.
//
// An empty asset symbol is the global record: it may only carry the switches and the
// large-withdrawal parameters, so the numeric per-asset limits must be zero. A non-empty
// symbol must name an already registered cross-chain asset (CommitDKG).
func (r *rgbx) checkBridgeConfigApply(apply *rtypes.BridgeConfigApply) error {
	if apply == nil {
		return ErrInvalidBridgeConfig
	}
	if apply.GetMaxPerTx() < 0 || apply.GetMaxPerWindow1H() < 0 || apply.GetMaxPerWindow24H() < 0 ||
		apply.GetMaxMintPerWindow() < 0 || apply.GetLargeWithdrawThreshold() < 0 ||
		apply.GetLargeWithdrawDelayBlocks() < 0 {
		elog.Error("checkBridgeConfigApply negative limit", "symbol", apply.GetAssetSymbol())
		return ErrInvalidBridgeConfig
	}
	if apply.GetAssetSymbol() == "" {
		if apply.GetMaxPerTx() != 0 || apply.GetMaxPerWindow1H() != 0 ||
			apply.GetMaxPerWindow24H() != 0 || apply.GetMaxMintPerWindow() != 0 {
			elog.Error("checkBridgeConfigApply global config with numeric limits",
				"maxPerTx", apply.GetMaxPerTx(), "maxPerWindow1h", apply.GetMaxPerWindow1H(),
				"maxPerWindow24h", apply.GetMaxPerWindow24H(), "maxMintPerWindow", apply.GetMaxMintPerWindow())
			return ErrInvalidBridgeConfig
		}
		return nil
	}
	if !isValidSymbolCharset(apply.GetAssetSymbol()) {
		elog.Error("checkBridgeConfigApply invalid symbol", "symbol", apply.GetAssetSymbol())
		return ErrInvalidAssetSymbol
	}
	if _, err := r.getCrossChainInfo(apply.GetAssetSymbol()); err != nil {
		elog.Error("checkBridgeConfigApply asset not registered", "symbol", apply.GetAssetSymbol(), "err", err)
		return ErrGetCrossChainInfo
	}
	return nil
}

// checkBridgeConfigApprove performs the cheap payload checks. The autonomy query and the
// record status check happen in Exec (they read state that may change between CheckTx and
// block execution).
func (r *rgbx) checkBridgeConfigApprove(approve *rtypes.BridgeConfigApprove) error {
	if approve == nil || approve.GetApplyConfigId() == "" || approve.GetAutonomyItemId() == "" {
		return ErrInvalidBridgeConfig
	}
	return nil
}

// normalizeBridgeConfigApplyID validates an apply id (hex, optional 0x prefix) and returns
// its canonical lower-case hex form, which is also the apply record key suffix.
func normalizeBridgeConfigApplyID(id string) (string, error) {
	id = strings.TrimPrefix(strings.TrimSpace(id), "0x")
	if id == "" {
		return "", ErrInvalidBridgeConfig
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", ErrInvalidBridgeConfig
	}
	return strings.ToLower(id), nil
}

// Exec_BridgeConfigApply stores a complete snapshot as a pending record keyed by this
// transaction hash. Nothing becomes effective here; bridgeConfigApprove is required.
func (r *rgbx) Exec_BridgeConfigApply(apply *rtypes.BridgeConfigApply, tx *types.Transaction, _ int) (*types.Receipt, error) {
	if err := r.checkBridgeConfigApply(apply); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(tx.Hash())
	key := formatBridgeConfigApplyKey(id)
	if r.GetStateDB() != nil {
		if _, err := r.GetStateDB().Get(key); !errors.Is(err, types.ErrNotFound) {
			elog.Error("Exec_BridgeConfigApply duplicate apply record", "applyId", id, "err", err)
			return nil, ErrBridgeConfigApplyExists
		}
	}
	record := &bridgeConfigRecord{
		Snapshot: types.Encode(apply),
		Status:   bridgeConfigStatusApply,
		Proposer: tx.From(),
		Height:   r.GetHeight(),
	}
	elog.Info("Exec_BridgeConfigApply pending", "applyId", id, "symbol", apply.GetAssetSymbol(),
		"proposer", tx.From(), "height", r.GetHeight())
	return &types.Receipt{
		Ty: types.ExecOk,
		KV: []*types.KeyValue{{Key: key, Value: types.Encode(record)}},
	}, nil
}

// Exec_BridgeConfigApprove queries the autonomy executor for an approved (itemId, applyId)
// pair and, only on success, promotes the stored snapshot to the effective config state.
func (r *rgbx) Exec_BridgeConfigApprove(approve *rtypes.BridgeConfigApprove, _ *types.Transaction, _ int) (*types.Receipt, error) {
	if err := r.checkBridgeConfigApprove(approve); err != nil {
		return nil, err
	}
	id, err := normalizeBridgeConfigApplyID(approve.GetApplyConfigId())
	if err != nil {
		elog.Error("Exec_BridgeConfigApprove invalid apply id", "applyId", approve.GetApplyConfigId())
		return nil, err
	}
	recordKey := formatBridgeConfigApplyKey(id)
	if r.GetStateDB() == nil {
		return nil, ErrBridgeConfigApplyNotExist
	}
	value, err := r.GetStateDB().Get(recordKey)
	if errors.Is(err, types.ErrNotFound) {
		elog.Error("Exec_BridgeConfigApprove apply record not found", "applyId", id)
		return nil, ErrBridgeConfigApplyNotExist
	}
	if err != nil {
		elog.Error("Exec_BridgeConfigApprove read apply record", "applyId", id, "err", err)
		return nil, err
	}
	record := &bridgeConfigRecord{}
	if err = types.Decode(value, record); err != nil {
		elog.Error("Exec_BridgeConfigApprove decode apply record", "applyId", id, "err", err)
		return nil, err
	}
	if record.Status != bridgeConfigStatusApply {
		elog.Error("Exec_BridgeConfigApprove apply record not pending", "applyId", id, "status", record.Status)
		return nil, ErrBridgeConfigNotPending
	}
	cfg := r.GetAPI().GetConfig()
	autonomyExec := types.ConfSub(cfg, manager.ManageX).GStr(types.AutonomyCfgKey)
	if autonomyExec == "" {
		elog.Error("Exec_BridgeConfigApprove autonomy exec not configured", "applyId", id)
		return nil, types.ErrNotFound
	}
	// Same query shape as chain33's manage executor: success means the item was approved.
	if _, err = r.GetAPI().QueryChain(&types.ChainExecutor{
		Driver:   autonomyExec,
		FuncName: "IsAutonomyApprovedItem",
		Param:    types.Encode(&types.ReqMultiStrings{Datas: []string{approve.GetAutonomyItemId(), id}}),
	}); err != nil {
		elog.Error("Exec_BridgeConfigApprove autonomy query", "applyId", id,
			"itemId", approve.GetAutonomyItemId(), "err", err)
		return nil, err
	}
	snapshot := &rtypes.BridgeConfigApply{}
	if err = types.Decode(record.Snapshot, snapshot); err != nil {
		elog.Error("Exec_BridgeConfigApprove decode snapshot", "applyId", id, "err", err)
		return nil, err
	}
	// The snapshot was validated at apply time; re-run the shape check so a corrupt record
	// can never promote a malformed config (cheap, and it keeps the invariant local).
	if err = r.checkBridgeConfigApply(snapshot); err != nil {
		return nil, err
	}
	record.Status = bridgeConfigStatusApproved
	elog.Info("Exec_BridgeConfigApprove approved", "applyId", id, "itemId", approve.GetAutonomyItemId(),
		"symbol", snapshot.GetAssetSymbol(), "limitsEnabled", snapshot.GetLimitsEnabled(),
		"pauseDeposit", snapshot.GetPauseDeposit(), "pauseWithdraw", snapshot.GetPauseWithdraw())
	return &types.Receipt{
		Ty: types.ExecOk,
		KV: []*types.KeyValue{
			{Key: recordKey, Value: types.Encode(record)},
			{Key: formatBridgeConfigKey(snapshot.GetAssetSymbol()), Value: types.Encode(snapshot)},
		},
	}, nil
}

// numericLimitsEnabled decides whether an asset's numeric limits apply. The asset record's
// own switch must be on; if a global record exists, its switch is the master kill switch.
// A missing global record is not "switched off" — it simply was never configured — so an
// asset record can carry limits on its own.
func numericLimitsEnabled(global, cfg *rtypes.BridgeConfigApply) bool {
	if cfg == nil || !cfg.GetLimitsEnabled() {
		return false
	}
	return global == nil || global.GetLimitsEnabled()
}

// checkWithdrawBridgeConfig applies the effective bridge config to a Withdraw action.
// Pause flags are independent of the numeric switches.
func (r *rgbx) checkWithdrawBridgeConfig(withdraw *rtypes.WithdrawAsset) error {
	global, err := r.loadBridgeConfig("")
	if err != nil {
		return err
	}
	if global != nil && global.GetPauseWithdraw() {
		elog.Error("checkWithdrawBridgeConfig withdrawals paused")
		return ErrBridgeWithdrawPaused
	}
	symbol := formatSymbol(withdraw.GetAssetSymbol())
	// An empty symbol is the global record itself; it never carries per-asset limits.
	if symbol == "" {
		return nil
	}
	cfg, err := r.loadBridgeConfig(symbol)
	if err != nil {
		return err
	}
	// An emergency brake may be approved globally or for a single asset; either one rejects.
	if cfg != nil && cfg.GetPauseWithdraw() {
		elog.Error("checkWithdrawBridgeConfig asset withdrawals paused", "symbol", symbol)
		return ErrBridgeWithdrawPaused
	}
	if !numericLimitsEnabled(global, cfg) {
		return nil
	}
	amount := withdraw.GetAmount()
	if cfg.GetMaxPerTx() > 0 && amount > cfg.GetMaxPerTx() {
		elog.Error("checkWithdrawBridgeConfig max per tx exceeded", "symbol", symbol,
			"amount", amount, "maxPerTx", cfg.GetMaxPerTx())
		return ErrBridgeWithdrawLimitExceeded
	}
	bucket := bridgeWindowBucket(r.GetHeight())
	if cfg.GetMaxPerWindow1H() > 0 {
		used, err := r.loadBridgeWindowCounter(symbol, bridgeWindowWithdraw, bucket)
		if err != nil {
			return err
		}
		if exceedsLimit(used, amount, cfg.GetMaxPerWindow1H()) {
			elog.Error("checkWithdrawBridgeConfig 1h window exceeded", "symbol", symbol,
				"amount", amount, "used", used, "limit", cfg.GetMaxPerWindow1H())
			return ErrBridgeWithdrawLimitExceeded
		}
	}
	if cfg.GetMaxPerWindow24H() > 0 {
		var used int64
		for i := int64(0); i < h24WindowBuckets; i++ {
			counter, err := r.loadBridgeWindowCounter(symbol, bridgeWindowWithdraw, bucket-i)
			if err != nil {
				return err
			}
			used += counter
		}
		if exceedsLimit(used, amount, cfg.GetMaxPerWindow24H()) {
			elog.Error("checkWithdrawBridgeConfig 24h window exceeded", "symbol", symbol,
				"amount", amount, "used", used, "limit", cfg.GetMaxPerWindow24H())
			return ErrBridgeWithdrawLimitExceeded
		}
	}
	return nil
}

// withdrawWindowKV returns the KV that accounts a successful withdrawal against the rolling
// window, or nil when no window limit is active. Called from Exec_Withdraw after the lock
// transfer succeeded, so the counter and the lock roll back together.
func (r *rgbx) withdrawWindowKV(withdraw *rtypes.WithdrawAsset) (*types.KeyValue, error) {
	global, err := r.loadBridgeConfig("")
	if err != nil {
		return nil, err
	}
	symbol := formatSymbol(withdraw.GetAssetSymbol())
	if symbol == "" {
		return nil, nil
	}
	cfg, err := r.loadBridgeConfig(symbol)
	if err != nil {
		return nil, err
	}
	if !numericLimitsEnabled(global, cfg) {
		return nil, nil
	}
	// maxPerTx needs no counter; only the rolling windows do.
	if cfg.GetMaxPerWindow1H() <= 0 && cfg.GetMaxPerWindow24H() <= 0 {
		return nil, nil
	}
	return r.bumpBridgeWindowCounter(symbol, bridgeWindowWithdraw, withdraw.GetAmount())
}

// checkDepositPaused rejects deposits while the emergency brake is engaged, globally or for
// this asset.
func (r *rgbx) checkDepositPaused(deposit *rtypes.DepositAsset) error {
	global, err := r.loadBridgeConfig("")
	if err != nil {
		return err
	}
	if global != nil && global.GetPauseDeposit() {
		elog.Error("checkDeposit bridge deposits paused")
		return ErrBridgeDepositPaused
	}
	symbol := formatSymbol(deposit.GetAssetSymbol())
	if symbol == "" {
		return nil
	}
	cfg, err := r.loadBridgeConfig(symbol)
	if err != nil {
		return err
	}
	if cfg != nil && cfg.GetPauseDeposit() {
		elog.Error("checkDeposit asset deposits paused", "symbol", symbol)
		return ErrBridgeDepositPaused
	}
	return nil
}

// checkDepositMintLimit enforces maxMintPerWindow for the asset.
func (r *rgbx) checkDepositMintLimit(deposit *rtypes.DepositAsset) error {
	global, err := r.loadBridgeConfig("")
	if err != nil {
		return err
	}
	symbol := formatSymbol(deposit.GetAssetSymbol())
	if symbol == "" {
		return nil
	}
	cfg, err := r.loadBridgeConfig(symbol)
	if err != nil {
		return err
	}
	if !numericLimitsEnabled(global, cfg) || cfg.GetMaxMintPerWindow() <= 0 {
		return nil
	}
	used, err := r.loadBridgeWindowCounter(symbol, bridgeWindowMint, bridgeWindowBucket(r.GetHeight()))
	if err != nil {
		return err
	}
	if exceedsLimit(used, deposit.GetAmount(), cfg.GetMaxMintPerWindow()) {
		elog.Error("checkDeposit mint window exceeded", "symbol", symbol,
			"amount", deposit.GetAmount(), "used", used, "limit", cfg.GetMaxMintPerWindow())
		return ErrBridgeMintLimitExceeded
	}
	return nil
}

// depositWindowKV accounts a successful mint against the rolling mint window, or nil when
// no mint limit is active.
func (r *rgbx) depositWindowKV(deposit *rtypes.DepositAsset) (*types.KeyValue, error) {
	global, err := r.loadBridgeConfig("")
	if err != nil {
		return nil, err
	}
	symbol := formatSymbol(deposit.GetAssetSymbol())
	if symbol == "" {
		return nil, nil
	}
	cfg, err := r.loadBridgeConfig(symbol)
	if err != nil {
		return nil, err
	}
	if !numericLimitsEnabled(global, cfg) || cfg.GetMaxMintPerWindow() <= 0 {
		return nil, nil
	}
	return r.bumpBridgeWindowCounter(symbol, bridgeWindowMint, deposit.GetAmount())
}

// Query_GetBridgeConfig returns the effective config. An empty request symbol addresses the
// global record. A missing record returns an empty config (defaults = unlimited) instead of
// an error, matching GetCrossChainInfo.
func (r *rgbx) Query_GetBridgeConfig(req *types.ReqString) (types.Message, error) {
	symbol := req.GetData()
	cfg, err := r.loadBridgeConfig(symbol)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &rtypes.BridgeConfigApply{AssetSymbol: formatSymbol(symbol)}, nil
	}
	return cfg, nil
}
