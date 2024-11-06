// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.26.0
// 	protoc        v3.17.0
// source: rgbx.proto

package types

import (
	context "context"
	grpc "google.golang.org/grpc"
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type RgbxAction struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Ty int32 `protobuf:"varint,1,opt,name=ty,proto3" json:"ty,omitempty"`
	// Types that are assignable to Value:
	//	*RgbxAction_Mint
	//	*RgbxAction_Transfer
	//	*RgbxAction_Confirm
	Value isRgbxAction_Value `protobuf_oneof:"value"`
}

func (x *RgbxAction) Reset() {
	*x = RgbxAction{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *RgbxAction) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*RgbxAction) ProtoMessage() {}

func (x *RgbxAction) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use RgbxAction.ProtoReflect.Descriptor instead.
func (*RgbxAction) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{0}
}

func (x *RgbxAction) GetTy() int32 {
	if x != nil {
		return x.Ty
	}
	return 0
}

func (m *RgbxAction) GetValue() isRgbxAction_Value {
	if m != nil {
		return m.Value
	}
	return nil
}

func (x *RgbxAction) GetMint() *MintAsset {
	if x, ok := x.GetValue().(*RgbxAction_Mint); ok {
		return x.Mint
	}
	return nil
}

func (x *RgbxAction) GetTransfer() *TransferAsset {
	if x, ok := x.GetValue().(*RgbxAction_Transfer); ok {
		return x.Transfer
	}
	return nil
}

func (x *RgbxAction) GetConfirm() *ConfirmTx {
	if x, ok := x.GetValue().(*RgbxAction_Confirm); ok {
		return x.Confirm
	}
	return nil
}

type isRgbxAction_Value interface {
	isRgbxAction_Value()
}

type RgbxAction_Mint struct {
	Mint *MintAsset `protobuf:"bytes,2,opt,name=mint,proto3,oneof"`
}

type RgbxAction_Transfer struct {
	Transfer *TransferAsset `protobuf:"bytes,3,opt,name=transfer,proto3,oneof"`
}

type RgbxAction_Confirm struct {
	Confirm *ConfirmTx `protobuf:"bytes,4,opt,name=confirm,proto3,oneof"`
}

func (*RgbxAction_Mint) isRgbxAction_Value() {}

func (*RgbxAction_Transfer) isRgbxAction_Value() {}

func (*RgbxAction_Confirm) isRgbxAction_Value() {}

type Asset struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Type        uint32 `protobuf:"varint,1,opt,name=type,proto3" json:"type,omitempty"`
	TotalAmount int64  `protobuf:"varint,2,opt,name=totalAmount,proto3" json:"totalAmount,omitempty"`
	Symbol      string `protobuf:"bytes,3,opt,name=symbol,proto3" json:"symbol,omitempty"`
	MetaHash    []byte `protobuf:"bytes,4,opt,name=metaHash,proto3" json:"metaHash,omitempty"`
	GenesisTx   []byte `protobuf:"bytes,5,opt,name=genesisTx,proto3" json:"genesisTx,omitempty"`
}

func (x *Asset) Reset() {
	*x = Asset{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[1]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *Asset) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Asset) ProtoMessage() {}

func (x *Asset) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[1]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Asset.ProtoReflect.Descriptor instead.
func (*Asset) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{1}
}

func (x *Asset) GetType() uint32 {
	if x != nil {
		return x.Type
	}
	return 0
}

func (x *Asset) GetTotalAmount() int64 {
	if x != nil {
		return x.TotalAmount
	}
	return 0
}

func (x *Asset) GetSymbol() string {
	if x != nil {
		return x.Symbol
	}
	return ""
}

func (x *Asset) GetMetaHash() []byte {
	if x != nil {
		return x.MetaHash
	}
	return nil
}

func (x *Asset) GetGenesisTx() []byte {
	if x != nil {
		return x.GenesisTx
	}
	return nil
}

