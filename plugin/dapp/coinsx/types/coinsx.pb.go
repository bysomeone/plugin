// Code generated by protoc-gen-go. DO NOT EDIT.
// source: coinsx.proto

package types

import (
	fmt "fmt"
	types "github.com/33cn/chain33/types"
	proto "github.com/golang/protobuf/proto"
	math "math"
)

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto.ProtoPackageIsVersion3 // please upgrade the proto package

//kvmvcc statedb not support 0 value
type TransferFlag int32

const (
	TransferFlag_NONE    TransferFlag = 0
	TransferFlag_ENABLE  TransferFlag = 1
	TransferFlag_DISABLE TransferFlag = 2
)

var TransferFlag_name = map[int32]string{
	0: "NONE",
	1: "ENABLE",
	2: "DISABLE",
}

var TransferFlag_value = map[string]int32{
	"NONE":    0,
	"ENABLE":  1,
	"DISABLE": 2,
}

func (x TransferFlag) String() string {
	return proto.EnumName(TransferFlag_name, int32(x))
}

func (TransferFlag) EnumDescriptor() ([]byte, []int) {
	return fileDescriptor_f07af7065b15e440, []int{0}
}

type AccountOp int32

const (
	AccountOp_ADD AccountOp = 0
	AccountOp_DEL AccountOp = 1
)

var AccountOp_name = map[int32]string{
	0: "ADD",
	1: "DEL",
}

var AccountOp_value = map[string]int32{
	"ADD": 0,
	"DEL": 1,
}

func (x AccountOp) String() string {
	return proto.EnumName(AccountOp_name, int32(x))
}

func (AccountOp) EnumDescriptor() ([]byte, []int) {
	return fileDescriptor_f07af7065b15e440, []int{1}
}

type ConfigType int32

const (
	ConfigType_INVALID  ConfigType = 0
	ConfigType_TRANSFER ConfigType = 1
	ConfigType_ACCOUNTS ConfigType = 2
)

var ConfigType_name = map[int32]string{
	0: "INVALID",
	1: "TRANSFER",
	2: "ACCOUNTS",
}

var ConfigType_value = map[string]int32{
	"INVALID":  0,
	"TRANSFER": 1,
	"ACCOUNTS": 2,
}

func (x ConfigType) String() string {
	return proto.EnumName(ConfigType_name, int32(x))
}

func (ConfigType) EnumDescriptor() ([]byte, []int) {
	return fileDescriptor_f07af7065b15e440, []int{2}
}

// message for execs.coinsx
type CoinsxAction struct {
	// Types that are valid to be assigned to Value:
	//	*CoinsxAction_Transfer
	//	*CoinsxAction_Withdraw
	//	*CoinsxAction_Genesis
	//	*CoinsxAction_TransferToExec
	//	*CoinsxAction_Config
	Value                isCoinsxAction_Value `protobuf_oneof:"value"`
	Ty                   int32                `protobuf:"varint,3,opt,name=ty,proto3" json:"ty,omitempty"`
	XXX_NoUnkeyedLiteral struct{}             `json:"-"`
	XXX_unrecognized     []byte               `json:"-"`
	XXX_sizecache        int32                `json:"-"`
}

func (m *CoinsxAction) Reset()         { *m = CoinsxAction{} }
func (m *CoinsxAction) String() string { return proto.CompactTextString(m) }
func (*CoinsxAction) ProtoMessage()    {}
func (*CoinsxAction) Descriptor() ([]byte, []int) {
	return fileDescriptor_f07af7065b15e440, []int{0}
}

