// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.23.0
// 	protoc        v3.6.0
// source: rollup.proto

package types

import (
	context "context"
	types "github.com/33cn/chain33/types"
	proto "github.com/golang/protobuf/proto"
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

// This is a compile-time assertion that a sufficiently up-to-date version
// of the legacy proto package is being used.
const _ = proto.ProtoPackageIsVersion4

// rollup合约交易行为总类型
type RollupAction struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Ty int32 `protobuf:"varint,1,opt,name=ty,proto3" json:"ty,omitempty"`
	// Types that are assignable to Value:
	//	*RollupAction_Commit
	Value isRollupAction_Value `protobuf_oneof:"value"`
}

func (x *RollupAction) Reset() {
	*x = RollupAction{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rollup_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *RollupAction) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*RollupAction) ProtoMessage() {}

func (x *RollupAction) ProtoReflect() protoreflect.Message {
	mi := &file_rollup_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use RollupAction.ProtoReflect.Descriptor instead.
func (*RollupAction) Descriptor() ([]byte, []int) {
	return file_rollup_proto_rawDescGZIP(), []int{0}
}

func (x *RollupAction) GetTy() int32 {
	if x != nil {
		return x.Ty
	}
	return 0
}

func (m *RollupAction) GetValue() isRollupAction_Value {
	if m != nil {
		return m.Value
	}
	return nil
}

func (x *RollupAction) GetCommit() *CheckPoint {
	if x, ok := x.GetValue().(*RollupAction_Commit); ok {
		return x.Commit
	}
	return nil
}

type isRollupAction_Value interface {
	isRollupAction_Value()
}

type RollupAction_Commit struct {
	Commit *CheckPoint `protobuf:"bytes,2,opt,name=commit,proto3,oneof"`
}

func (*RollupAction_Commit) isRollupAction_Value() {}

type BlockBatch struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	BlockHeaders    []*types.Header `protobuf:"bytes,1,rep,name=blockHeaders,proto3" json:"blockHeaders,omitempty"`
	TxList          [][]byte        `protobuf:"bytes,2,rep,name=txList,proto3" json:"txList,omitempty"`
	PubKeyList      [][]byte        `protobuf:"bytes,3,rep,name=pubKeyList,proto3" json:"pubKeyList,omitempty"`
	AggregateTxSign []byte          `protobuf:"bytes,4,opt,name=aggregateTxSign,proto3" json:"aggregateTxSign,omitempty"`
	TxAddrIDList    []byte          `protobuf:"bytes,5,opt,name=txAddrIDList,proto3" json:"txAddrIDList,omitempty"`
	CrossTxRootHash []byte          `protobuf:"bytes,6,opt,name=crossTxRootHash,proto3" json:"crossTxRootHash,omitempty"`
}

func (x *BlockBatch) Reset() {
	*x = BlockBatch{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rollup_proto_msgTypes[1]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *BlockBatch) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*BlockBatch) ProtoMessage() {}

func (x *BlockBatch) ProtoReflect() protoreflect.Message {
	mi := &file_rollup_proto_msgTypes[1]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use BlockBatch.ProtoReflect.Descriptor instead.
func (*BlockBatch) Descriptor() ([]byte, []int) {
	return file_rollup_proto_rawDescGZIP(), []int{1}
}

func (x *BlockBatch) GetBlockHeaders() []*types.Header {
	if x != nil {
		return x.BlockHeaders
	}
	return nil
}

func (x *BlockBatch) GetTxList() [][]byte {
	if x != nil {
		return x.TxList
	}
	return nil
}

func (x *BlockBatch) GetPubKeyList() [][]byte {
	if x != nil {
		return x.PubKeyList
	}
	return nil
}

func (x *BlockBatch) GetAggregateTxSign() []byte {
	if x != nil {
		return x.AggregateTxSign
	}
	return nil
}

func (x *BlockBatch) GetTxAddrIDList() []byte {
	if x != nil {
		return x.TxAddrIDList
	}
	return nil
}

func (x *BlockBatch) GetCrossTxRootHash() []byte {
	if x != nil {
		return x.CrossTxRootHash
	}
	return nil
}

type ValidatorSignMsg struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	CommitRound int64  `protobuf:"varint,1,opt,name=commitRound,proto3" json:"commitRound,omitempty"`
	PubKey      []byte `protobuf:"bytes,2,opt,name=pubKey,proto3" json:"pubKey,omitempty"`
	Signature   []byte `protobuf:"bytes,3,opt,name=signature,proto3" json:"signature,omitempty"`
	MsgHash     []byte `protobuf:"bytes,4,opt,name=msgHash,proto3" json:"msgHash,omitempty"`
}

