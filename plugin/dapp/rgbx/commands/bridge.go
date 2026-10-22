package commands

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/spf13/cobra"
)

// createTxRPC is the seam used by the commands in this file. It is sendCreateTxRPC in
// production; tests replace it to observe the constructed action without a running node.
var createTxRPC = sendCreateTxRPC

// bridgeConfigFieldFlags maps each CLI flag to the proto field name accepted by the apply
// field mask. The mask names come from rtypes (the same constants the executor uses), so the
// CLI cannot drift from the on-chain contract.
var bridgeConfigFieldFlags = []struct {
	flag  string
	field string
}{
	{"limits-enabled", rtypes.BridgeConfigFieldLimitsEnabled},
	{"max-per-tx", rtypes.BridgeConfigFieldMaxPerTx},
	{"max-per-window-1h", rtypes.BridgeConfigFieldMaxPerWindow1H},
	{"max-per-window-24h", rtypes.BridgeConfigFieldMaxPerWindow24H},
	{"max-mint-per-window", rtypes.BridgeConfigFieldMaxMintPerWindow},
	{"large-deposit-threshold", rtypes.BridgeConfigFieldLargeDepositThreshold},
	{"large-deposit-extra-confirmations", rtypes.BridgeConfigFieldLargeDepositExtraConfirmations},
	{"pause-deposit", rtypes.BridgeConfigFieldPauseDeposit},
	{"pause-withdraw", rtypes.BridgeConfigFieldPauseWithdraw},
	{"withdraw-max-fee-rate-sat-per-vbyte", rtypes.BridgeConfigFieldWithdrawMaxFeeRateSatPerVbyte},
	{"rgb20-withdraw-fee-lock-vbytes", rtypes.BridgeConfigFieldRgb20WithdrawFeeLockVbytes},
	{"rgb20-withdraw-fee", rtypes.BridgeConfigFieldRgb20WithdrawFee},
}

// bridgeConfigFieldNames returns the mask names in the order of bridgeConfigFieldFlags,
// used in help text so the documented list cannot drift from the accepted one.
func bridgeConfigFieldNames() string {
	names := make([]string, 0, len(bridgeConfigFieldFlags))
	for _, item := range bridgeConfigFieldFlags {
		names = append(names, item.field)
	}
	return strings.Join(names, ", ")
}

func bridgeConfigApplyCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bridge-config-apply",
		Short: "propose a bridge configuration change (step 1 of 2)",
		Run:   bridgeConfigApply,
		Example: "bridge-config-apply --asset-symbol XBTC --max-per-tx 100000000 --fields maxPerTx\n" +
			"  bridge-config-apply --pause-deposit=true --fields pauseDeposit\n" +
			"  bridge-config-apply --asset-symbol USDT --rgb20-withdraw-fee USDT=1000000 --fields rgb20WithdrawFee",
	}
	bridgeConfigApplyFlags(cmd)
	return cmd
}

func bridgeConfigApplyFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.String("asset-symbol", "", "asset symbol; empty addresses the global config (switches and large-deposit knobs only)")
	flags.Bool("limits-enabled", false, "master switch for the numeric limits")
	flags.Int64("max-per-tx", 0, "per-transaction limit in the asset's minimum unit (0 = unlimited)")
	flags.Int64("max-per-window-1h", 0, "per-1h-window limit in the asset's minimum unit (0 = unlimited)")
	flags.Int64("max-per-window-24h", 0, "per-24h-window limit in the asset's minimum unit (0 = unlimited)")
	flags.Int64("max-mint-per-window", 0, "per-window mint limit in the asset's minimum unit (0 = unlimited)")
	flags.Int64("large-deposit-threshold", 0,
		"deposit amount (minimum unit) at or above which extra BTC confirmations are required (0 = disabled)")
	flags.Int64("large-deposit-extra-confirmations", 0,
		"extra BTC confirmations required for large deposits (added on top of the base confirmation count)")
	flags.Bool("pause-deposit", false, "pause deposits")
	flags.Bool("pause-withdraw", false, "pause withdrawals")
	flags.Uint32("withdraw-max-fee-rate-sat-per-vbyte", 0,
		"cap on the BTC miner fee rate in sat/vB for BTC-paying withdrawals (0 = not configured)")
	flags.Uint32("rgb20-withdraw-fee-lock-vbytes", 0,
		"conservative RGB20 anchor transaction size in vB used to cap the locked xBTC (0 = not configured)")
	flags.StringArray("rgb20-withdraw-fee", nil,
		"fixed RGB20 withdrawal fee as SYMBOL=AMOUNT, AMOUNT in the asset's minimum unit; repeatable")
	flags.StringSlice("fields", nil,
		"field mask of proto field names to update; empty means a full snapshot. "+
			"Valid names: "+bridgeConfigFieldNames()+". "+
			"Required when only some field flags are given, otherwise the unnamed fields would be reset to zero")
}