func (m *CoinsxAction) XXX_Unmarshal(b []byte) error {
	return xxx_messageInfo_CoinsxAction.Unmarshal(m, b)
}
func (m *CoinsxAction) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	return xxx_messageInfo_CoinsxAction.Marshal(b, m, deterministic)
}
func (m *CoinsxAction) XXX_Merge(src proto.Message) {
	xxx_messageInfo_CoinsxAction.Merge(m, src)
}
func (m *CoinsxAction) XXX_Size() int {
	return xxx_messageInfo_CoinsxAction.Size(m)
}
func (m *CoinsxAction) XXX_DiscardUnknown() {
	xxx_messageInfo_CoinsxAction.DiscardUnknown(m)
}

var xxx_messageInfo_CoinsxAction proto.InternalMessageInfo

type isCoinsxAction_Value interface {
	isCoinsxAction_Value()
}

type CoinsxAction_Transfer struct {
	Transfer *types.AssetsTransfer `protobuf:"bytes,1,opt,name=transfer,proto3,oneof"`
}

type CoinsxAction_Withdraw struct {
	Withdraw *types.AssetsWithdraw `protobuf:"bytes,4,opt,name=withdraw,proto3,oneof"`
}

type CoinsxAction_Genesis struct {
	Genesis *types.AssetsGenesis `protobuf:"bytes,2,opt,name=genesis,proto3,oneof"`
}

type CoinsxAction_TransferToExec struct {
	TransferToExec *types.AssetsTransferToExec `protobuf:"bytes,5,opt,name=transferToExec,proto3,oneof"`
}

type CoinsxAction_Config struct {
	Config *CoinsConfig `protobuf:"bytes,6,opt,name=config,proto3,oneof"`
}

func (*CoinsxAction_Transfer) isCoinsxAction_Value() {}

func (*CoinsxAction_Withdraw) isCoinsxAction_Value() {}

func (*CoinsxAction_Genesis) isCoinsxAction_Value() {}

func (*CoinsxAction_TransferToExec) isCoinsxAction_Value() {}

func (*CoinsxAction_Config) isCoinsxAction_Value() {}

func (m *CoinsxAction) GetValue() isCoinsxAction_Value {
	if m != nil {
		return m.Value
	}
	return nil
}

func (m *CoinsxAction) GetTransfer() *types.AssetsTransfer {
	if x, ok := m.GetValue().(*CoinsxAction_Transfer); ok {
		return x.Transfer
	}
	return nil
}

func (m *CoinsxAction) GetWithdraw() *types.AssetsWithdraw {
	if x, ok := m.GetValue().(*CoinsxAction_Withdraw); ok {
		return x.Withdraw
	}
	return nil
}

func (m *CoinsxAction) GetGenesis() *types.AssetsGenesis {
	if x, ok := m.GetValue().(*CoinsxAction_Genesis); ok {
		return x.Genesis
	}
	return nil
}

func (m *CoinsxAction) GetTransferToExec() *types.AssetsTransferToExec {
	if x, ok := m.GetValue().(*CoinsxAction_TransferToExec); ok {
		return x.TransferToExec
	}
	return nil
}

func (m *CoinsxAction) GetConfig() *CoinsConfig {
	if x, ok := m.GetValue().(*CoinsxAction_Config); ok {
		return x.Config
	}
	return nil
}

func (m *CoinsxAction) GetTy() int32 {
	if m != nil {
		return m.Ty
	}
	return 0
}

// XXX_OneofWrappers is for the internal use of the proto package.
func (*CoinsxAction) XXX_OneofWrappers() []interface{} {
	return []interface{}{
		(*CoinsxAction_Transfer)(nil),
		(*CoinsxAction_Withdraw)(nil),
		(*CoinsxAction_Genesis)(nil),
		(*CoinsxAction_TransferToExec)(nil),
		(*CoinsxAction_Config)(nil),
	}
}

type TransferFlagConfig struct {
	Flag                 TransferFlag `protobuf:"varint,1,opt,name=flag,proto3,enum=types.TransferFlag" json:"flag,omitempty"`
	XXX_NoUnkeyedLiteral struct{}     `json:"-"`
	XXX_unrecognized     []byte       `json:"-"`
	XXX_sizecache        int32        `json:"-"`
}