func (x *ValidatorSignMsg) Reset() {
	*x = ValidatorSignMsg{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rollup_proto_msgTypes[2]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ValidatorSignMsg) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ValidatorSignMsg) ProtoMessage() {}

func (x *ValidatorSignMsg) ProtoReflect() protoreflect.Message {
	mi := &file_rollup_proto_msgTypes[2]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ValidatorSignMsg.ProtoReflect.Descriptor instead.
func (*ValidatorSignMsg) Descriptor() ([]byte, []int) {
	return file_rollup_proto_rawDescGZIP(), []int{2}
}

func (x *ValidatorSignMsg) GetCommitRound() int64 {
	if x != nil {
		return x.CommitRound
	}
	return 0
}

func (x *ValidatorSignMsg) GetPubKey() []byte {
	if x != nil {
		return x.PubKey
	}
	return nil
}

func (x *ValidatorSignMsg) GetSignature() []byte {
	if x != nil {
		return x.Signature
	}
	return nil
}

func (x *ValidatorSignMsg) GetMsgHash() []byte {
	if x != nil {
		return x.MsgHash
	}
	return nil
}

// CheckPoint
type CheckPoint struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	ChainTitle             string      `protobuf:"bytes,1,opt,name=chainTitle,proto3" json:"chainTitle,omitempty"`
	CommitRound            int64       `protobuf:"varint,2,opt,name=commitRound,proto3" json:"commitRound,omitempty"`
	Batch                  *BlockBatch `protobuf:"bytes,3,opt,name=batch,proto3" json:"batch,omitempty"`
	ValidatorPubs          [][]byte    `protobuf:"bytes,4,rep,name=validatorPubs,proto3" json:"validatorPubs,omitempty"`
	AggregateValidatorSign []byte      `protobuf:"bytes,5,opt,name=aggregateValidatorSign,proto3" json:"aggregateValidatorSign,omitempty"`
}

func (x *CheckPoint) Reset() {
	*x = CheckPoint{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rollup_proto_msgTypes[3]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *CheckPoint) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CheckPoint) ProtoMessage() {}

func (x *CheckPoint) ProtoReflect() protoreflect.Message {
	mi := &file_rollup_proto_msgTypes[3]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use CheckPoint.ProtoReflect.Descriptor instead.
func (*CheckPoint) Descriptor() ([]byte, []int) {
	return file_rollup_proto_rawDescGZIP(), []int{3}
}

func (x *CheckPoint) GetChainTitle() string {
	if x != nil {
		return x.ChainTitle
	}
	return ""
}

func (x *CheckPoint) GetCommitRound() int64 {
	if x != nil {
		return x.CommitRound
	}
	return 0
}

func (x *CheckPoint) GetBatch() *BlockBatch {
	if x != nil {
		return x.Batch
	}
	return nil
}

func (x *CheckPoint) GetValidatorPubs() [][]byte {
	if x != nil {
		return x.ValidatorPubs
	}
	return nil
}

func (x *CheckPoint) GetAggregateValidatorSign() []byte {
	if x != nil {
		return x.AggregateValidatorSign
	}
	return nil
}

type RollupStatus struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Timestamp         int64  `protobuf:"varint,1,opt,name=timestamp,proto3" json:"timestamp,omitempty"`
	CommitRound       int64  `protobuf:"varint,2,opt,name=commitRound,proto3" json:"commitRound,omitempty"`
	CommitBlockHeight int64  `protobuf:"varint,3,opt,name=commitBlockHeight,proto3" json:"commitBlockHeight,omitempty"`
	CommitBlockHash   string `protobuf:"bytes,4,opt,name=commitBlockHash,proto3" json:"commitBlockHash,omitempty"`
	CommitAddr        string `protobuf:"bytes,5,opt,name=commitAddr,proto3" json:"commitAddr,omitempty"`
	CrossTxRootHash   []byte `protobuf:"bytes,6,opt,name=crossTxRootHash,proto3" json:"crossTxRootHash,omitempty"`
}