// buildBridgeConfigApply validates the flags and returns the action payload. It is a pure
// function (no RPC) so the masking rules can be tested directly.
func buildBridgeConfigApply(cmd *cobra.Command) (*rtypes.BridgeConfigApply, error) {
	flags := cmd.Flags()
	assetSymbol, _ := flags.GetString("asset-symbol")
	limitsEnabled, _ := flags.GetBool("limits-enabled")
	maxPerTx, _ := flags.GetInt64("max-per-tx")
	maxPerWindow1H, _ := flags.GetInt64("max-per-window-1h")
	maxPerWindow24H, _ := flags.GetInt64("max-per-window-24h")
	maxMintPerWindow, _ := flags.GetInt64("max-mint-per-window")
	largeDepositThreshold, _ := flags.GetInt64("large-deposit-threshold")
	largeDepositExtraConfirmations, _ := flags.GetInt64("large-deposit-extra-confirmations")
	pauseDeposit, _ := flags.GetBool("pause-deposit")
	pauseWithdraw, _ := flags.GetBool("pause-withdraw")
	withdrawMaxFeeRate, _ := flags.GetUint32("withdraw-max-fee-rate-sat-per-vbyte")
	rgb20WithdrawFeeLockVbytes, _ := flags.GetUint32("rgb20-withdraw-fee-lock-vbytes")
	rgb20WithdrawFeeRaw, _ := flags.GetStringArray("rgb20-withdraw-fee")
	fieldsRaw, _ := flags.GetStringSlice("fields")

	fields, err := normalizeBridgeConfigFields(fieldsRaw)
	if err != nil {
		return nil, err
	}

	// No mask means "full snapshot": everything not given is written as zero. A strict
	// subset of field flags under that rule would silently clear the rest, so require the
	// mask unless every field flag was given explicitly (or none at all).
	if len(fields) == 0 {
		changed := changedBridgeConfigFieldFlags(cmd)
		if len(changed) > 0 && len(changed) < len(bridgeConfigFieldFlags) {
			return nil, fmt.Errorf("--fields is required when setting only some fields (given: %s); "+
				"without a mask the apply is a full snapshot and would reset every other field to zero",
				strings.Join(changed, ", "))
		}
	}

	for _, item := range []struct {
		name  string
		value int64
	}{
		{"max-per-tx", maxPerTx},
		{"max-per-window-1h", maxPerWindow1H},
		{"max-per-window-24h", maxPerWindow24H},
		{"max-mint-per-window", maxMintPerWindow},
		{"large-deposit-threshold", largeDepositThreshold},
		{"large-deposit-extra-confirmations", largeDepositExtraConfirmations},
	} {
		if item.value < 0 {
			return nil, fmt.Errorf("--%s must not be negative", item.name)
		}
	}

	rgb20WithdrawFee, err := parseRgb20WithdrawFees(rgb20WithdrawFeeRaw)
	if err != nil {
		return nil, err
	}

	return &rtypes.BridgeConfigApply{
		AssetSymbol:                    assetSymbol,
		LimitsEnabled:                  limitsEnabled,
		MaxPerTx:                       maxPerTx,
		MaxPerWindow1H:                 maxPerWindow1H,
		MaxPerWindow24H:                maxPerWindow24H,
		MaxMintPerWindow:               maxMintPerWindow,
		LargeDepositThreshold:          largeDepositThreshold,
		LargeDepositExtraConfirmations: largeDepositExtraConfirmations,
		PauseDeposit:                   pauseDeposit,
		PauseWithdraw:                  pauseWithdraw,
		WithdrawMaxFeeRateSatPerVbyte:  withdrawMaxFeeRate,
		Rgb20WithdrawFeeLockVbytes:     rgb20WithdrawFeeLockVbytes,
		Rgb20WithdrawFee:               rgb20WithdrawFee,
		Fields:                         fields,
	}, nil
}