func (m *TransferFlagConfig) Reset()         { *m = TransferFlagConfig{} }
func (m *TransferFlagConfig) String() string { return proto.CompactTextString(m) }
func (*TransferFlagConfig) ProtoMessage()    {}
func (*TransferFlagConfig) Descriptor() ([]byte, []int) {
	return fileDescriptor_f07af7065b15e440, []int{1}
}

func (m *TransferFlagConfig) XXX_Unmarshal(b []byte) error {
	return xxx_messageInfo_TransferFlagConfig.Unmarshal(m, b)
}
func (m *TransferFlagConfig) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	return xxx_messageInfo_TransferFlagConfig.Marshal(b, m, deterministic)
}
func (m *TransferFlagConfig) XXX_Merge(src proto.Message) {
	xxx_messageInfo_TransferFlagConfig.Merge(m, src)
}
func (m *TransferFlagConfig) XXX_Size() int {
	return xxx_messageInfo_TransferFlagConfig.Size(m)
}
func (m *TransferFlagConfig) XXX_DiscardUnknown() {
	xxx_messageInfo_TransferFlagConfig.DiscardUnknown(m)
}

var xxx_messageInfo_TransferFlagConfig proto.InternalMessageInfo

func (m *TransferFlagConfig) GetFlag() TransferFlag {
	if m != nil {
		return m.Flag
	}
	return TransferFlag_NONE
}

type ManagerStatus struct {
	TransferFlag         TransferFlag `protobuf:"varint,1,opt,name=transferFlag,proto3,enum=types.TransferFlag" json:"transferFlag,omitempty"`
	ManagerAccounts      []string     `protobuf:"bytes,2,rep,name=managerAccounts,proto3" json:"managerAccounts,omitempty"`
	XXX_NoUnkeyedLiteral struct{}     `json:"-"`
	XXX_unrecognized     []byte       `json:"-"`
	XXX_sizecache        int32        `json:"-"`
}

func (m *ManagerStatus) Reset()         { *m = ManagerStatus{} }
func (m *ManagerStatus) String() string { return proto.CompactTextString(m) }
func (*ManagerStatus) ProtoMessage()    {}
func (*ManagerStatus) Descriptor() ([]byte, []int) {
	return fileDescriptor_f07af7065b15e440, []int{2}
}

func (m *ManagerStatus) XXX_Unmarshal(b []byte) error {
	return xxx_messageInfo_ManagerStatus.Unmarshal(m, b)
}
func (m *ManagerStatus) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	return xxx_messageInfo_ManagerStatus.Marshal(b, m, deterministic)
}
func (m *ManagerStatus) XXX_Merge(src proto.Message) {
	xxx_messageInfo_ManagerStatus.Merge(m, src)
}
func (m *ManagerStatus) XXX_Size() int {
	return xxx_messageInfo_ManagerStatus.Size(m)
}
func (m *ManagerStatus) XXX_DiscardUnknown() {
	xxx_messageInfo_ManagerStatus.DiscardUnknown(m)
}

var xxx_messageInfo_ManagerStatus proto.InternalMessageInfo

func (m *ManagerStatus) GetTransferFlag() TransferFlag {
	if m != nil {
		return m.TransferFlag
	}
	return TransferFlag_NONE
}

func (m *ManagerStatus) GetManagerAccounts() []string {
	if m != nil {
		return m.ManagerAccounts
	}
	return nil
}

type ReceiptManagerStatus struct {
	Prev                 *ManagerStatus `protobuf:"bytes,2,opt,name=prev,proto3" json:"prev,omitempty"`
	Curr                 *ManagerStatus `protobuf:"bytes,3,opt,name=curr,proto3" json:"curr,omitempty"`
	XXX_NoUnkeyedLiteral struct{}       `json:"-"`
	XXX_unrecognized     []byte         `json:"-"`
	XXX_sizecache        int32          `json:"-"`
}

