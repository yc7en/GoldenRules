package obs

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
)

// Box 原始快照的加密盒（AES-256-GCM）。
//
// 取舍：P0 不做密钥管理（KMS），密钥从 `-log-key` 或环境变量 `GOLDENRULES_LOG_KEY`
// 读（32 字节，hex 或 base64）。**没配密钥就不收原始快照**——明确降级为「仅脱敏日志」，
// 而不是悄悄用明文落盘。这条 fail-closed 是 `contracts/03` §8.2 的底线。
type Box struct {
	// mu 保护 current 与 retained：密钥轮换发生在运行期，而加解密在并发路径上。
	//
	// 轮换的**正确性要求**是「旧密文还能读」：轮换只改变「新写入用哪把钥匙」，
	// 绝不删除旧钥匙，否则历史原始快照会变成永久不可解密的字节（这是不可逆的数据损失）。
	mu       sync.RWMutex
	aead     cipher.AEAD
	keyID    string
	retained map[string]cipher.AEAD
	source   string
	// provider 可选的托管密钥源。只在「本地找不到某把钥匙」时才被调用——
	// 常规路径不该为每次解密多打一次网络往返。
	provider KeyProvider
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
	return newBoxFromKey(key, "explicit")
}

func newBoxFromKey(key []byte, source string) (*Box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM 失败: %w", err)
	}
	// keyID 只用摘要前缀：审计里要能说清「哪把钥匙」，但不能泄露钥匙。
	return &Box{
		aead:     aead,
		keyID:    keyIDOf(key),
		retained: map[string]cipher.AEAD{},
		source:   source,
	}, nil
}

// asOBS 把普通 error 收敛成带错误码的领域错误。
//
// 对外错误码必须封闭：调用方按码判断，不解析文案。已经带码的原样透出，
// 兜底的按解密失败处理（加解密路径上的裸错基本都是密钥/密文不匹配）。
func asOBS(err error) *Error {
	var oe *Error
	if errors.As(err, &oe) {
		return oe
	}
	return &Error{Code: CodeDecryptFailed, Message: err.Error()}
}

// NewBoxFromEnv 优先用显式入参，其次环境变量。
func NewBoxFromEnv(explicit string) (*Box, error) {
	if strings.TrimSpace(explicit) != "" {
		return NewBox(explicit)
	}
	return NewBox(os.Getenv("GOLDENRULES_LOG_KEY"))
}

// KeyID 当前密钥标识（新密文用它）。
func (b *Box) KeyID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.keyID
}

// Seal 加密：返回 nonce 与密文（都做 base64，便于塞进 JSON）。
//
// AAD 用 logId：把密文绑定到具体日志，防止「把 A 的密文挪到 B 上」这种低成本篡改。
func (b *Box) Seal(plain []byte, logID string) (nonce, cipherText string, err error) {
	b.mu.RLock()
	aead := b.aead
	b.mu.RUnlock()

	n := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, n); err != nil {
		return "", "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	sealed := aead.Seal(nil, n, plain, []byte(logID))
	return base64.StdEncoding.EncodeToString(n), base64.StdEncoding.EncodeToString(sealed), nil
}

// Open 解密。AAD 为 logId，绑定「这条密文属于这条日志」，防止把 A 的密文挪到 B 上。
func (b *Box) Open(nonceB64, cipherB64, logID string) ([]byte, error) {
	return b.OpenWithKeyID(nonceB64, cipherB64, logID, "")
}