func bridgeConfigApply(cmd *cobra.Command, _ []string) {
	apply, err := buildBridgeConfigApply(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%v\n", err)
		return
	}
	createTxRPC(cmd, rtypes.NameBridgeConfigApplyAction, apply)
}

// normalizeBridgeConfigFields trims the mask, drops empty entries and rejects names the
// executor would reject anyway, so a typo fails before a transaction is built.
func normalizeBridgeConfigFields(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	known := make(map[string]struct{}, len(bridgeConfigFieldFlags))
	for _, item := range bridgeConfigFieldFlags {
		known[item.field] = struct{}{}
	}
	fields := make([]string, 0, len(raw))
	for _, field := range raw {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if _, ok := known[field]; !ok {
			return nil, fmt.Errorf("unknown --fields value %q, valid names: %s",
				field, bridgeConfigFieldNames())
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// changedBridgeConfigFieldFlags returns the CLI flags for fields the caller set explicitly.
func changedBridgeConfigFieldFlags(cmd *cobra.Command) []string {
	var changed []string
	for _, item := range bridgeConfigFieldFlags {
		if cmd.Flags().Changed(item.flag) {
			changed = append(changed, "--"+item.flag)
		}
	}
	return changed
}

// parseRgb20WithdrawFees parses repeated SYMBOL=AMOUNT entries. AMOUNT is an integer in the
// asset's minimum unit; duplicates are rejected case-insensitively, matching the executor's
// normalized-symbol check.
func parseRgb20WithdrawFees(raw []string) ([]*rtypes.Rgb20WithdrawFee, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	fees := make([]*rtypes.Rgb20WithdrawFee, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("empty --rgb20-withdraw-fee entry, want SYMBOL=AMOUNT")
		}
		symbol, amountStr, ok := strings.Cut(entry, "=")
		symbol = strings.TrimSpace(symbol)
		amountStr = strings.TrimSpace(amountStr)
		if !ok || symbol == "" || amountStr == "" {
			return nil, fmt.Errorf("invalid --rgb20-withdraw-fee %q: want SYMBOL=AMOUNT", entry)
		}
		amount, err := strconv.ParseInt(amountStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid amount in --rgb20-withdraw-fee %q: "+
				"want an integer in the asset's minimum unit", entry)
		}
		if amount < 0 {
			return nil, fmt.Errorf("invalid amount in --rgb20-withdraw-fee %q: must not be negative", entry)
		}
		normalized := strings.ToUpper(symbol)
		if _, dup := seen[normalized]; dup {
			return nil, fmt.Errorf("duplicate --rgb20-withdraw-fee symbol %s", symbol)
		}
		seen[normalized] = struct{}{}
		fees = append(fees, &rtypes.Rgb20WithdrawFee{Symbol: symbol, FeeMinUnits: amount})
	}
	return fees, nil
}

func bridgeConfigApproveCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "bridge-config-approve",
		Short:   "activate an approved bridge configuration change (step 2 of 2)",
		Run:     bridgeConfigApprove,
		Example: "bridge-config-approve --apply-id <applyTxHashHex> --item-id <autonomyItemId>",
	}
	cmd.Flags().String("apply-id", "", "apply transaction hash, hex (0x prefix tolerated)")
	cmd.Flags().String("item-id", "", "autonomy governance item id that approved the apply")
	markRequired(cmd, "apply-id", "item-id")
	return cmd
}

func buildBridgeConfigApprove(cmd *cobra.Command) (*rtypes.BridgeConfigApprove, error) {
	flags := cmd.Flags()
	applyID, _ := flags.GetString("apply-id")
	itemID, _ := flags.GetString("item-id")
	applyID = strings.TrimSpace(applyID)
	itemID = strings.TrimSpace(itemID)
	if applyID == "" {
		return nil, fmt.Errorf("--apply-id is required")
	}
	if _, err := decodeHexAuto(applyID); err != nil {
		return nil, fmt.Errorf("invalid --apply-id %q: %w", applyID, err)
	}
	if itemID == "" {
		return nil, fmt.Errorf("--item-id is required")
	}
	return &rtypes.BridgeConfigApprove{ApplyConfigId: applyID, AutonomyItemId: itemID}, nil
}

