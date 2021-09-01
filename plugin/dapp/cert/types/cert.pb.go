// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.23.0
// 	protoc        v3.9.1
// source: cert.proto

package types

import (
	reflect "reflect"
	sync "sync"

	proto "github.com/golang/protobuf/proto"
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
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

type Cert struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	CertId     []byte `protobuf:"bytes,1,opt,name=certId,proto3" json:"certId,omitempty"`
	CreateTime int64  `protobuf:"varint,2,opt,name=createTime,proto3" json:"createTime,omitempty"`
	Key        string `protobuf:"bytes,3,opt,name=key,proto3" json:"key,omitempty"`
	Value      []byte `protobuf:"bytes,4,opt,name=value,proto3" json:"value,omitempty"`
}

func (x *Cert) Reset() {
	*x = Cert{}
	if protoimpl.UnsafeEnabled {
		mi := &file_cert_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *Cert) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Cert) ProtoMessage() {}

func (x *Cert) ProtoReflect() protoreflect.Message {
	mi := &file_cert_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Cert.ProtoReflect.Descriptor instead.
func (*Cert) Descriptor() ([]byte, []int) {
	return file_cert_proto_rawDescGZIP(), []int{0}
}

func (x *Cert) GetCertId() []byte {
	if x != nil {
		return x.CertId
	}
	return nil
}

func (x *Cert) GetCreateTime() int64 {
	if x != nil {
		return x.CreateTime
	}
	return 0
}

func (x *Cert) GetKey() string {
	if x != nil {
		return x.Key
	}
	return ""
}

func (x *Cert) GetValue() []byte {
	if x != nil {
		return x.Value
	}
	return nil
}

type CertAction struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// Types that are assignable to Value:
	//	*CertAction_New
	//	*CertAction_Update
	//	*CertAction_Normal
	Value isCertAction_Value `protobuf_oneof:"value"`
	Ty    int32              `protobuf:"varint,4,opt,name=ty,proto3" json:"ty,omitempty"`
}

func (x *CertAction) Reset() {
	*x = CertAction{}
	if protoimpl.UnsafeEnabled {
		mi := &file_cert_proto_msgTypes[1]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *CertAction) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CertAction) ProtoMessage() {}

func (x *CertAction) ProtoReflect() protoreflect.Message {
	mi := &file_cert_proto_msgTypes[1]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use CertAction.ProtoReflect.Descriptor instead.
func (*CertAction) Descriptor() ([]byte, []int) {
	return file_cert_proto_rawDescGZIP(), []int{1}
}

func (m *CertAction) GetValue() isCertAction_Value {
	if m != nil {
		return m.Value
	}
	return nil
}

func (x *CertAction) GetNew() *CertNew {
	if x, ok := x.GetValue().(*CertAction_New); ok {
		return x.New
	}
	return nil
}

func (x *CertAction) GetUpdate() *CertUpdate {
	if x, ok := x.GetValue().(*CertAction_Update); ok {
		return x.Update
	}
	return nil
}

func (x *CertAction) GetNormal() *CertNormal {
	if x, ok := x.GetValue().(*CertAction_Normal); ok {
		return x.Normal
	}
	return nil
}

func (x *CertAction) GetTy() int32 {
	if x != nil {
		return x.Ty
	}
	return 0
}

type isCertAction_Value interface {
	isCertAction_Value()
}

type CertAction_New struct {
	New *CertNew `protobuf:"bytes,1,opt,name=new,proto3,oneof"`
}

type CertAction_Update struct {
	Update *CertUpdate `protobuf:"bytes,2,opt,name=update,proto3,oneof"`
}

type CertAction_Normal struct {
	Normal *CertNormal `protobuf:"bytes,3,opt,name=normal,proto3,oneof"`
}

func (*CertAction_New) isCertAction_Value() {}

func (*CertAction_Update) isCertAction_Value() {}

func (*CertAction_Normal) isCertAction_Value() {}

type CertNew struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Key   string `protobuf:"bytes,1,opt,name=key,proto3" json:"key,omitempty"`
	Value []byte `protobuf:"bytes,2,opt,name=value,proto3" json:"value,omitempty"`
}

func (x *CertNew) Reset() {
	*x = CertNew{}
	if protoimpl.UnsafeEnabled {
		mi := &file_cert_proto_msgTypes[2]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *CertNew) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CertNew) ProtoMessage() {}