func (m *ReceiptManagerStatus) Reset()         { *m = ReceiptManagerStatus{} }
func (m *ReceiptManagerStatus) String() string { return proto.CompactTextString(m) }
func (*ReceiptManagerStatus) ProtoMessage()    {}
func (*ReceiptManagerStatus) Descriptor() ([]byte, []int) {
	return fileDescriptor_f07af7065b15e440, []int{3}
}

func (m *ReceiptManagerStatus) XXX_Unmarshal(b []byte) error {
	return xxx_messageInfo_ReceiptManagerStatus.Unmarshal(m, b)
}
func (m *ReceiptManagerStatus) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	return xxx_messageInfo_ReceiptManagerStatus.Marshal(b, m, deterministic)
}
func (m *ReceiptManagerStatus) XXX_Merge(src proto.Message) {
	xxx_messageInfo_ReceiptManagerStatus.Merge(m, src)
}
func (m *ReceiptManagerStatus) XXX_Size() int {
	return xxx_messageInfo_ReceiptManagerStatus.Size(m)
}
func (m *ReceiptManagerStatus) XXX_DiscardUnknown() {
	xxx_messageInfo_ReceiptManagerStatus.DiscardUnknown(m)
}

var xxx_messageInfo_ReceiptManagerStatus proto.InternalMessageInfo

func (m *ReceiptManagerStatus) GetPrev() *ManagerStatus {
	if m != nil {
		return m.Prev
	}
	return nil
}

func (m *ReceiptManagerStatus) GetCurr() *ManagerStatus {
	if m != nil {
		return m.Curr
	}
	return nil
}

type ManagerAccountsConfig struct {
	Op                   AccountOp `protobuf:"varint,1,opt,name=op,proto3,enum=types.AccountOp" json:"op,omitempty"`
	Accounts             string    `protobuf:"bytes,2,opt,name=accounts,proto3" json:"accounts,omitempty"`
	XXX_NoUnkeyedLiteral struct{}  `json:"-"`
	XXX_unrecognized     []byte    `json:"-"`
	XXX_sizecache        int32     `json:"-"`
}

func (m *ManagerAccountsConfig) Reset()         { *m = ManagerAccountsConfig{} }
func (m *ManagerAccountsConfig) String() string { return proto.CompactTextString(m) }
func (*ManagerAccountsConfig) ProtoMessage()    {}
func (*ManagerAccountsConfig) Descriptor() ([]byte, []int) {
	return fileDescriptor_f07af7065b15e440, []int{4}
}

func (m *ManagerAccountsConfig) XXX_Unmarshal(b []byte) error {
	return xxx_messageInfo_ManagerAccountsConfig.Unmarshal(m, b)
}
func (m *ManagerAccountsConfig) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	return xxx_messageInfo_ManagerAccountsConfig.Marshal(b, m, deterministic)
}
func (m *ManagerAccountsConfig) XXX_Merge(src proto.Message) {
	xxx_messageInfo_ManagerAccountsConfig.Merge(m, src)
}
func (m *ManagerAccountsConfig) XXX_Size() int {
	return xxx_messageInfo_ManagerAccountsConfig.Size(m)
}
func (m *ManagerAccountsConfig) XXX_DiscardUnknown() {
	xxx_messageInfo_ManagerAccountsConfig.DiscardUnknown(m)
}

var xxx_messageInfo_ManagerAccountsConfig proto.InternalMessageInfo

func (m *ManagerAccountsConfig) GetOp() AccountOp {
	if m != nil {
		return m.Op
	}
	return AccountOp_ADD
}

func (m *ManagerAccountsConfig) GetAccounts() string {
	if m != nil {
		return m.Accounts
	}
	return ""
}