type MintAsset struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Type        uint32 `protobuf:"varint,1,opt,name=type,proto3" json:"type,omitempty"`
	TotalAmount int64  `protobuf:"varint,2,opt,name=totalAmount,proto3" json:"totalAmount,omitempty"`
	Symbol      string `protobuf:"bytes,3,opt,name=symbol,proto3" json:"symbol,omitempty"`
	MetaHash    []byte `protobuf:"bytes,4,opt,name=metaHash,proto3" json:"metaHash,omitempty"`
	// represents the outpoint of the transaction's
	// input that resulted in the creation of the asset.
	GenesisOut *OutPoint `protobuf:"bytes,5,opt,name=genesisOut,proto3" json:"genesisOut,omitempty"`
}

func (x *MintAsset) Reset() {
	*x = MintAsset{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[2]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *MintAsset) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*MintAsset) ProtoMessage() {}

func (x *MintAsset) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[2]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use MintAsset.ProtoReflect.Descriptor instead.
func (*MintAsset) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{2}
}

func (x *MintAsset) GetType() uint32 {
	if x != nil {
		return x.Type
	}
	return 0
}

func (x *MintAsset) GetTotalAmount() int64 {
	if x != nil {
		return x.TotalAmount
	}
	return 0
}

func (x *MintAsset) GetSymbol() string {
	if x != nil {
		return x.Symbol
	}
	return ""
}

func (x *MintAsset) GetMetaHash() []byte {
	if x != nil {
		return x.MetaHash
	}
	return nil
}

func (x *MintAsset) GetGenesisOut() *OutPoint {
	if x != nil {
		return x.GenesisOut
	}
	return nil
}

type OutPoint struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Hash  []byte `protobuf:"bytes,1,opt,name=hash,proto3" json:"hash,omitempty"`
	Index uint32 `protobuf:"varint,2,opt,name=index,proto3" json:"index,omitempty"`
}

func (x *OutPoint) Reset() {
	*x = OutPoint{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[3]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *OutPoint) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*OutPoint) ProtoMessage() {}

func (x *OutPoint) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[3]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use OutPoint.ProtoReflect.Descriptor instead.
func (*OutPoint) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{3}
}

func (x *OutPoint) GetHash() []byte {
	if x != nil {
		return x.Hash
	}
	return nil
}

func (x *OutPoint) GetIndex() uint32 {
	if x != nil {
		return x.Index
	}
	return 0
}

type TxOut struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Value    int64  `protobuf:"varint,1,opt,name=value,proto3" json:"value,omitempty"`
	PkScript []byte `protobuf:"bytes,2,opt,name=pkScript,proto3" json:"pkScript,omitempty"`
}

func (x *TxOut) Reset() {
	*x = TxOut{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[4]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *TxOut) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*TxOut) ProtoMessage() {}

func (x *TxOut) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[4]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use TxOut.ProtoReflect.Descriptor instead.
func (*TxOut) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{4}
}

func (x *TxOut) GetValue() int64 {
	if x != nil {
		return x.Value
	}
	return 0
}

func (x *TxOut) GetPkScript() []byte {
	if x != nil {
		return x.PkScript
	}
	return nil
}

type BtcWitness struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	TxHash   []byte    `protobuf:"bytes,1,opt,name=txHash,proto3" json:"txHash,omitempty"`
	In       *OutPoint `protobuf:"bytes,2,opt,name=in,proto3" json:"in,omitempty"`
	OpRetOut *TxOut    `protobuf:"bytes,3,opt,name=opRetOut,proto3" json:"opRetOut,omitempty"`
}

func (x *BtcWitness) Reset() {
	*x = BtcWitness{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[5]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *BtcWitness) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*BtcWitness) ProtoMessage() {}

func (x *BtcWitness) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[5]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use BtcWitness.ProtoReflect.Descriptor instead.
func (*BtcWitness) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{5}
}

func (x *BtcWitness) GetTxHash() []byte {
	if x != nil {
		return x.TxHash
	}
	return nil
}

func (x *BtcWitness) GetIn() *OutPoint {
	if x != nil {
		return x.In
	}
	return nil
}

func (x *BtcWitness) GetOpRetOut() *TxOut {
	if x != nil {
		return x.OpRetOut
	}
	return nil
}

type AssetAccount struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// Types that are assignable to Value:
	//	*AssetAccount_Utxo
	//	*AssetAccount_Address
	Value isAssetAccount_Value `protobuf_oneof:"value"`
}

func (x *AssetAccount) Reset() {
	*x = AssetAccount{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[6]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *AssetAccount) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AssetAccount) ProtoMessage() {}