func (x *CertNew) ProtoReflect() protoreflect.Message {
	mi := &file_cert_proto_msgTypes[2]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use CertNew.ProtoReflect.Descriptor instead.
func (*CertNew) Descriptor() ([]byte, []int) {
	return file_cert_proto_rawDescGZIP(), []int{2}
}

func (x *CertNew) GetKey() string {
	if x != nil {
		return x.Key
	}
	return ""
}

func (x *CertNew) GetValue() []byte {
	if x != nil {
		return x.Value
	}
	return nil
}

type CertUpdate struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Key   string `protobuf:"bytes,1,opt,name=key,proto3" json:"key,omitempty"`
	Value []byte `protobuf:"bytes,2,opt,name=value,proto3" json:"value,omitempty"`
}

func (x *CertUpdate) Reset() {
	*x = CertUpdate{}
	if protoimpl.UnsafeEnabled {
		mi := &file_cert_proto_msgTypes[3]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *CertUpdate) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CertUpdate) ProtoMessage() {}

func (x *CertUpdate) ProtoReflect() protoreflect.Message {
	mi := &file_cert_proto_msgTypes[3]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use CertUpdate.ProtoReflect.Descriptor instead.
func (*CertUpdate) Descriptor() ([]byte, []int) {
	return file_cert_proto_rawDescGZIP(), []int{3}
}

func (x *CertUpdate) GetKey() string {
	if x != nil {
		return x.Key
	}
	return ""
}

func (x *CertUpdate) GetValue() []byte {
	if x != nil {
		return x.Value
	}
	return nil
}

type CertNormal struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Key   string `protobuf:"bytes,1,opt,name=key,proto3" json:"key,omitempty"`
	Value []byte `protobuf:"bytes,2,opt,name=value,proto3" json:"value,omitempty"`
}

func (x *CertNormal) Reset() {
	*x = CertNormal{}
	if protoimpl.UnsafeEnabled {
		mi := &file_cert_proto_msgTypes[4]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *CertNormal) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CertNormal) ProtoMessage() {}

func (x *CertNormal) ProtoReflect() protoreflect.Message {
	mi := &file_cert_proto_msgTypes[4]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use CertNormal.ProtoReflect.Descriptor instead.
func (*CertNormal) Descriptor() ([]byte, []int) {
	return file_cert_proto_rawDescGZIP(), []int{4}
}

func (x *CertNormal) GetKey() string {
	if x != nil {
		return x.Key
	}
	return ""
}

func (x *CertNormal) GetValue() []byte {
	if x != nil {
		return x.Value
	}
	return nil
}

type Authority struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Enable     bool   `protobuf:"varint,1,opt,name=enable,proto3" json:"enable,omitempty"`
	CryptoPath string `protobuf:"bytes,2,opt,name=cryptoPath,proto3" json:"cryptoPath,omitempty"`
	SignType   string `protobuf:"bytes,3,opt,name=signType,proto3" json:"signType,omitempty"`
}

func (x *Authority) Reset() {
	*x = Authority{}
	if protoimpl.UnsafeEnabled {
		mi := &file_cert_proto_msgTypes[5]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *Authority) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Authority) ProtoMessage() {}

func (x *Authority) ProtoReflect() protoreflect.Message {
	mi := &file_cert_proto_msgTypes[5]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Authority.ProtoReflect.Descriptor instead.
func (*Authority) Descriptor() ([]byte, []int) {
	return file_cert_proto_rawDescGZIP(), []int{5}
}

func (x *Authority) GetEnable() bool {
	if x != nil {
		return x.Enable
	}
	return false
}

func (x *Authority) GetCryptoPath() string {
	if x != nil {
		return x.CryptoPath
	}
	return ""
}

func (x *Authority) GetSignType() string {
	if x != nil {
		return x.SignType
	}
	return ""
}

type CertSignature struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Signature []byte `protobuf:"bytes,1,opt,name=signature,proto3" json:"signature,omitempty"`
	Cert      []byte `protobuf:"bytes,2,opt,name=cert,proto3" json:"cert,omitempty"`
	Uid       []byte `protobuf:"bytes,3,opt,name=uid,proto3" json:"uid,omitempty"`
}

func (x *CertSignature) Reset() {
	*x = CertSignature{}
	if protoimpl.UnsafeEnabled {
		mi := &file_cert_proto_msgTypes[6]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *CertSignature) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CertSignature) ProtoMessage() {}

