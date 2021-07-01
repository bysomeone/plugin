// Code generated by protoc-gen-go. DO NOT EDIT.
// source: ethoracle.proto

package types

import (
	fmt "fmt"
	math "math"

	proto "github.com/golang/protobuf/proto"
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

// EthBridgeClaim is a structure that contains all the data for a particular bridge claim
type OracleClaim struct {
	ID                   string   `protobuf:"bytes,1,opt,name=ID,proto3" json:"ID,omitempty"`
	ValidatorAddress     string   `protobuf:"bytes,2,opt,name=ValidatorAddress,proto3" json:"ValidatorAddress,omitempty"`
	Content              string   `protobuf:"bytes,3,opt,name=Content,proto3" json:"Content,omitempty"`
	XXX_NoUnkeyedLiteral struct{} `json:"-"`
	XXX_unrecognized     []byte   `json:"-"`
	XXX_sizecache        int32    `json:"-"`
}

func (m *OracleClaim) Reset()         { *m = OracleClaim{} }
func (m *OracleClaim) String() string { return proto.CompactTextString(m) }
func (*OracleClaim) ProtoMessage()    {}
func (*OracleClaim) Descriptor() ([]byte, []int) {
	return fileDescriptor_5f8f811c6e0fbec0, []int{0}
}

func (m *OracleClaim) XXX_Unmarshal(b []byte) error {
	return xxx_messageInfo_OracleClaim.Unmarshal(m, b)
}
func (m *OracleClaim) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	return xxx_messageInfo_OracleClaim.Marshal(b, m, deterministic)
}
func (m *OracleClaim) XXX_Merge(src proto.Message) {
	xxx_messageInfo_OracleClaim.Merge(m, src)
}
func (m *OracleClaim) XXX_Size() int {
	return xxx_messageInfo_OracleClaim.Size(m)
}
func (m *OracleClaim) XXX_DiscardUnknown() {
	xxx_messageInfo_OracleClaim.DiscardUnknown(m)
}

var xxx_messageInfo_OracleClaim proto.InternalMessageInfo

func (m *OracleClaim) GetID() string {
	if m != nil {
		return m.ID
	}
	return ""
}

func (m *OracleClaim) GetValidatorAddress() string {
	if m != nil {
		return m.ValidatorAddress
	}
	return ""
}

func (m *OracleClaim) GetContent() string {
	if m != nil {
		return m.Content
	}
	return ""
}

type Prophecy struct {
	ID                   string             `protobuf:"bytes,1,opt,name=ID,proto3" json:"ID,omitempty"`
	Status               *ProphecyStatus    `protobuf:"bytes,2,opt,name=Status,proto3" json:"Status,omitempty"`
	ClaimValidators      []*ClaimValidators `protobuf:"bytes,3,rep,name=ClaimValidators,proto3" json:"ClaimValidators,omitempty"`
	ValidatorClaims      []*ValidatorClaims `protobuf:"bytes,4,rep,name=ValidatorClaims,proto3" json:"ValidatorClaims,omitempty"`
	XXX_NoUnkeyedLiteral struct{}           `json:"-"`
	XXX_unrecognized     []byte             `json:"-"`
	XXX_sizecache        int32              `json:"-"`
}

func (m *Prophecy) Reset()         { *m = Prophecy{} }
func (m *Prophecy) String() string { return proto.CompactTextString(m) }
func (*Prophecy) ProtoMessage()    {}
func (*Prophecy) Descriptor() ([]byte, []int) {
	return fileDescriptor_5f8f811c6e0fbec0, []int{1}
}

func (m *Prophecy) XXX_Unmarshal(b []byte) error {
	return xxx_messageInfo_Prophecy.Unmarshal(m, b)
}
func (m *Prophecy) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	return xxx_messageInfo_Prophecy.Marshal(b, m, deterministic)
}
func (m *Prophecy) XXX_Merge(src proto.Message) {
	xxx_messageInfo_Prophecy.Merge(m, src)
}
func (m *Prophecy) XXX_Size() int {
	return xxx_messageInfo_Prophecy.Size(m)
}
func (m *Prophecy) XXX_DiscardUnknown() {
	xxx_messageInfo_Prophecy.DiscardUnknown(m)
}