func (x *AssetAccount) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[6]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AssetAccount.ProtoReflect.Descriptor instead.
func (*AssetAccount) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{6}
}

func (m *AssetAccount) GetValue() isAssetAccount_Value {
	if m != nil {
		return m.Value
	}
	return nil
}

func (x *AssetAccount) GetUtxo() *OutPoint {
	if x, ok := x.GetValue().(*AssetAccount_Utxo); ok {
		return x.Utxo
	}
	return nil
}

func (x *AssetAccount) GetAddress() string {
	if x, ok := x.GetValue().(*AssetAccount_Address); ok {
		return x.Address
	}
	return ""
}

type isAssetAccount_Value interface {
	isAssetAccount_Value()
}

type AssetAccount_Utxo struct {
	Utxo *OutPoint `protobuf:"bytes,1,opt,name=utxo,proto3,oneof"`
}

type AssetAccount_Address struct {
	Address string `protobuf:"bytes,2,opt,name=address,proto3,oneof"`
}

func (*AssetAccount_Utxo) isAssetAccount_Value() {}

func (*AssetAccount_Address) isAssetAccount_Value() {}

type TransferAsset struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Symbol string        `protobuf:"bytes,1,opt,name=symbol,proto3" json:"symbol,omitempty"`
	Amount int64         `protobuf:"varint,2,opt,name=amount,proto3" json:"amount,omitempty"`
	From   *AssetAccount `protobuf:"bytes,3,opt,name=from,proto3" json:"from,omitempty"`
	To     *AssetAccount `protobuf:"bytes,4,opt,name=to,proto3" json:"to,omitempty"`
	Change *AssetAccount `protobuf:"bytes,5,opt,name=change,proto3" json:"change,omitempty"`
}

func (x *TransferAsset) Reset() {
	*x = TransferAsset{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[7]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *TransferAsset) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*TransferAsset) ProtoMessage() {}

func (x *TransferAsset) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[7]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use TransferAsset.ProtoReflect.Descriptor instead.
func (*TransferAsset) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{7}
}

func (x *TransferAsset) GetSymbol() string {
	if x != nil {
		return x.Symbol
	}
	return ""
}

func (x *TransferAsset) GetAmount() int64 {
	if x != nil {
		return x.Amount
	}
	return 0
}

func (x *TransferAsset) GetFrom() *AssetAccount {
	if x != nil {
		return x.From
	}
	return nil
}

func (x *TransferAsset) GetTo() *AssetAccount {
	if x != nil {
		return x.To
	}
	return nil
}

func (x *TransferAsset) GetChange() *AssetAccount {
	if x != nil {
		return x.Change
	}
	return nil
}

type ConfirmTx struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	ActionType int32       `protobuf:"varint,1,opt,name=actionType,proto3" json:"actionType,omitempty"`
	Timeout    bool        `protobuf:"varint,2,opt,name=timeout,proto3" json:"timeout,omitempty"`
	TxHash     []byte      `protobuf:"bytes,3,opt,name=txHash,proto3" json:"txHash,omitempty"`
	Witness    *BtcWitness `protobuf:"bytes,4,opt,name=witness,proto3" json:"witness,omitempty"`
}

func (x *ConfirmTx) Reset() {
	*x = ConfirmTx{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[8]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ConfirmTx) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ConfirmTx) ProtoMessage() {}

func (x *ConfirmTx) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[8]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ConfirmTx.ProtoReflect.Descriptor instead.
func (*ConfirmTx) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{8}
}

func (x *ConfirmTx) GetActionType() int32 {
	if x != nil {
		return x.ActionType
	}
	return 0
}

func (x *ConfirmTx) GetTimeout() bool {
	if x != nil {
		return x.Timeout
	}
	return false
}

func (x *ConfirmTx) GetTxHash() []byte {
	if x != nil {
		return x.TxHash
	}
	return nil
}

func (x *ConfirmTx) GetWitness() *BtcWitness {
	if x != nil {
		return x.Witness
	}
	return nil
}