func (x *CertSignature) ProtoReflect() protoreflect.Message {
	mi := &file_cert_proto_msgTypes[6]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use CertSignature.ProtoReflect.Descriptor instead.
func (*CertSignature) Descriptor() ([]byte, []int) {
	return file_cert_proto_rawDescGZIP(), []int{6}
}

func (x *CertSignature) GetSignature() []byte {
	if x != nil {
		return x.Signature
	}
	return nil
}

func (x *CertSignature) GetCert() []byte {
	if x != nil {
		return x.Cert
	}
	return nil
}

func (x *CertSignature) GetUid() []byte {
	if x != nil {
		return x.Uid
	}
	return nil
}

type ReqQueryValidCertSN struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Addr string `protobuf:"bytes,1,opt,name=addr,proto3" json:"addr,omitempty"`
}

func (x *ReqQueryValidCertSN) Reset() {
	*x = ReqQueryValidCertSN{}
	if protoimpl.UnsafeEnabled {
		mi := &file_cert_proto_msgTypes[7]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ReqQueryValidCertSN) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ReqQueryValidCertSN) ProtoMessage() {}

func (x *ReqQueryValidCertSN) ProtoReflect() protoreflect.Message {
	mi := &file_cert_proto_msgTypes[7]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ReqQueryValidCertSN.ProtoReflect.Descriptor instead.
func (*ReqQueryValidCertSN) Descriptor() ([]byte, []int) {
	return file_cert_proto_rawDescGZIP(), []int{7}
}

func (x *ReqQueryValidCertSN) GetAddr() string {
	if x != nil {
		return x.Addr
	}
	return ""
}

type RepQueryValidCertSN struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Sn []byte `protobuf:"bytes,1,opt,name=sn,proto3" json:"sn,omitempty"`
}

func (x *RepQueryValidCertSN) Reset() {
	*x = RepQueryValidCertSN{}
	if protoimpl.UnsafeEnabled {
		mi := &file_cert_proto_msgTypes[8]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *RepQueryValidCertSN) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*RepQueryValidCertSN) ProtoMessage() {}

