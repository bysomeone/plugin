// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package common

import (
	"bytes"
	"errors"
	"io"
	"unsafe"

	"github.com/33cn/chain33/common/ed25519/edwards25519"
	log "github.com/33cn/chain33/common/log/log15"
	. "github.com/33cn/plugin/plugin/dapp/privacy/crypto"
	sccrypto "github.com/NebulousLabs/Sia/crypto"
)

const (
	publicKeyLen  = 32
	privateKeyLen = 64
	// KeyLen32 key Length
)

var (
	errViewPub    = errors.New("ErrViewPub")
	errSpendPub   = errors.New("ErrSpendPub")
	errViewSecret = errors.New("ErrViewSecret")
	//errSpendSecret   = errors.New("ErrSpendSecret")
	errNullRandInput = errors.New("ErrNullRandInput")
)

var privacylog = log.New("module", "crypto.privacy")

func generateKeyPairWithPrivKey(privByte *[KeyLen32]byte, privKeyPrivacyPtr *PrivKeyPrivacy, pubKeyPrivacyPtr *PubKeyPrivacy) error {
	if nil == privByte {
		return errNullRandInput
	}

	_, err := io.ReadFull(bytes.NewReader(privByte[:]), privKeyPrivacyPtr[:32])
	if err != nil {
		return err
	}

	addr32 := (*[KeyLen32]byte)(unsafe.Pointer(privKeyPrivacyPtr))
	addr64 := (*[privateKeyLen]byte)(unsafe.Pointer(privKeyPrivacyPtr))
	edwards25519.ScReduce(addr32, addr64)

	//to generate the publickey
	var A edwards25519.ExtendedGroupElement
	pubKeyAddr32 := (*[KeyLen32]byte)(unsafe.Pointer(pubKeyPrivacyPtr))
	edwards25519.GeScalarMultBase(&A, addr32)
	A.ToBytes(pubKeyAddr32)
	copy(addr64[KeyLen32:], pubKeyAddr32[:])

	return nil
}

func NewPrivacyWithPrivKeyEx(privKey *[KeyLen32]byte, objs interface{}) (privacy *Privacy, err error) {
	//privacylog.Info("NewPrivacyWithPrivKeyEx", "input prikey", common.Bytes2Hex(privKey[:]))
	hash := sccrypto.HashAll(*privKey, objs)
	privacy = &Privacy{}

	if err = generateKeyPairWithPrivKey((*[KeyLen32]byte)(unsafe.Pointer(&hash[0])), &privacy.SpendPrivKey, &privacy.SpendPubkey); err != nil {
		return nil, err
	}

	hashViewPriv := sccrypto.HashAll(privacy.SpendPrivKey[0:KeyLen32])
	if err = generateKeyPairWithPrivKey((*[KeyLen32]byte)(unsafe.Pointer(&hashViewPriv[0])), &privacy.ViewPrivKey, &privacy.ViewPubkey); err != nil {
		return nil, err
	}
	//privacylog.Info("NewPrivacyWithPrivKeyEx", "the new privacy created with viewpub", common.Bytes2Hex(privacy.ViewPubkey[:]))
	//privacylog.Info("NewPrivacyWithPrivKeyEx", "the new privacy created with spendpub", common.Bytes2Hex(privacy.SpendPubkey[:]))

	return privacy, nil
}