var xxx_messageInfo_Prophecy proto.InternalMessageInfo

func (m *Prophecy) GetID() string {
	if m != nil {
		return m.ID
	}
	return ""
}

func (m *Prophecy) GetStatus() *ProphecyStatus {
	if m != nil {
		return m.Status
	}
	return nil
}

func (m *Prophecy) GetClaimValidators() []*ClaimValidators {
	if m != nil {
		return m.ClaimValidators
	}
	return nil
}

func (m *Prophecy) GetValidatorClaims() []*ValidatorClaims {
	if m != nil {
		return m.ValidatorClaims
	}
	return nil
}

func init() {
	proto.RegisterType((*OracleClaim)(nil), "types.OracleClaim")
	proto.RegisterType((*Prophecy)(nil), "types.Prophecy")
}

func init() {
	proto.RegisterFile("ethoracle.proto", fileDescriptor_5f8f811c6e0fbec0)
}

var fileDescriptor_5f8f811c6e0fbec0 = []byte{
	// 218 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0xe2, 0xe2, 0x4f, 0x2d, 0xc9, 0xc8,
	0x2f, 0x4a, 0x4c, 0xce, 0x49, 0xd5, 0x2b, 0x28, 0xca, 0x2f, 0xc9, 0x17, 0x62, 0x2d, 0xa9, 0x2c,
	0x48, 0x2d, 0x96, 0x12, 0xa8, 0x30, 0x4a, 0x2d, 0xc9, 0x48, 0x2d, 0x4a, 0x2d, 0xcd, 0x85, 0x48,
	0x28, 0x25, 0x73, 0x71, 0xfb, 0x83, 0x15, 0x3a, 0xe7, 0x24, 0x66, 0xe6, 0x0a, 0xf1, 0x71, 0x31,
	0x79, 0xba, 0x48, 0x30, 0x2a, 0x30, 0x6a, 0x70, 0x06, 0x31, 0x79, 0xba, 0x08, 0x69, 0x71, 0x09,
	0x84, 0x25, 0xe6, 0x64, 0xa6, 0x24, 0x96, 0xe4, 0x17, 0x39, 0xa6, 0xa4, 0x14, 0xa5, 0x16, 0x17,
	0x4b, 0x30, 0x81, 0x65, 0x31, 0xc4, 0x85, 0x24, 0xb8, 0xd8, 0x9d, 0xf3, 0xf3, 0x4a, 0x52, 0xf3,
	0x4a, 0x24, 0x98, 0xc1, 0x4a, 0x60, 0x5c, 0xa5, 0xb3, 0x8c, 0x5c, 0x1c, 0x01, 0x45, 0xf9, 0x05,
	0x19, 0xa9, 0xc9, 0x95, 0x18, 0x56, 0xe8, 0x72, 0xb1, 0x05, 0x97, 0x24, 0x96, 0x94, 0x42, 0x0c,
	0xe6, 0x36, 0x12, 0xd5, 0x03, 0xbb, 0x55, 0x0f, 0xa6, 0x01, 0x22, 0x19, 0x04, 0x55, 0x24, 0xe4,
	0xc0, 0xc5, 0x0f, 0x76, 0x2a, 0xdc, 0xfa, 0x62, 0x09, 0x66, 0x05, 0x66, 0x0d, 0x6e, 0x23, 0x31,
	0xa8, 0x3e, 0x34, 0xd9, 0x20, 0x74, 0xe5, 0x20, 0x13, 0xe0, 0x3c, 0xb0, 0x5c, 0xb1, 0x04, 0x0b,
	0x8a, 0x09, 0x68, 0xb2, 0x41, 0xe8, 0xca, 0x93, 0xd8, 0xc0, 0x61, 0x67, 0x0c, 0x08, 0x00, 0x00,
	0xff, 0xff, 0xf6, 0xef, 0xfa, 0xc4, 0x67, 0x01, 0x00, 0x00,
}