func (x *RollupStatus) Reset() {
	*x = RollupStatus{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rollup_proto_msgTypes[4]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *RollupStatus) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*RollupStatus) ProtoMessage() {}

func (x *RollupStatus) ProtoReflect() protoreflect.Message {
	mi := &file_rollup_proto_msgTypes[4]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use RollupStatus.ProtoReflect.Descriptor instead.
func (*RollupStatus) Descriptor() ([]byte, []int) {
	return file_rollup_proto_rawDescGZIP(), []int{4}
}

func (x *RollupStatus) GetTimestamp() int64 {
	if x != nil {
		return x.Timestamp
	}
	return 0
}

func (x *RollupStatus) GetCommitRound() int64 {
	if x != nil {
		return x.CommitRound
	}
	return 0
}

func (x *RollupStatus) GetCommitBlockHeight() int64 {
	if x != nil {
		return x.CommitBlockHeight
	}
	return 0
}

func (x *RollupStatus) GetCommitBlockHash() string {
	if x != nil {
		return x.CommitBlockHash
	}
	return ""
}

func (x *RollupStatus) GetCommitAddr() string {
	if x != nil {
		return x.CommitAddr
	}
	return ""
}

func (x *RollupStatus) GetCrossTxRootHash() []byte {
	if x != nil {
		return x.CrossTxRootHash
	}
	return nil
}

type CommitRoundInfo struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	CommitRound     int64 `protobuf:"varint,1,opt,name=commitRound,proto3" json:"commitRound,omitempty"`
	LastBlockHeight int64 `protobuf:"varint,2,opt,name=lastBlockHeight,proto3" json:"lastBlockHeight,omitempty"`
	// 第一个区块的父哈希
	ParentBlockHash string `protobuf:"bytes,3,opt,name=parentBlockHash,proto3" json:"parentBlockHash,omitempty"`
	LastBlockHash   string `protobuf:"bytes,4,opt,name=lastBlockHash,proto3" json:"lastBlockHash,omitempty"`
}

func (x *CommitRoundInfo) Reset() {
	*x = CommitRoundInfo{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rollup_proto_msgTypes[5]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *CommitRoundInfo) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CommitRoundInfo) ProtoMessage() {}

func (x *CommitRoundInfo) ProtoReflect() protoreflect.Message {
	mi := &file_rollup_proto_msgTypes[5]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use CommitRoundInfo.ProtoReflect.Descriptor instead.
func (*CommitRoundInfo) Descriptor() ([]byte, []int) {
	return file_rollup_proto_rawDescGZIP(), []int{5}
}

func (x *CommitRoundInfo) GetCommitRound() int64 {
	if x != nil {
		return x.CommitRound
	}
	return 0
}

func (x *CommitRoundInfo) GetLastBlockHeight() int64 {
	if x != nil {
		return x.LastBlockHeight
	}
	return 0
}

func (x *CommitRoundInfo) GetParentBlockHash() string {
	if x != nil {
		return x.ParentBlockHash
	}
	return ""
}

func (x *CommitRoundInfo) GetLastBlockHash() string {
	if x != nil {
		return x.LastBlockHash
	}
	return ""
}

type Req struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	ChainTitle string `protobuf:"bytes,1,opt,name=chainTitle,proto3" json:"chainTitle,omitempty"`
}

func (x *Req) Reset() {
	*x = Req{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rollup_proto_msgTypes[6]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *Req) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Req) ProtoMessage() {}