func bridgeConfigApprove(cmd *cobra.Command, _ []string) {
	approve, err := buildBridgeConfigApprove(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%v\n", err)
		return
	}
	createTxRPC(cmd, rtypes.NameBridgeConfigApproveAction, approve)
}

func abandonWithdrawCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "abandon-withdraw",
		Short: "record a proposal to abandon an unsettled withdrawal",
		Run:   abandonWithdraw,
		Example: "abandon-withdraw --burn-tx-hash <withdrawTxHashHex> " +
			"--owner <refundAddress> --reason \"stuck for too long\"",
	}
	cmd.Flags().String("burn-tx-hash", "",
		"chain33 hash of the Withdraw (burn/lock) transaction, hex (0x prefix tolerated)")
	cmd.Flags().String("owner", "",
		"refund recipient recorded in the burn ledger; must match it exactly (typo protection)")
	cmd.Flags().String("reason", "", "free-form reason recorded on chain")
	markRequired(cmd, "burn-tx-hash", "owner")
	return cmd
}

func buildAbandonWithdraw(cmd *cobra.Command) (*rtypes.AbandonWithdraw, error) {
	flags := cmd.Flags()
	burnTxHashHex, _ := flags.GetString("burn-tx-hash")
	owner, _ := flags.GetString("owner")
	reason, _ := flags.GetString("reason")

	burnTxHash, err := decodeHexAuto(burnTxHashHex)
	if err != nil {
		return nil, fmt.Errorf("invalid --burn-tx-hash %q: %w", burnTxHashHex, err)
	}
	if len(burnTxHash) == 0 {
		return nil, fmt.Errorf("--burn-tx-hash is required")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, fmt.Errorf("--owner is required")
	}
	return &rtypes.AbandonWithdraw{BurnTxHash: burnTxHash, Owner: owner, Reason: reason}, nil
}

func abandonWithdraw(cmd *cobra.Command, _ []string) {
	abandon, err := buildAbandonWithdraw(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%v\n", err)
		return
	}
	createTxRPC(cmd, rtypes.NameAbandonWithdrawAction, abandon)
}

func bumpWithdrawFeeCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "bump-withdraw-fee",
		Short:   "ask the bridge to re-sign an unsettled withdrawal at a higher fee rate",
		Run:     bumpWithdrawFee,
		Example: "bump-withdraw-fee --withdrawal-tx-hash <withdrawTxHashHex> --fee-rate 12",
	}
	cmd.Flags().String("withdrawal-tx-hash", "",
		"chain33 hash of the Withdraw (burn/lock) transaction, hex (0x prefix tolerated)")
	cmd.Flags().Int64("fee-rate", 0, "new BTC miner fee rate in sat/vB, must be above the original rate")
	markRequired(cmd, "withdrawal-tx-hash", "fee-rate")
	return cmd
}

func buildBumpWithdrawFee(cmd *cobra.Command) (*rtypes.BumpWithdrawFee, error) {
	flags := cmd.Flags()
	txHashHex, _ := flags.GetString("withdrawal-tx-hash")
	feeRate, _ := flags.GetInt64("fee-rate")

	txHash, err := decodeHexAuto(txHashHex)
	if err != nil {
		return nil, fmt.Errorf("invalid --withdrawal-tx-hash %q: %w", txHashHex, err)
	}
	if len(txHash) == 0 {
		return nil, fmt.Errorf("--withdrawal-tx-hash is required")
	}
	if feeRate <= 0 {
		return nil, fmt.Errorf("invalid --fee-rate %d: must be a positive sat/vB value", feeRate)
	}
	return &rtypes.BumpWithdrawFee{WithdrawalTxHash: txHash, FeeRate: feeRate}, nil
}

func bumpWithdrawFee(cmd *cobra.Command, _ []string) {
	bump, err := buildBumpWithdrawFee(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%v\n", err)
		return
	}
	createTxRPC(cmd, rtypes.NameBumpWithdrawFeeAction, bump)
}

func registerDepositScriptCMD() *cobra.Command {
	return &cobra.Command{
		Use:   "register-deposit-script",
		Short: "register a BTC deposit script for the signing address (idempotent)",
		Run:   registerDepositScript,
		Args:  cobra.NoArgs,
	}
}

func registerDepositScript(cmd *cobra.Command, _ []string) {
	createTxRPC(cmd, rtypes.NameRegisterDepositScriptAction, &rtypes.RegisterDepositScript{})
}