type CoinsConfig struct {
	Ty ConfigType `protobuf:"varint,1,opt,name=ty,proto3,enum=types.ConfigType" json:"ty,omitempty"`
	// Types that are valid to be assigned to Value:
	//	*CoinsConfig_TransferFlag
	//	*CoinsConfig_ManagerAccounts
	Value                isCoinsConfig_Value `protobuf_oneof:"value"`
	XXX_NoUnkeyedLiteral struct{}            `json:"-"`
	XXX_unrecognized     []byte              `json:"-"`
	XXX_sizecache        int32               `json:"-"`
}

func (m *CoinsConfig) Reset()         { *m = CoinsConfig{} }
func (m *CoinsConfig) String() string { return proto.CompactTextString(m) }
func (*CoinsConfig) ProtoMessage()    {}
func (*CoinsConfig) Descriptor() ([]byte, []int) {
	return fileDescriptor_f07af7065b15e440, []int{5}
}

func (m *CoinsConfig) XXX_Unmarshal(b []byte) error {
	return xxx_messageInfo_CoinsConfig.Unmarshal(m, b)
}
func (m *CoinsConfig) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	return xxx_messageInfo_CoinsConfig.Marshal(b, m, deterministic)
}
func (m *CoinsConfig) XXX_Merge(src proto.Message) {
	xxx_messageInfo_CoinsConfig.Merge(m, src)
}
func (m *CoinsConfig) XXX_Size() int {
	return xxx_messageInfo_CoinsConfig.Size(m)
}
func (m *CoinsConfig) XXX_DiscardUnknown() {
	xxx_messageInfo_CoinsConfig.DiscardUnknown(m)
}

var xxx_messageInfo_CoinsConfig proto.InternalMessageInfo

func (m *CoinsConfig) GetTy() ConfigType {
	if m != nil {
		return m.Ty
	}
	return ConfigType_INVALID
}

type isCoinsConfig_Value interface {
	isCoinsConfig_Value()
}

type CoinsConfig_TransferFlag struct {
	TransferFlag *TransferFlagConfig `protobuf:"bytes,2,opt,name=transferFlag,proto3,oneof"`
}

type CoinsConfig_ManagerAccounts struct {
	ManagerAccounts *ManagerAccountsConfig `protobuf:"bytes,3,opt,name=managerAccounts,proto3,oneof"`
}

func (*CoinsConfig_TransferFlag) isCoinsConfig_Value() {}

func (*CoinsConfig_ManagerAccounts) isCoinsConfig_Value() {}

func (m *CoinsConfig) GetValue() isCoinsConfig_Value {
	if m != nil {
		return m.Value
	}
	return nil
}

func (m *CoinsConfig) GetTransferFlag() *TransferFlagConfig {
	if x, ok := m.GetValue().(*CoinsConfig_TransferFlag); ok {
		return x.TransferFlag
	}
	return nil
}

func (m *CoinsConfig) GetManagerAccounts() *ManagerAccountsConfig {
	if x, ok := m.GetValue().(*CoinsConfig_ManagerAccounts); ok {
		return x.ManagerAccounts
	}
	return nil
}

// XXX_OneofWrappers is for the internal use of the proto package.
func (*CoinsConfig) XXX_OneofWrappers() []interface{} {
	return []interface{}{
		(*CoinsConfig_TransferFlag)(nil),
		(*CoinsConfig_ManagerAccounts)(nil),
	}
}

func init() {
	proto.RegisterEnum("types.TransferFlag", TransferFlag_name, TransferFlag_value)
	proto.RegisterEnum("types.AccountOp", AccountOp_name, AccountOp_value)
	proto.RegisterEnum("types.ConfigType", ConfigType_name, ConfigType_value)
	proto.RegisterType((*CoinsxAction)(nil), "types.CoinsxAction")
	proto.RegisterType((*TransferFlagConfig)(nil), "types.TransferFlagConfig")
	proto.RegisterType((*ManagerStatus)(nil), "types.ManagerStatus")
	proto.RegisterType((*ReceiptManagerStatus)(nil), "types.ReceiptManagerStatus")
	proto.RegisterType((*ManagerAccountsConfig)(nil), "types.ManagerAccountsConfig")
	proto.RegisterType((*CoinsConfig)(nil), "types.CoinsConfig")
}