func (x *Req) ProtoReflect() protoreflect.Message {
	mi := &file_rollup_proto_msgTypes[6]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Req.ProtoReflect.Descriptor instead.
func (*Req) Descriptor() ([]byte, []int) {
	return file_rollup_proto_rawDescGZIP(), []int{6}
}

func (x *Req) GetChainTitle() string {
	if x != nil {
		return x.ChainTitle
	}
	return ""
}

type ChainTitle struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Value string `protobuf:"bytes,1,opt,name=value,proto3" json:"value,omitempty"`
}

func (x *ChainTitle) Reset() {
	*x = ChainTitle{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rollup_proto_msgTypes[7]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ChainTitle) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ChainTitle) ProtoMessage() {}

func (x *ChainTitle) ProtoReflect() protoreflect.Message {
	mi := &file_rollup_proto_msgTypes[7]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ChainTitle.ProtoReflect.Descriptor instead.
func (*ChainTitle) Descriptor() ([]byte, []int) {
	return file_rollup_proto_rawDescGZIP(), []int{7}
}

func (x *ChainTitle) GetValue() string {
	if x != nil {
		return x.Value
	}
	return ""
}

type ValidatorPubs struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	BlsPubs []string `protobuf:"bytes,1,rep,name=blsPubs,proto3" json:"blsPubs,omitempty"`
}

func (x *ValidatorPubs) Reset() {
	*x = ValidatorPubs{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rollup_proto_msgTypes[8]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ValidatorPubs) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ValidatorPubs) ProtoMessage() {}

