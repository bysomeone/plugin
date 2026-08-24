package rgb20

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// decodeBase64 解码 base64；失败返回 nil。
func decodeBase64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// decodeHex 解码 hex。
func decodeHex(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimSpace(s))
}

// mustJSON 序列化为 JSON；失败时 panic（内部状态编码不应失败）。
func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("rgb20 marshal json: %v", err))
	}
	return b
}

func unmarshalJSON(data []byte, v interface{}) error {
	if len(data) == 0 {
		return fmt.Errorf("empty data")
	}
	return json.Unmarshal(data, v)
}