func (x *RepQueryValidCertSN) ProtoReflect() protoreflect.Message {
	mi := &file_cert_proto_msgTypes[8]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use RepQueryValidCertSN.ProtoReflect.Descriptor instead.
func (*RepQueryValidCertSN) Descriptor() ([]byte, []int) {
	return file_cert_proto_rawDescGZIP(), []int{8}
}

func (x *RepQueryValidCertSN) GetSn() []byte {
	if x != nil {
		return x.Sn
	}
	return nil
}

var File_cert_proto protoreflect.FileDescriptor

var file_cert_proto_rawDesc = []byte{
	0x0a, 0x0a, 0x63, 0x65, 0x72, 0x74, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x05, 0x74, 0x79,
	0x70, 0x65, 0x73, 0x22, 0x66, 0x0a, 0x04, 0x43, 0x65, 0x72, 0x74, 0x12, 0x16, 0x0a, 0x06, 0x63,
	0x65, 0x72, 0x74, 0x49, 0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x06, 0x63, 0x65, 0x72,
	0x74, 0x49, 0x64, 0x12, 0x1e, 0x0a, 0x0a, 0x63, 0x72, 0x65, 0x61, 0x74, 0x65, 0x54, 0x69, 0x6d,
	0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x03, 0x52, 0x0a, 0x63, 0x72, 0x65, 0x61, 0x74, 0x65, 0x54,
	0x69, 0x6d, 0x65, 0x12, 0x10, 0x0a, 0x03, 0x6b, 0x65, 0x79, 0x18, 0x03, 0x20, 0x01, 0x28, 0x09,
	0x52, 0x03, 0x6b, 0x65, 0x79, 0x12, 0x14, 0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x18, 0x04,
	0x20, 0x01, 0x28, 0x0c, 0x52, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x22, 0xa3, 0x01, 0x0a, 0x0a,
	0x43, 0x65, 0x72, 0x74, 0x41, 0x63, 0x74, 0x69, 0x6f, 0x6e, 0x12, 0x22, 0x0a, 0x03, 0x6e, 0x65,
	0x77, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x0e, 0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e,
	0x43, 0x65, 0x72, 0x74, 0x4e, 0x65, 0x77, 0x48, 0x00, 0x52, 0x03, 0x6e, 0x65, 0x77, 0x12, 0x2b,
	0x0a, 0x06, 0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x11,
	0x2e, 0x74, 0x79, 0x70, 0x65, 0x73, 0x2e, 0x43, 0x65, 0x72, 0x74, 0x55, 0x70, 0x64, 0x61, 0x74,
	0x65, 0x48, 0x00, 0x52, 0x06, 0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x12, 0x2b, 0x0a, 0x06, 0x6e,
	0x6f, 0x72, 0x6d, 0x61, 0x6c, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x11, 0x2e, 0x74, 0x79,
	0x70, 0x65, 0x73, 0x2e, 0x43, 0x65, 0x72, 0x74, 0x4e, 0x6f, 0x72, 0x6d, 0x61, 0x6c, 0x48, 0x00,
	0x52, 0x06, 0x6e, 0x6f, 0x72, 0x6d, 0x61, 0x6c, 0x12, 0x0e, 0x0a, 0x02, 0x74, 0x79, 0x18, 0x04,
	0x20, 0x01, 0x28, 0x05, 0x52, 0x02, 0x74, 0x79, 0x42, 0x07, 0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75,
	0x65, 0x22, 0x31, 0x0a, 0x07, 0x43, 0x65, 0x72, 0x74, 0x4e, 0x65, 0x77, 0x12, 0x10, 0x0a, 0x03,
	0x6b, 0x65, 0x79, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x6b, 0x65, 0x79, 0x12, 0x14,
	0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x05, 0x76,
	0x61, 0x6c, 0x75, 0x65, 0x22, 0x34, 0x0a, 0x0a, 0x43, 0x65, 0x72, 0x74, 0x55, 0x70, 0x64, 0x61,
	0x74, 0x65, 0x12, 0x10, 0x0a, 0x03, 0x6b, 0x65, 0x79, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52,
	0x03, 0x6b, 0x65, 0x79, 0x12, 0x14, 0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x18, 0x02, 0x20,
	0x01, 0x28, 0x0c, 0x52, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x22, 0x34, 0x0a, 0x0a, 0x43, 0x65,
	0x72, 0x74, 0x4e, 0x6f, 0x72, 0x6d, 0x61, 0x6c, 0x12, 0x10, 0x0a, 0x03, 0x6b, 0x65, 0x79, 0x18,
	0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x6b, 0x65, 0x79, 0x12, 0x14, 0x0a, 0x05, 0x76, 0x61,
	0x6c, 0x75, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65,
	0x22, 0x5f, 0x0a, 0x09, 0x41, 0x75, 0x74, 0x68, 0x6f, 0x72, 0x69, 0x74, 0x79, 0x12, 0x16, 0x0a,
	0x06, 0x65, 0x6e, 0x61, 0x62, 0x6c, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x08, 0x52, 0x06, 0x65,
	0x6e, 0x61, 0x62, 0x6c, 0x65, 0x12, 0x1e, 0x0a, 0x0a, 0x63, 0x72, 0x79, 0x70, 0x74, 0x6f, 0x50,
	0x61, 0x74, 0x68, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0a, 0x63, 0x72, 0x79, 0x70, 0x74,
	0x6f, 0x50, 0x61, 0x74, 0x68, 0x12, 0x1a, 0x0a, 0x08, 0x73, 0x69, 0x67, 0x6e, 0x54, 0x79, 0x70,
	0x65, 0x18, 0x03, 0x20, 0x01, 0x28, 0x09, 0x52, 0x08, 0x73, 0x69, 0x67, 0x6e, 0x54, 0x79, 0x70,
	0x65, 0x22, 0x53, 0x0a, 0x0d, 0x43, 0x65, 0x72, 0x74, 0x53, 0x69, 0x67, 0x6e, 0x61, 0x74, 0x75,
	0x72, 0x65, 0x12, 0x1c, 0x0a, 0x09, 0x73, 0x69, 0x67, 0x6e, 0x61, 0x74, 0x75, 0x72, 0x65, 0x18,
	0x01, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x09, 0x73, 0x69, 0x67, 0x6e, 0x61, 0x74, 0x75, 0x72, 0x65,
	0x12, 0x12, 0x0a, 0x04, 0x63, 0x65, 0x72, 0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x04,
	0x63, 0x65, 0x72, 0x74, 0x12, 0x10, 0x0a, 0x03, 0x75, 0x69, 0x64, 0x18, 0x03, 0x20, 0x01, 0x28,
	0x0c, 0x52, 0x03, 0x75, 0x69, 0x64, 0x22, 0x29, 0x0a, 0x13, 0x52, 0x65, 0x71, 0x51, 0x75, 0x65,
	0x72, 0x79, 0x56, 0x61, 0x6c, 0x69, 0x64, 0x43, 0x65, 0x72, 0x74, 0x53, 0x4e, 0x12, 0x12, 0x0a,
	0x04, 0x61, 0x64, 0x64, 0x72, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x61, 0x64, 0x64,
	0x72, 0x22, 0x25, 0x0a, 0x13, 0x52, 0x65, 0x70, 0x51, 0x75, 0x65, 0x72, 0x79, 0x56, 0x61, 0x6c,
	0x69, 0x64, 0x43, 0x65, 0x72, 0x74, 0x53, 0x4e, 0x12, 0x0e, 0x0a, 0x02, 0x73, 0x6e, 0x18, 0x01,
	0x20, 0x01, 0x28, 0x0c, 0x52, 0x02, 0x73, 0x6e, 0x42, 0x0a, 0x5a, 0x08, 0x2e, 0x2e, 0x2f, 0x74,
	0x79, 0x70, 0x65, 0x73, 0x62, 0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_cert_proto_rawDescOnce sync.Once
	file_cert_proto_rawDescData = file_cert_proto_rawDesc
)

func file_cert_proto_rawDescGZIP() []byte {
	file_cert_proto_rawDescOnce.Do(func() {
		file_cert_proto_rawDescData = protoimpl.X.CompressGZIP(file_cert_proto_rawDescData)
	})
	return file_cert_proto_rawDescData
}

var file_cert_proto_msgTypes = make([]protoimpl.MessageInfo, 9)
var file_cert_proto_goTypes = []interface{}{
	(*Cert)(nil),                // 0: types.Cert
	(*CertAction)(nil),          // 1: types.CertAction
	(*CertNew)(nil),             // 2: types.CertNew
	(*CertUpdate)(nil),          // 3: types.CertUpdate
	(*CertNormal)(nil),          // 4: types.CertNormal
	(*Authority)(nil),           // 5: types.Authority
	(*CertSignature)(nil),       // 6: types.CertSignature
	(*ReqQueryValidCertSN)(nil), // 7: types.ReqQueryValidCertSN
	(*RepQueryValidCertSN)(nil), // 8: types.RepQueryValidCertSN
}
var file_cert_proto_depIdxs = []int32{
	2, // 0: types.CertAction.new:type_name -> types.CertNew
	3, // 1: types.CertAction.update:type_name -> types.CertUpdate
	4, // 2: types.CertAction.normal:type_name -> types.CertNormal
	3, // [3:3] is the sub-list for method output_type
	3, // [3:3] is the sub-list for method input_type
	3, // [3:3] is the sub-list for extension type_name
	3, // [3:3] is the sub-list for extension extendee
	0, // [0:3] is the sub-list for field type_name
}

func init() { file_cert_proto_init() }
func file_cert_proto_init() {
	if File_cert_proto != nil {
		return
	}
	if !protoimpl.UnsafeEnabled {
		file_cert_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*Cert); i {
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
		file_cert_proto_msgTypes[1].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*CertAction); i {
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
		file_cert_proto_msgTypes[2].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*CertNew); i {
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
		file_cert_proto_msgTypes[3].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*CertUpdate); i {
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
		file_cert_proto_msgTypes[4].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*CertNormal); i {
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
		file_cert_proto_msgTypes[5].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*Authority); i {
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
		file_cert_proto_msgTypes[6].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*CertSignature); i {
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
		file_cert_proto_msgTypes[7].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ReqQueryValidCertSN); i {
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
		file_cert_proto_msgTypes[8].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*RepQueryValidCertSN); i {
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
	file_cert_proto_msgTypes[1].OneofWrappers = []interface{}{
		(*CertAction_New)(nil),
		(*CertAction_Update)(nil),
		(*CertAction_Normal)(nil),
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_cert_proto_rawDesc,
			NumEnums:      0,
			NumMessages:   9,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_cert_proto_goTypes,
		DependencyIndexes: file_cert_proto_depIdxs,
		MessageInfos:      file_cert_proto_msgTypes,
	}.Build()
	File_cert_proto = out.File
	file_cert_proto_rawDesc = nil
	file_cert_proto_goTypes = nil
	file_cert_proto_depIdxs = nil
}