// OpenWithKeyID 用指定密钥解密；keyID 为空表示「当前钥 + 全部保留钥」依次尝试。
//
// 明确的两条取舍：
//
//  1. **未知 keyID 必须报错，不能静默回退当前钥**。否则「轮换后读不了」会被
//     掩盖成一句含糊的 GCM 校验失败，运维永远查不清是哪一把钥匙丢了。
//  2. keyID 为空时按「当前钥 → 保留钥」顺序试。这段路径只服务于旧数据——
//     历史原始快照写入时还没有按钥记录 keyId 的字段。
func (b *Box) OpenWithKeyID(nonceB64, cipherB64, logID, keyID string) ([]byte, error) {
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return nil, fmt.Errorf("nonce 解码失败: %w", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil {
		return nil, fmt.Errorf("密文解码失败: %w", err)
	}

	b.mu.RLock()
	var candidates []cipher.AEAD
	resolved := true
	switch {
	case keyID == "":
		candidates = append(candidates, b.aead)
		// 保留钥顺序不确定，但候选之间互斥（不同 key 解不开彼此），顺序不影响正确性。
		for _, a := range b.retained {
			candidates = append(candidates, a)
		}
	case keyID == b.keyID:
		candidates = []cipher.AEAD{b.aead}
	default:
		if a, ok := b.retained[keyID]; ok {
			candidates = []cipher.AEAD{a}
		} else {
			resolved = false
		}
	}
	b.mu.RUnlock()

	if !resolved {
		// 本地没有：向托管源要一次（拿到后会落入保留集）。这里已经**不持锁**，
		// 因为 provider 可能是一次网络往返，持锁会阻塞全部加解密。
		a, err := b.adoptFromProvider(keyID)
		if err != nil {
			return nil, err
		}
		candidates = []cipher.AEAD{a}
	}

	var lastErr error
	for _, aead := range candidates {
		plain, oerr := aead.Open(nil, nonce, sealed, []byte(logID))
		if oerr == nil {
			return plain, nil
		}
		lastErr = oerr
	}
	return nil, fmt.Errorf("解密失败: %w", lastErr)
}

// WithKeyProvider 挂上托管密钥源（KMS）。返回自身便于链式调用。
func (b *Box) WithKeyProvider(p KeyProvider) *Box {
	b.mu.Lock()
	b.provider = p
	b.mu.Unlock()
	return b
}

// ProviderName 自述托管源名字；未挂则返回空串。
func (b *Box) ProviderName() string {
	b.mu.RLock()
	p := b.provider
	b.mu.RUnlock()
	if p == nil {
		return ""
	}
	return p.Name()
}

// adoptFromProvider 本地没有这把钥匙时向托管源要一次。
//
// 拿到后**落进 retained**：同一把历史钥会被反复用于解密老密文，
// 每次都回 KMS 取一遍既慢又浪费配额。
func (b *Box) adoptFromProvider(keyID string) (cipher.AEAD, error) {
	b.mu.RLock()
	p := b.provider
	b.mu.RUnlock()
	if p == nil {
		return nil, errf(CodeKeyUnknown, "未知密钥标识 "+keyID+"（未配置托管密钥源，无法按需取回）")
	}
	key, got, source, err := p.Fetch(keyID)
	if err != nil {
		return nil, err
	}
	if got != keyID {
		// 托管源给了另一把钥匙：宁可直接失败，也不要用错钥匙去试——
		// 错钥匙只会得到一句 GCM 校验失败，把「取错钥」伪装成「数据损坏」。
		return nil, errf(CodeKeyUnknown, "托管源返回的密钥标识 "+got+" 与请求的 "+keyID+" 不一致")
	}
	next, err := newBoxFromKey(key, source)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.retained[got] = next.aead
	b.mu.Unlock()
	return next.aead, nil
}

// Rotate 轮换密钥：新密文改用新钥，旧钥进入保留集。
//
// 这里只做「换钥匙」，不做「换完就删」——删旧钥等于让历史原始快照永久不可解密，
// 是不可逆的数据损失。真要下线旧钥，必须先把历史密文全部重加密（本轮不做）。
//
// 幂等：重复轮换到同一把钥匙是空操作，不会在保留集里堆积同 keyID 的副本。
func (b *Box) Rotate(encoded string) error {
	raw := strings.TrimSpace(encoded)
	if raw == "" {
		return errf(CodeKeyMissing, "轮换密钥为空")
	}
	key, err := decodeKey(raw)
	if err != nil {
		return err
	}
	next, err := newBoxFromKey(key, "rotated")
	if err != nil {
		return err
	}
	b.swapIn(next)
	return nil
}

// RotateFromProvider 向托管源要「当前钥」并轮换过去（KMS 定期轮换的接线点）。
//
// 返回轮换后的 keyID；若托管源给的还是同一把钥匙则原样返回、不做任何变动。
func (b *Box) RotateFromProvider() (string, error) {
	b.mu.RLock()
	p := b.provider
	b.mu.RUnlock()
	if p == nil {
		return "", errf(CodeKeyMissing, "未配置托管密钥源，无法从 KMS 轮换")
	}
	key, id, source, err := p.Fetch("")
	if err != nil {
		return "", err
	}
	next, err := newBoxFromKey(key, source)
	if err != nil {
		return "", err
	}
	if next.keyID != id {
		return "", errf(CodeKeyInvalid, "托管源自述 keyID "+id+" 与实际密钥摘要 "+next.keyID+" 不一致")
	}
	b.swapIn(next)
	return id, nil
}

// swapIn 换上新钥，旧钥沉入保留集。
func (b *Box) swapIn(next *Box) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if next.keyID == b.keyID {
		return
	}
	b.retained[b.keyID] = b.aead
	b.aead = next.aead
	b.keyID = next.keyID
	b.source = next.source
}

// RetainedKeyIDs 保留钥标识列表（可用于自述「历史密文还用哪些钥匙」）。
func (b *Box) RetainedKeyIDs() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.retained))
	for id := range b.retained {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// KeySource 当前钥的来源（explicit / env / kms / rotated），用于运维自述。
func (b *Box) KeySource() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.source
}

func decodeKey(raw string) ([]byte, error) {
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errf(CodeKeyInvalid, "密钥必须是 32 字节的 hex 或 base64")
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
