package obs

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// Box 原始快照的加密盒（AES-256-GCM）。
//
// 取舍：P0 不做密钥管理（KMS），密钥从 `-log-key` 或环境变量 `GOLDENRULES_LOG_KEY`
// 读（32 字节，hex 或 base64）。**没配密钥就不收原始快照**——明确降级为「仅脱敏日志」，
// 而不是悄悄用明文落盘。这条 fail-closed 是 `contracts/03` §8.2 的底线。
type Box struct {
	aead  cipher.AEAD
	keyID string
}

// NewBox 由 hex/base64 编码的 32 字节密钥构造加密盒。
func NewBox(encoded string) (*Box, error) {
	raw := strings.TrimSpace(encoded)
	if raw == "" {
		return nil, errf(CodeKeyMissing, "未配置原始快照加密密钥（-log-key 或 GOLDENRULES_LOG_KEY）")
	}
	key, err := decodeKey(raw)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM 失败: %w", err)
	}
	// keyID 只用摘要前缀：审计里要能说清「哪把钥匙」，但不能泄露钥匙。
	sum := keyIDOf(key)
	return &Box{aead: aead, keyID: sum}, nil
}

// NewBoxFromEnv 优先用显式入参，其次环境变量。
func NewBoxFromEnv(explicit string) (*Box, error) {
	if strings.TrimSpace(explicit) != "" {
		return NewBox(explicit)
	}
	return NewBox(os.Getenv("GOLDENRULES_LOG_KEY"))
}

// KeyID 当前密钥标识。
func (b *Box) KeyID() string { return b.keyID }

// Seal 加密：返回 nonce 与密文（都做 base64，便于塞进 JSON）。
//
// AAD 用 logId：把密文绑定到具体日志，防止「把 A 的密文挪到 B 上」这种低成本篡改。
func (b *Box) Seal(plain []byte, logID string) (nonce, cipherText string, err error) {
	n := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, n); err != nil {
		return "", "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	sealed := b.aead.Seal(nil, n, plain, []byte(logID))
	return base64.StdEncoding.EncodeToString(n), base64.StdEncoding.EncodeToString(sealed), nil
}

// Open 解密。AAD 为 logId，绑定「这条密文属于这条日志」，防止把 A 的密文挪到 B 上。
func (b *Box) Open(nonceB64, cipherB64, logID string) ([]byte, error) {
	n, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return nil, errf(CodeDecryptFailed, "nonce 解码失败")
	}
	ct, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil {
		return nil, errf(CodeDecryptFailed, "密文解码失败")
	}
	plain, err := b.aead.Open(nil, n, ct, []byte(logID))
	if err != nil {
		return nil, errf(CodeDecryptFailed, "解密失败（密钥不匹配或密文被篡改）")
	}
	return plain, nil
}

func decodeKey(raw string) ([]byte, error) {
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errf(CodeKeyMissing, "密钥必须是 32 字节的 hex 或 base64")
}

func sha256Short(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

func keyIDOf(key []byte) string {
	// 简单摘要：不从 keyID 反推密钥。用 sha256 前 4 字节即可区分不同钥匙。
	h := sha256Short(key)
	return "k-" + h
}