func (x *ValidatorPubs) ProtoReflect() protoreflect.Message {
	mi := &file_rollup_proto_msgTypes[8]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ValidatorPubs.ProtoReflect.Descriptor instead.
func (*ValidatorPubs) Descriptor() ([]byte, []int) {
	return file_rollup_proto_rawDescGZIP(), []int{8}
}

func (x *ValidatorPubs) GetBlsPubs() []string {
	if x != nil {
		return x.BlsPubs
	}
	return nil
}

var File_rollup_proto protoreflect.FileDescriptor

var file_rollup_proto_rawDesc = []byte{
	0x0a, 0x0c, 0x72, 0x6f, 0x6c, 0x6c, 0x75, 0x70, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x05,
	0x74, 0x79, 0x70, 0x65, 0x73, 0x1a, 0x10, 0x62, 0x6c, 0x6f, 0x63, 0x6b, 0x63, 0x68, 0x61, 0x69,
	0x6e, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x22, 0x54, 0x0a, 0x0c, 0x52, 0x6f, 0x6c, 0x6c, 0x75,
	0x70, 0x41, 0x63, 0x74, 0x69, 0x6f, 0x6e, 0x12, 0x0e, 0x0a, 0x02, 0x74, 0x79, 0x18, 0x01, 0x20,
	0x01, 0x28, 0x05, 0x52, 0x02, 0x74, 0x79, 0x12, 0x2b, 0x0a, 0x06, 0x63, 0x6f, 0x6d, 0x6d, 0x69,
	0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x11, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e,
	0x43, 0x68, 0x65, 0x63, 0x6b, 0x50, 0x6f, 0x69, 0x6e, 0x74, 0x48, 0x00, 0x52, 0x06, 0x63, 0x6f,
	0x6d, 0x6d, 0x69, 0x74, 0x42, 0x07, 0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x22, 0xef, 0x01,
	0x0a, 0x0a, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x42, 0x61, 0x74, 0x63, 0x68, 0x12, 0x31, 0x0a, 0x0c,
	0x62, 0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x65, 0x61, 0x64, 0x65, 0x72, 0x73, 0x18, 0x01, 0x20, 0x03,
	0x28, 0x0b, 0x32, 0x0d, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x48, 0x65, 0x61, 0x64, 0x65,
	0x72, 0x52, 0x0c, 0x62, 0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x65, 0x61, 0x64, 0x65, 0x72, 0x73, 0x12,
	0x16, 0x0a, 0x06, 0x74, 0x78, 0x4c, 0x69, 0x73, 0x74, 0x18, 0x02, 0x20, 0x03, 0x28, 0x0c, 0x52,
	0x06, 0x74, 0x78, 0x4c, 0x69, 0x73, 0x74, 0x12, 0x1e, 0x0a, 0x0a, 0x70, 0x75, 0x62, 0x4b, 0x65,
	0x79, 0x4c, 0x69, 0x73, 0x74, 0x18, 0x03, 0x20, 0x03, 0x28, 0x0c, 0x52, 0x0a, 0x70, 0x75, 0x62,
	0x4b, 0x65, 0x79, 0x4c, 0x69, 0x73, 0x74, 0x12, 0x28, 0x0a, 0x0f, 0x61, 0x67, 0x67, 0x72, 0x65,
	0x67, 0x61, 0x74, 0x65, 0x54, 0x78, 0x53, 0x69, 0x67, 0x6e, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0c,
	0x52, 0x0f, 0x61, 0x67, 0x67, 0x72, 0x65, 0x67, 0x61, 0x74, 0x65, 0x54, 0x78, 0x53, 0x69, 0x67,
	0x6e, 0x12, 0x22, 0x0a, 0x0c, 0x74, 0x78, 0x41, 0x64, 0x64, 0x72, 0x49, 0x44, 0x4c, 0x69, 0x73,
	0x74, 0x18, 0x05, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x0c, 0x74, 0x78, 0x41, 0x64, 0x64, 0x72, 0x49,
	0x44, 0x4c, 0x69, 0x73, 0x74, 0x12, 0x28, 0x0a, 0x0f, 0x63, 0x72, 0x6f, 0x73, 0x73, 0x54, 0x78,
	0x52, 0x6f, 0x6f, 0x74, 0x48, 0x61, 0x73, 0x68, 0x18, 0x06, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x0f,
	0x63, 0x72, 0x6f, 0x73, 0x73, 0x54, 0x78, 0x52, 0x6f, 0x6f, 0x74, 0x48, 0x61, 0x73, 0x68, 0x22,
	0x84, 0x01, 0x0a, 0x10, 0x56, 0x61, 0x6c, 0x69, 0x64, 0x61, 0x74, 0x6f, 0x72, 0x53, 0x69, 0x67,
	0x6e, 0x4d, 0x73, 0x67, 0x12, 0x20, 0x0a, 0x0b, 0x63, 0x6f, 0x6d, 0x6d, 0x69, 0x74, 0x52, 0x6f,
	0x75, 0x6e, 0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x03, 0x52, 0x0b, 0x63, 0x6f, 0x6d, 0x6d, 0x69,
	0x74, 0x52, 0x6f, 0x75, 0x6e, 0x64, 0x12, 0x16, 0x0a, 0x06, 0x70, 0x75, 0x62, 0x4b, 0x65, 0x79,
	0x18, 0x02, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x06, 0x70, 0x75, 0x62, 0x4b, 0x65, 0x79, 0x12, 0x1c,
	0x0a, 0x09, 0x73, 0x69, 0x67, 0x6e, 0x61, 0x74, 0x75, 0x72, 0x65, 0x18, 0x03, 0x20, 0x01, 0x28,
	0x0c, 0x52, 0x09, 0x73, 0x69, 0x67, 0x6e, 0x61, 0x74, 0x75, 0x72, 0x65, 0x12, 0x18, 0x0a, 0x07,
	0x6d, 0x73, 0x67, 0x48, 0x61, 0x73, 0x68, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x07, 0x6d,
	0x73, 0x67, 0x48, 0x61, 0x73, 0x68, 0x22, 0xd5, 0x01, 0x0a, 0x0a, 0x43, 0x68, 0x65, 0x63, 0x6b,
	0x50, 0x6f, 0x69, 0x6e, 0x74, 0x12, 0x1e, 0x0a, 0x0a, 0x63, 0x68, 0x61, 0x69, 0x6e, 0x54, 0x69,
	0x74, 0x6c, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0a, 0x63, 0x68, 0x61, 0x69, 0x6e,
	0x54, 0x69, 0x74, 0x6c, 0x65, 0x12, 0x20, 0x0a, 0x0b, 0x63, 0x6f, 0x6d, 0x6d, 0x69, 0x74, 0x52,
	0x6f, 0x75, 0x6e, 0x64, 0x18, 0x02, 0x20, 0x01, 0x28, 0x03, 0x52, 0x0b, 0x63, 0x6f, 0x6d, 0x6d,
	0x69, 0x74, 0x52, 0x6f, 0x75, 0x6e, 0x64, 0x12, 0x27, 0x0a, 0x05, 0x62, 0x61, 0x74, 0x63, 0x68,
	0x18, 0x03, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x11, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x42,
	0x6c, 0x6f, 0x63, 0x6b, 0x42, 0x61, 0x74, 0x63, 0x68, 0x52, 0x05, 0x62, 0x61, 0x74, 0x63, 0x68,
	0x12, 0x24, 0x0a, 0x0d, 0x76, 0x61, 0x6c, 0x69, 0x64, 0x61, 0x74, 0x6f, 0x72, 0x50, 0x75, 0x62,
	0x73, 0x18, 0x04, 0x20, 0x03, 0x28, 0x0c, 0x52, 0x0d, 0x76, 0x61, 0x6c, 0x69, 0x64, 0x61, 0x74,
	0x6f, 0x72, 0x50, 0x75, 0x62, 0x73, 0x12, 0x36, 0x0a, 0x16, 0x61, 0x67, 0x67, 0x72, 0x65, 0x67,
	0x61, 0x74, 0x65, 0x56, 0x61, 0x6c, 0x69, 0x64, 0x61, 0x74, 0x6f, 0x72, 0x53, 0x69, 0x67, 0x6e,
	0x18, 0x05, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x16, 0x61, 0x67, 0x67, 0x72, 0x65, 0x67, 0x61, 0x74,
	0x65, 0x56, 0x61, 0x6c, 0x69, 0x64, 0x61, 0x74, 0x6f, 0x72, 0x53, 0x69, 0x67, 0x6e, 0x22, 0xf0,
	0x01, 0x0a, 0x0c, 0x52, 0x6f, 0x6c, 0x6c, 0x75, 0x70, 0x53, 0x74, 0x61, 0x74, 0x75, 0x73, 0x12,
	0x1c, 0x0a, 0x09, 0x74, 0x69, 0x6d, 0x65, 0x73, 0x74, 0x61, 0x6d, 0x70, 0x18, 0x01, 0x20, 0x01,
	0x28, 0x03, 0x52, 0x09, 0x74, 0x69, 0x6d, 0x65, 0x73, 0x74, 0x61, 0x6d, 0x70, 0x12, 0x20, 0x0a,
	0x0b, 0x63, 0x6f, 0x6d, 0x6d, 0x69, 0x74, 0x52, 0x6f, 0x75, 0x6e, 0x64, 0x18, 0x02, 0x20, 0x01,
	0x28, 0x03, 0x52, 0x0b, 0x63, 0x6f, 0x6d, 0x6d, 0x69, 0x74, 0x52, 0x6f, 0x75, 0x6e, 0x64, 0x12,
	0x2c, 0x0a, 0x11, 0x63, 0x6f, 0x6d, 0x6d, 0x69, 0x74, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x65,
	0x69, 0x67, 0x68, 0x74, 0x18, 0x03, 0x20, 0x01, 0x28, 0x03, 0x52, 0x11, 0x63, 0x6f, 0x6d, 0x6d,
	0x69, 0x74, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x65, 0x69, 0x67, 0x68, 0x74, 0x12, 0x28, 0x0a,
	0x0f, 0x63, 0x6f, 0x6d, 0x6d, 0x69, 0x74, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x61, 0x73, 0x68,
	0x18, 0x04, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0f, 0x63, 0x6f, 0x6d, 0x6d, 0x69, 0x74, 0x42, 0x6c,
	0x6f, 0x63, 0x6b, 0x48, 0x61, 0x73, 0x68, 0x12, 0x1e, 0x0a, 0x0a, 0x63, 0x6f, 0x6d, 0x6d, 0x69,
	0x74, 0x41, 0x64, 0x64, 0x72, 0x18, 0x05, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0a, 0x63, 0x6f, 0x6d,
	0x6d, 0x69, 0x74, 0x41, 0x64, 0x64, 0x72, 0x12, 0x28, 0x0a, 0x0f, 0x63, 0x72, 0x6f, 0x73, 0x73,
	0x54, 0x78, 0x52, 0x6f, 0x6f, 0x74, 0x48, 0x61, 0x73, 0x68, 0x18, 0x06, 0x20, 0x01, 0x28, 0x0c,
	0x52, 0x0f, 0x63, 0x72, 0x6f, 0x73, 0x73, 0x54, 0x78, 0x52, 0x6f, 0x6f, 0x74, 0x48, 0x61, 0x73,
	0x68, 0x22, 0xad, 0x01, 0x0a, 0x0f, 0x43, 0x6f, 0x6d, 0x6d, 0x69, 0x74, 0x52, 0x6f, 0x75, 0x6e,
	0x64, 0x49, 0x6e, 0x66, 0x6f, 0x12, 0x20, 0x0a, 0x0b, 0x63, 0x6f, 0x6d, 0x6d, 0x69, 0x74, 0x52,
	0x6f, 0x75, 0x6e, 0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x03, 0x52, 0x0b, 0x63, 0x6f, 0x6d, 0x6d,
	0x69, 0x74, 0x52, 0x6f, 0x75, 0x6e, 0x64, 0x12, 0x28, 0x0a, 0x0f, 0x6c, 0x61, 0x73, 0x74, 0x42,
	0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x65, 0x69, 0x67, 0x68, 0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x03,
	0x52, 0x0f, 0x6c, 0x61, 0x73, 0x74, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x65, 0x69, 0x67, 0x68,
	0x74, 0x12, 0x28, 0x0a, 0x0f, 0x70, 0x61, 0x72, 0x65, 0x6e, 0x74, 0x42, 0x6c, 0x6f, 0x63, 0x6b,
	0x48, 0x61, 0x73, 0x68, 0x18, 0x03, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0f, 0x70, 0x61, 0x72, 0x65,
	0x6e, 0x74, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x61, 0x73, 0x68, 0x12, 0x24, 0x0a, 0x0d, 0x6c,
	0x61, 0x73, 0x74, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x61, 0x73, 0x68, 0x18, 0x04, 0x20, 0x01,
	0x28, 0x09, 0x52, 0x0d, 0x6c, 0x61, 0x73, 0x74, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x48, 0x61, 0x73,
	0x68, 0x22, 0x25, 0x0a, 0x03, 0x52, 0x65, 0x71, 0x12, 0x1e, 0x0a, 0x0a, 0x63, 0x68, 0x61, 0x69,
	0x6e, 0x54, 0x69, 0x74, 0x6c, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0a, 0x63, 0x68,
	0x61, 0x69, 0x6e, 0x54, 0x69, 0x74, 0x6c, 0x65, 0x22, 0x22, 0x0a, 0x0a, 0x63, 0x68, 0x61, 0x69,
	0x6e, 0x54, 0x69, 0x74, 0x6c, 0x65, 0x12, 0x14, 0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x18,
	0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x22, 0x29, 0x0a, 0x0d,
	0x56, 0x61, 0x6c, 0x69, 0x64, 0x61, 0x74, 0x6f, 0x72, 0x50, 0x75, 0x62, 0x73, 0x12, 0x18, 0x0a,
	0x07, 0x62, 0x6c, 0x73, 0x50, 0x75, 0x62, 0x73, 0x18, 0x01, 0x20, 0x03, 0x28, 0x09, 0x52, 0x07,
	0x62, 0x6c, 0x73, 0x50, 0x75, 0x62, 0x73, 0x32, 0x08, 0x0a, 0x06, 0x72, 0x6f, 0x6c, 0x6c, 0x75,
	0x70, 0x42, 0x0a, 0x5a, 0x08, 0x2e, 0x2e, 0x2f, 0x74, 0x79, 0x70, 0x65, 0x73, 0x62, 0x06, 0x70,
	0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_rollup_proto_rawDescOnce sync.Once
	file_rollup_proto_rawDescData = file_rollup_proto_rawDesc
)

func file_rollup_proto_rawDescGZIP() []byte {
	file_rollup_proto_rawDescOnce.Do(func() {
		file_rollup_proto_rawDescData = protoimpl.X.CompressGZIP(file_rollup_proto_rawDescData)
	})
	return file_rollup_proto_rawDescData
}

var file_rollup_proto_msgTypes = make([]protoimpl.MessageInfo, 9)
var file_rollup_proto_goTypes = []interface{}{
	(*RollupAction)(nil),     // 0: types.RollupAction
	(*BlockBatch)(nil),       // 1: types.BlockBatch
	(*ValidatorSignMsg)(nil), // 2: types.ValidatorSignMsg
	(*CheckPoint)(nil),       // 3: types.CheckPoint
	(*RollupStatus)(nil),     // 4: types.RollupStatus
	(*CommitRoundInfo)(nil),  // 5: types.CommitRoundInfo
	(*Req)(nil),              // 6: types.Req
	(*ChainTitle)(nil),       // 7: types.chainTitle
	(*ValidatorPubs)(nil),    // 8: types.ValidatorPubs
	(*types.Header)(nil),     // 9: types.Header
}
var file_rollup_proto_depIdxs = []int32{
	3, // 0: types.RollupAction.commit:type_name -> types.CheckPoint
	9, // 1: types.BlockBatch.blockHeaders:type_name -> types.Header
	1, // 2: types.CheckPoint.batch:type_name -> types.BlockBatch
	3, // [3:3] is the sub-list for method output_type
	3, // [3:3] is the sub-list for method input_type
	3, // [3:3] is the sub-list for extension type_name
	3, // [3:3] is the sub-list for extension extendee
	0, // [0:3] is the sub-list for field type_name
}

func init() { file_rollup_proto_init() }
func file_rollup_proto_init() {
	if File_rollup_proto != nil {
		return
	}
	if !protoimpl.UnsafeEnabled {
		file_rollup_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*RollupAction); i {
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
		file_rollup_proto_msgTypes[1].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*BlockBatch); i {
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
		file_rollup_proto_msgTypes[2].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ValidatorSignMsg); i {
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
		file_rollup_proto_msgTypes[3].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*CheckPoint); i {
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
		file_rollup_proto_msgTypes[4].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*RollupStatus); i {
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
		file_rollup_proto_msgTypes[5].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*CommitRoundInfo); i {
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
		file_rollup_proto_msgTypes[6].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*Req); i {
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
		file_rollup_proto_msgTypes[7].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ChainTitle); i {
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
		file_rollup_proto_msgTypes[8].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ValidatorPubs); i {
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
	file_rollup_proto_msgTypes[0].OneofWrappers = []interface{}{
		(*RollupAction_Commit)(nil),
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_rollup_proto_rawDesc,
			NumEnums:      0,
			NumMessages:   9,
			NumExtensions: 0,
			NumServices:   1,
		},
		GoTypes:           file_rollup_proto_goTypes,
		DependencyIndexes: file_rollup_proto_depIdxs,
		MessageInfos:      file_rollup_proto_msgTypes,
	}.Build()
	File_rollup_proto = out.File
	file_rollup_proto_rawDesc = nil
	file_rollup_proto_goTypes = nil
	file_rollup_proto_depIdxs = nil
}

// Reference imports to suppress errors if they are not otherwise used.
var _ context.Context
var _ grpc.ClientConnInterface

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
const _ = grpc.SupportPackageIsVersion6

// RollupClient is the client API for Rollup service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://godoc.org/google.golang.org/grpc#ClientConn.NewStream.
type RollupClient interface {
}

type rollupClient struct {
	cc grpc.ClientConnInterface
}

func NewRollupClient(cc grpc.ClientConnInterface) RollupClient {
	return &rollupClient{cc}
}

// RollupServer is the server API for Rollup service.
type RollupServer interface {
}

// UnimplementedRollupServer can be embedded to have forward compatible implementations.
type UnimplementedRollupServer struct {
}

func RegisterRollupServer(s *grpc.Server, srv RollupServer) {
	s.RegisterService(&_Rollup_serviceDesc, srv)
}

var _Rollup_serviceDesc = grpc.ServiceDesc{
	ServiceName: "types.rollup",
	HandlerType: (*RollupServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams:     []grpc.StreamDesc{},
	Metadata:    "rollup.proto",
}