func init() {
	proto.RegisterFile("coinsx.proto", fileDescriptor_f07af7065b15e440)
}

var fileDescriptor_f07af7065b15e440 = []byte{
	// 528 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0x7c, 0x94, 0xdf, 0x6e, 0xd3, 0x30,
	0x14, 0xc6, 0x93, 0xac, 0x7f, 0x4f, 0x4b, 0xc9, 0xcc, 0x26, 0x85, 0x01, 0x52, 0xc9, 0x0d, 0x51,
	0x85, 0x2a, 0xd8, 0x84, 0xb8, 0x42, 0x28, 0x6d, 0x33, 0x52, 0xa9, 0x4b, 0x25, 0xb7, 0x03, 0x89,
	0x3b, 0x13, 0xdc, 0x10, 0x54, 0x92, 0x28, 0x71, 0xb7, 0xf5, 0xcd, 0x78, 0x00, 0x1e, 0x0c, 0xd9,
	0x4e, 0xb2, 0xa6, 0x2b, 0xdc, 0xd5, 0x3e, 0xbf, 0xef, 0x9c, 0x9e, 0xef, 0x8b, 0x0c, 0x5d, 0x3f,
	0x0e, 0xa3, 0xec, 0x6e, 0x98, 0xa4, 0x31, 0x8b, 0x51, 0x9d, 0x6d, 0x13, 0x9a, 0x9d, 0x1d, 0xb3,
	0x94, 0x44, 0x19, 0xf1, 0x59, 0x18, 0x47, 0xb2, 0x62, 0xfe, 0xd6, 0xa0, 0x3b, 0x16, 0xa8, 0x2d,
	0xae, 0xd1, 0x05, 0xb4, 0x04, 0xb5, 0xa2, 0xa9, 0xa1, 0xf6, 0x55, 0xab, 0x73, 0x7e, 0x3a, 0x14,
	0xea, 0xa1, 0x9d, 0x65, 0x94, 0x65, 0xcb, 0xbc, 0xe8, 0x2a, 0xb8, 0x04, 0xb9, 0xe8, 0x36, 0x64,
	0x3f, 0xbe, 0xa7, 0xe4, 0xd6, 0xa8, 0x1d, 0x10, 0x7d, 0xc9, 0x8b, 0x5c, 0x54, 0x80, 0xe8, 0x0d,
	0x34, 0x03, 0x1a, 0xd1, 0x2c, 0xcc, 0x0c, 0x4d, 0x68, 0x4e, 0x2a, 0x9a, 0x4f, 0xb2, 0xe6, 0x2a,
	0xb8, 0xc0, 0x90, 0x03, 0xbd, 0x62, 0xe4, 0x32, 0x76, 0xee, 0xa8, 0x6f, 0xd4, 0x85, 0xf0, 0xd9,
	0xc1, 0x7f, 0x28, 0x11, 0x57, 0xc1, 0x7b, 0x22, 0xf4, 0x1a, 0x1a, 0x7e, 0x1c, 0xad, 0xc2, 0xc0,
	0x68, 0x08, 0x39, 0xca, 0xe5, 0xc2, 0x87, 0xb1, 0xa8, 0xb8, 0x0a, 0xce, 0x19, 0xd4, 0x03, 0x8d,
	0x6d, 0x8d, 0xa3, 0xbe, 0x6a, 0xd5, 0xb1, 0xc6, 0xb6, 0xa3, 0x26, 0xd4, 0x6f, 0xc8, 0x7a, 0x43,
	0xcd, 0x0f, 0x80, 0x8a, 0x51, 0x97, 0x6b, 0x12, 0x48, 0x21, 0x7a, 0x05, 0xb5, 0xd5, 0x9a, 0x04,
	0xc2, 0xbb, 0xde, 0xf9, 0x93, 0xbc, 0xf5, 0x2e, 0x88, 0x05, 0x60, 0xa6, 0xf0, 0xe8, 0x8a, 0x44,
	0x24, 0xa0, 0xe9, 0x82, 0x11, 0xb6, 0xc9, 0xd0, 0x7b, 0xe8, 0xb2, 0x1d, 0xec, 0x7f, 0x1d, 0x2a,
	0x20, 0xb2, 0xe0, 0xf1, 0x2f, 0xd9, 0xc9, 0xf6, 0xfd, 0x78, 0x13, 0x31, 0x6e, 0xe8, 0x91, 0xd5,
	0xc6, 0xfb, 0xd7, 0xe6, 0x4f, 0x38, 0xc1, 0xd4, 0xa7, 0x61, 0xc2, 0xaa, 0xa3, 0x2d, 0xa8, 0x25,
	0x29, 0xbd, 0xd9, 0xcb, 0xa1, 0xc2, 0x60, 0x41, 0x70, 0xd2, 0xdf, 0xa4, 0xa9, 0xf0, 0xe3, 0x9f,
	0x24, 0x27, 0xcc, 0x6b, 0x38, 0xbd, 0xaa, 0x8e, 0xcf, 0x1d, 0xea, 0x83, 0x16, 0x27, 0xf9, 0x76,
	0x7a, 0x91, 0x9c, 0x44, 0xe6, 0x09, 0xd6, 0xe2, 0x04, 0x9d, 0x41, 0x8b, 0xdc, 0x6f, 0xa2, 0x5a,
	0x6d, 0x5c, 0x9e, 0xcd, 0x3f, 0x2a, 0x74, 0x76, 0x82, 0x42, 0x2f, 0x45, 0x3c, 0xb2, 0xdb, 0x71,
	0x19, 0x24, 0x2f, 0x2d, 0xb7, 0x09, 0xe5, 0x89, 0xa1, 0x8f, 0x7b, 0xc6, 0xca, 0x2d, 0x9f, 0x1e,
	0x30, 0xb6, 0x0c, 0xbf, 0x6a, 0xb0, 0xfb, 0xd0, 0x60, 0xb9, 0xff, 0xf3, 0xea, 0xfe, 0xd5, 0x45,
	0x5d, 0xe5, 0x41, 0x00, 0xe5, 0xc7, 0x33, 0x78, 0x0b, 0xdd, 0xdd, 0xc1, 0xa8, 0x05, 0x35, 0x6f,
	0xee, 0x39, 0xba, 0x82, 0x00, 0x1a, 0x8e, 0x67, 0x8f, 0x66, 0x8e, 0xae, 0xa2, 0x0e, 0x34, 0x27,
	0xd3, 0x85, 0x38, 0x68, 0x83, 0x17, 0xd0, 0x2e, 0x6d, 0x42, 0x4d, 0x38, 0xb2, 0x27, 0x13, 0x5d,
	0xe1, 0x3f, 0x26, 0xce, 0x4c, 0x57, 0x07, 0xef, 0x00, 0xee, 0xf7, 0xe6, 0xca, 0xa9, 0xf7, 0xd9,
	0x9e, 0x4d, 0x39, 0xd3, 0x85, 0xd6, 0x12, 0xdb, 0xde, 0xe2, 0xd2, 0xc1, 0xba, 0xca, 0x4f, 0xf6,
	0x78, 0x3c, 0xbf, 0xf6, 0x96, 0x0b, 0x5d, 0x1b, 0x35, 0xbf, 0xca, 0xc7, 0xe1, 0x5b, 0x43, 0x3c,
	0x08, 0x17, 0x7f, 0x03, 0x00, 0x00, 0xff, 0xff, 0x6f, 0x23, 0x0f, 0x23, 0x3a, 0x04, 0x00, 0x00,
}