type PendingTx struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	ActionType int32     `protobuf:"varint,1,opt,name=actionType,proto3" json:"actionType,omitempty"`
	Timestamp  int64     `protobuf:"varint,2,opt,name=timestamp,proto3" json:"timestamp,omitempty"`
	TxHash     []byte    `protobuf:"bytes,3,opt,name=txHash,proto3" json:"txHash,omitempty"`
	Utxo       *OutPoint `protobuf:"bytes,4,opt,name=utxo,proto3" json:"utxo,omitempty"`
}

func (x *PendingTx) Reset() {
	*x = PendingTx{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rgbx_proto_msgTypes[9]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *PendingTx) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*PendingTx) ProtoMessage() {}

func (x *PendingTx) ProtoReflect() protoreflect.Message {
	mi := &file_rgbx_proto_msgTypes[9]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use PendingTx.ProtoReflect.Descriptor instead.
func (*PendingTx) Descriptor() ([]byte, []int) {
	return file_rgbx_proto_rawDescGZIP(), []int{9}
}

func (x *PendingTx) GetActionType() int32 {
	if x != nil {
		return x.ActionType
	}
	return 0
}

func (x *PendingTx) GetTimestamp() int64 {
	if x != nil {
		return x.Timestamp
	}
	return 0
}

func (x *PendingTx) GetTxHash() []byte {
	if x != nil {
		return x.TxHash
	}
	return nil
}

func (x *PendingTx) GetUtxo() *OutPoint {
	if x != nil {
		return x.Utxo
	}
	return nil
}

var File_rgbx_proto protoreflect.FileDescriptor

var file_rgbx_proto_rawDesc = []byte{
	0x0a, 0x0a, 0x72, 0x67, 0x62, 0x78, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x05, 0x74, 0x79,
	0x70, 0x65, 0x73, 0x22, 0xaf, 0x01, 0x0a, 0x0a, 0x52, 0x67, 0x62, 0x78, 0x41, 0x63, 0x74, 0x69,
	0x6f, 0x6e, 0x12, 0x0e, 0x0a, 0x02, 0x74, 0x79, 0x18, 0x01, 0x20, 0x01, 0x28, 0x05, 0x52, 0x02,
	0x74, 0x79, 0x12, 0x26, 0x0a, 0x04, 0x6d, 0x69, 0x6e, 0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b,
	0x32, 0x10, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x6d, 0x69, 0x6e, 0x74, 0x41, 0x73, 0x73,
	0x65, 0x74, 0x48, 0x00, 0x52, 0x04, 0x6d, 0x69, 0x6e, 0x74, 0x12, 0x32, 0x0a, 0x08, 0x74, 0x72,
	0x61, 0x6e, 0x73, 0x66, 0x65, 0x72, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x14, 0x2e, 0x74,
	0x79, 0x70, 0x65, 0x73, 0x2e, 0x74, 0x72, 0x61, 0x6e, 0x73, 0x66, 0x65, 0x72, 0x41, 0x73, 0x73,
	0x65, 0x74, 0x48, 0x00, 0x52, 0x08, 0x74, 0x72, 0x61, 0x6e, 0x73, 0x66, 0x65, 0x72, 0x12, 0x2c,
	0x0a, 0x07, 0x63, 0x6f, 0x6e, 0x66, 0x69, 0x72, 0x6d, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0b, 0x32,
	0x10, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x63, 0x6f, 0x6e, 0x66, 0x69, 0x72, 0x6d, 0x54,
	0x78, 0x48, 0x00, 0x52, 0x07, 0x63, 0x6f, 0x6e, 0x66, 0x69, 0x72, 0x6d, 0x42, 0x07, 0x0a, 0x05,
	0x76, 0x61, 0x6c, 0x75, 0x65, 0x22, 0x8f, 0x01, 0x0a, 0x05, 0x61, 0x73, 0x73, 0x65, 0x74, 0x12,
	0x12, 0x0a, 0x04, 0x74, 0x79, 0x70, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0d, 0x52, 0x04, 0x74,
	0x79, 0x70, 0x65, 0x12, 0x20, 0x0a, 0x0b, 0x74, 0x6f, 0x74, 0x61, 0x6c, 0x41, 0x6d, 0x6f, 0x75,
	0x6e, 0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x03, 0x52, 0x0b, 0x74, 0x6f, 0x74, 0x61, 0x6c, 0x41,
	0x6d, 0x6f, 0x75, 0x6e, 0x74, 0x12, 0x16, 0x0a, 0x06, 0x73, 0x79, 0x6d, 0x62, 0x6f, 0x6c, 0x18,
	0x03, 0x20, 0x01, 0x28, 0x09, 0x52, 0x06, 0x73, 0x79, 0x6d, 0x62, 0x6f, 0x6c, 0x12, 0x1a, 0x0a,
	0x08, 0x6d, 0x65, 0x74, 0x61, 0x48, 0x61, 0x73, 0x68, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0c, 0x52,
	0x08, 0x6d, 0x65, 0x74, 0x61, 0x48, 0x61, 0x73, 0x68, 0x12, 0x1c, 0x0a, 0x09, 0x67, 0x65, 0x6e,
	0x65, 0x73, 0x69, 0x73, 0x54, 0x78, 0x18, 0x05, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x09, 0x67, 0x65,
	0x6e, 0x65, 0x73, 0x69, 0x73, 0x54, 0x78, 0x22, 0xa6, 0x01, 0x0a, 0x09, 0x6d, 0x69, 0x6e, 0x74,
	0x41, 0x73, 0x73, 0x65, 0x74, 0x12, 0x12, 0x0a, 0x04, 0x74, 0x79, 0x70, 0x65, 0x18, 0x01, 0x20,
	0x01, 0x28, 0x0d, 0x52, 0x04, 0x74, 0x79, 0x70, 0x65, 0x12, 0x20, 0x0a, 0x0b, 0x74, 0x6f, 0x74,
	0x61, 0x6c, 0x41, 0x6d, 0x6f, 0x75, 0x6e, 0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x03, 0x52, 0x0b,
	0x74, 0x6f, 0x74, 0x61, 0x6c, 0x41, 0x6d, 0x6f, 0x75, 0x6e, 0x74, 0x12, 0x16, 0x0a, 0x06, 0x73,
	0x79, 0x6d, 0x62, 0x6f, 0x6c, 0x18, 0x03, 0x20, 0x01, 0x28, 0x09, 0x52, 0x06, 0x73, 0x79, 0x6d,
	0x62, 0x6f, 0x6c, 0x12, 0x1a, 0x0a, 0x08, 0x6d, 0x65, 0x74, 0x61, 0x48, 0x61, 0x73, 0x68, 0x18,
	0x04, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x08, 0x6d, 0x65, 0x74, 0x61, 0x48, 0x61, 0x73, 0x68, 0x12,
	0x2f, 0x0a, 0x0a, 0x67, 0x65, 0x6e, 0x65, 0x73, 0x69, 0x73, 0x4f, 0x75, 0x74, 0x18, 0x05, 0x20,
	0x01, 0x28, 0x0b, 0x32, 0x0f, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x6f, 0x75, 0x74, 0x50,
	0x6f, 0x69, 0x6e, 0x74, 0x52, 0x0a, 0x67, 0x65, 0x6e, 0x65, 0x73, 0x69, 0x73, 0x4f, 0x75, 0x74,
	0x22, 0x34, 0x0a, 0x08, 0x6f, 0x75, 0x74, 0x50, 0x6f, 0x69, 0x6e, 0x74, 0x12, 0x12, 0x0a, 0x04,
	0x68, 0x61, 0x73, 0x68, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x04, 0x68, 0x61, 0x73, 0x68,
	0x12, 0x14, 0x0a, 0x05, 0x69, 0x6e, 0x64, 0x65, 0x78, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0d, 0x52,
	0x05, 0x69, 0x6e, 0x64, 0x65, 0x78, 0x22, 0x39, 0x0a, 0x05, 0x74, 0x78, 0x4f, 0x75, 0x74, 0x12,
	0x14, 0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x03, 0x52, 0x05,
	0x76, 0x61, 0x6c, 0x75, 0x65, 0x12, 0x1a, 0x0a, 0x08, 0x70, 0x6b, 0x53, 0x63, 0x72, 0x69, 0x70,
	0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x08, 0x70, 0x6b, 0x53, 0x63, 0x72, 0x69, 0x70,
	0x74, 0x22, 0x6f, 0x0a, 0x0a, 0x62, 0x74, 0x63, 0x57, 0x69, 0x74, 0x6e, 0x65, 0x73, 0x73, 0x12,
	0x16, 0x0a, 0x06, 0x74, 0x78, 0x48, 0x61, 0x73, 0x68, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0c, 0x52,
	0x06, 0x74, 0x78, 0x48, 0x61, 0x73, 0x68, 0x12, 0x1f, 0x0a, 0x02, 0x69, 0x6e, 0x18, 0x02, 0x20,
	0x01, 0x28, 0x0b, 0x32, 0x0f, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x6f, 0x75, 0x74, 0x50,
	0x6f, 0x69, 0x6e, 0x74, 0x52, 0x02, 0x69, 0x6e, 0x12, 0x28, 0x0a, 0x08, 0x6f, 0x70, 0x52, 0x65,
	0x74, 0x4f, 0x75, 0x74, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x0c, 0x2e, 0x74, 0x79, 0x70,
	0x65, 0x73, 0x2e, 0x74, 0x78, 0x4f, 0x75, 0x74, 0x52, 0x08, 0x6f, 0x70, 0x52, 0x65, 0x74, 0x4f,
	0x75, 0x74, 0x22, 0x5a, 0x0a, 0x0c, 0x41, 0x73, 0x73, 0x65, 0x74, 0x41, 0x63, 0x63, 0x6f, 0x75,
	0x6e, 0x74, 0x12, 0x25, 0x0a, 0x04, 0x75, 0x74, 0x78, 0x6f, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b,
	0x32, 0x0f, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x6f, 0x75, 0x74, 0x50, 0x6f, 0x69, 0x6e,
	0x74, 0x48, 0x00, 0x52, 0x04, 0x75, 0x74, 0x78, 0x6f, 0x12, 0x1a, 0x0a, 0x07, 0x61, 0x64, 0x64,
	0x72, 0x65, 0x73, 0x73, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x48, 0x00, 0x52, 0x07, 0x61, 0x64,
	0x64, 0x72, 0x65, 0x73, 0x73, 0x42, 0x07, 0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x22, 0xba,
	0x01, 0x0a, 0x0d, 0x74, 0x72, 0x61, 0x6e, 0x73, 0x66, 0x65, 0x72, 0x41, 0x73, 0x73, 0x65, 0x74,
	0x12, 0x16, 0x0a, 0x06, 0x73, 0x79, 0x6d, 0x62, 0x6f, 0x6c, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09,
	0x52, 0x06, 0x73, 0x79, 0x6d, 0x62, 0x6f, 0x6c, 0x12, 0x16, 0x0a, 0x06, 0x61, 0x6d, 0x6f, 0x75,
	0x6e, 0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x03, 0x52, 0x06, 0x61, 0x6d, 0x6f, 0x75, 0x6e, 0x74,
	0x12, 0x27, 0x0a, 0x04, 0x66, 0x72, 0x6f, 0x6d, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x13,
	0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x41, 0x73, 0x73, 0x65, 0x74, 0x41, 0x63, 0x63, 0x6f,
	0x75, 0x6e, 0x74, 0x52, 0x04, 0x66, 0x72, 0x6f, 0x6d, 0x12, 0x23, 0x0a, 0x02, 0x74, 0x6f, 0x18,
	0x04, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x13, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x41, 0x73,
	0x73, 0x65, 0x74, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x52, 0x02, 0x74, 0x6f, 0x12, 0x2b,
	0x0a, 0x06, 0x63, 0x68, 0x61, 0x6e, 0x67, 0x65, 0x18, 0x05, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x13,
	0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x41, 0x73, 0x73, 0x65, 0x74, 0x41, 0x63, 0x63, 0x6f,
	0x75, 0x6e, 0x74, 0x52, 0x06, 0x63, 0x68, 0x61, 0x6e, 0x67, 0x65, 0x22, 0x8a, 0x01, 0x0a, 0x09,
	0x63, 0x6f, 0x6e, 0x66, 0x69, 0x72, 0x6d, 0x54, 0x78, 0x12, 0x1e, 0x0a, 0x0a, 0x61, 0x63, 0x74,
	0x69, 0x6f, 0x6e, 0x54, 0x79, 0x70, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x05, 0x52, 0x0a, 0x61,
	0x63, 0x74, 0x69, 0x6f, 0x6e, 0x54, 0x79, 0x70, 0x65, 0x12, 0x18, 0x0a, 0x07, 0x74, 0x69, 0x6d,
	0x65, 0x6f, 0x75, 0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x08, 0x52, 0x07, 0x74, 0x69, 0x6d, 0x65,
	0x6f, 0x75, 0x74, 0x12, 0x16, 0x0a, 0x06, 0x74, 0x78, 0x48, 0x61, 0x73, 0x68, 0x18, 0x03, 0x20,
	0x01, 0x28, 0x0c, 0x52, 0x06, 0x74, 0x78, 0x48, 0x61, 0x73, 0x68, 0x12, 0x2b, 0x0a, 0x07, 0x77,
	0x69, 0x74, 0x6e, 0x65, 0x73, 0x73, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x11, 0x2e, 0x74,
	0x79, 0x70, 0x65, 0x73, 0x2e, 0x62, 0x74, 0x63, 0x57, 0x69, 0x74, 0x6e, 0x65, 0x73, 0x73, 0x52,
	0x07, 0x77, 0x69, 0x74, 0x6e, 0x65, 0x73, 0x73, 0x22, 0x86, 0x01, 0x0a, 0x09, 0x70, 0x65, 0x6e,
	0x64, 0x69, 0x6e, 0x67, 0x54, 0x78, 0x12, 0x1e, 0x0a, 0x0a, 0x61, 0x63, 0x74, 0x69, 0x6f, 0x6e,
	0x54, 0x79, 0x70, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x05, 0x52, 0x0a, 0x61, 0x63, 0x74, 0x69,
	0x6f, 0x6e, 0x54, 0x79, 0x70, 0x65, 0x12, 0x1c, 0x0a, 0x09, 0x74, 0x69, 0x6d, 0x65, 0x73, 0x74,
	0x61, 0x6d, 0x70, 0x18, 0x02, 0x20, 0x01, 0x28, 0x03, 0x52, 0x09, 0x74, 0x69, 0x6d, 0x65, 0x73,
	0x74, 0x61, 0x6d, 0x70, 0x12, 0x16, 0x0a, 0x06, 0x74, 0x78, 0x48, 0x61, 0x73, 0x68, 0x18, 0x03,
	0x20, 0x01, 0x28, 0x0c, 0x52, 0x06, 0x74, 0x78, 0x48, 0x61, 0x73, 0x68, 0x12, 0x23, 0x0a, 0x04,
	0x75, 0x74, 0x78, 0x6f, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x0f, 0x2e, 0x74, 0x79, 0x70,
	0x65, 0x73, 0x2e, 0x6f, 0x75, 0x74, 0x50, 0x6f, 0x69, 0x6e, 0x74, 0x52, 0x04, 0x75, 0x74, 0x78,
	0x6f, 0x32, 0x06, 0x0a, 0x04, 0x72, 0x67, 0x62, 0x78, 0x42, 0x0a, 0x5a, 0x08, 0x2e, 0x2e, 0x2f,
	0x74, 0x79, 0x70, 0x65, 0x73, 0x62, 0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_rgbx_proto_rawDescOnce sync.Once
	file_rgbx_proto_rawDescData = file_rgbx_proto_rawDesc
)

func file_rgbx_proto_rawDescGZIP() []byte {
	file_rgbx_proto_rawDescOnce.Do(func() {
		file_rgbx_proto_rawDescData = protoimpl.X.CompressGZIP(file_rgbx_proto_rawDescData)
	})
	return file_rgbx_proto_rawDescData
}

var file_rgbx_proto_msgTypes = make([]protoimpl.MessageInfo, 10)
var file_rgbx_proto_goTypes = []interface{}{
	(*RgbxAction)(nil),    // 0: types.RgbxAction
	(*Asset)(nil),         // 1: types.asset
	(*MintAsset)(nil),     // 2: types.mintAsset
	(*OutPoint)(nil),      // 3: types.outPoint
	(*TxOut)(nil),         // 4: types.txOut
	(*BtcWitness)(nil),    // 5: types.btcWitness
	(*AssetAccount)(nil),  // 6: types.AssetAccount
	(*TransferAsset)(nil), // 7: types.transferAsset
	(*ConfirmTx)(nil),     // 8: types.confirmTx
	(*PendingTx)(nil),     // 9: types.pendingTx
}
var file_rgbx_proto_depIdxs = []int32{
	2,  // 0: types.RgbxAction.mint:type_name -> types.mintAsset
	7,  // 1: types.RgbxAction.transfer:type_name -> types.transferAsset
	8,  // 2: types.RgbxAction.confirm:type_name -> types.confirmTx
	3,  // 3: types.mintAsset.genesisOut:type_name -> types.outPoint
	3,  // 4: types.btcWitness.in:type_name -> types.outPoint
	4,  // 5: types.btcWitness.opRetOut:type_name -> types.txOut
	3,  // 6: types.AssetAccount.utxo:type_name -> types.outPoint
	6,  // 7: types.transferAsset.from:type_name -> types.AssetAccount
	6,  // 8: types.transferAsset.to:type_name -> types.AssetAccount
	6,  // 9: types.transferAsset.change:type_name -> types.AssetAccount
	5,  // 10: types.confirmTx.witness:type_name -> types.btcWitness
	3,  // 11: types.pendingTx.utxo:type_name -> types.outPoint
	12, // [12:12] is the sub-list for method output_type
	12, // [12:12] is the sub-list for method input_type
	12, // [12:12] is the sub-list for extension type_name
	12, // [12:12] is the sub-list for extension extendee
	0,  // [0:12] is the sub-list for field type_name
}

func init() { file_rgbx_proto_init() }
func file_rgbx_proto_init() {
	if File_rgbx_proto != nil {
		return
	}
	if !protoimpl.UnsafeEnabled {
		file_rgbx_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*RgbxAction); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rgbx_proto_msgTypes[1].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*Asset); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rgbx_proto_msgTypes[2].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*MintAsset); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rgbx_proto_msgTypes[3].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*OutPoint); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rgbx_proto_msgTypes[4].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*TxOut); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rgbx_proto_msgTypes[5].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*BtcWitness); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rgbx_proto_msgTypes[6].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*AssetAccount); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rgbx_proto_msgTypes[7].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*TransferAsset); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rgbx_proto_msgTypes[8].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ConfirmTx); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rgbx_proto_msgTypes[9].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*PendingTx); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
	}
	file_rgbx_proto_msgTypes[0].OneofWrappers = []interface{}{
		(*RgbxAction_Mint)(nil),
		(*RgbxAction_Transfer)(nil),
		(*RgbxAction_Confirm)(nil),
	}
	file_rgbx_proto_msgTypes[6].OneofWrappers = []interface{}{
		(*AssetAccount_Utxo)(nil),
		(*AssetAccount_Address)(nil),
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_rgbx_proto_rawDesc,
			NumEnums:      0,
			NumMessages:   10,
			NumExtensions: 0,
			NumServices:   1,
		},
		GoTypes:           file_rgbx_proto_goTypes,
		DependencyIndexes: file_rgbx_proto_depIdxs,
		MessageInfos:      file_rgbx_proto_msgTypes,
	}.Build()
	File_rgbx_proto = out.File
	file_rgbx_proto_rawDesc = nil
	file_rgbx_proto_goTypes = nil
	file_rgbx_proto_depIdxs = nil
}

// Reference imports to suppress errors if they are not otherwise used.
var _ context.Context
var _ grpc.ClientConnInterface

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
const _ = grpc.SupportPackageIsVersion6

// RgbxClient is the client API for Rgbx service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://godoc.org/google.golang.org/grpc#ClientConn.NewStream.
type RgbxClient interface {
}

type rgbxClient struct {
	cc grpc.ClientConnInterface
}

func NewRgbxClient(cc grpc.ClientConnInterface) RgbxClient {
	return &rgbxClient{cc}
}

// RgbxServer is the server API for Rgbx service.
type RgbxServer interface {
}

// UnimplementedRgbxServer can be embedded to have forward compatible implementations.
type UnimplementedRgbxServer struct {
}

func RegisterRgbxServer(s *grpc.Server, srv RgbxServer) {
	s.RegisterService(&_Rgbx_serviceDesc, srv)
}

var _Rgbx_serviceDesc = grpc.ServiceDesc{
	ServiceName: "types.rgbx",
	HandlerType: (*RgbxServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams:     []grpc.StreamDesc{},
	Metadata:    "rgbx.proto",
}