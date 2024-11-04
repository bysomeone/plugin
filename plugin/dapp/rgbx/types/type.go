package types

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// ToString encode to string
func (o *OutPoint) ToString() string {

	return fmt.Sprintf("%s:%d", hex.EncodeToString(o.GetHash()), o.GetIndex())
}

// FromString decode from string
func (o *OutPoint) FromString(s string) error {
	strs := strings.Split(s, ":")
	if len(strs) != 2 {
		return fmt.Errorf("invalid outpoint: %s", s)
	}
	b, err := hex.DecodeString(strs[0])
	if err != nil {
		return err
	}
	o.Hash = b

	v, err := strconv.ParseInt(strs[1], 10, 32)
	if err != nil {
		return err
	}
	o.Index = uint32(v)
	return nil
}

func (a *AssetAccount) Address() string {
	if a.GetUtxo() != nil {
		return a.GetUtxo().ToString()
	}
	return a.GetAddress()
}
