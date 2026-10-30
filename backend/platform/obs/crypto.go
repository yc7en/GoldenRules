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

// Box is the encryption box for raw snapshots (AES-256-GCM).
//
// Trade-off: P0 does not do key management (KMS); the key is read from `-log-key` or the environment variable
// `GOLDENRULES_LOG_KEY` (32 bytes, hex or base64). **With no key configured, raw snapshots are not collected at all** — an explicit degrade to "redacted logs only",
// rather than quietly writing plaintext to disk. This fail-closed rule is the bottom line of `contracts/03` §8.2.
type Box struct {
	// mu guards current and retained: key rotation happens at runtime, while encryption and decryption run on concurrent paths.
	//
	// The **correctness requirement** of rotation is "old ciphertext stays readable": rotation only changes which key new writes use,
	// and never deletes old keys; otherwise historical raw snapshots become permanently undecryptable bytes (irreversible data loss).
	mu       sync.RWMutex
	aead     cipher.AEAD
	keyID    string
	retained map[string]cipher.AEAD
	source   string
	// provider is the optional managed key source. It is only consulted when a key cannot be found locally —
	// the normal path must not pay an extra network round-trip on every decryption.
	provider KeyProvider
}

// NewBox builds the encryption box from a 32-byte key encoded as hex/base64.
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
	// keyID uses only a digest prefix: the audit trail must be able to say "which key" without leaking the key.
	return &Box{
		aead:     aead,
		keyID:    keyIDOf(key),
		retained: map[string]cipher.AEAD{},
		source:   source,
	}, nil
}

// asOBS narrows a plain error into a domain error carrying an error code.
//
// The outward error codes must stay closed: callers branch on the code and never parse the text. Errors that already
// carry a code pass through unchanged; anything else is treated as a decryption failure (bare errors on the crypto path are almost always a key/ciphertext mismatch).
func asOBS(err error) *Error {
	var oe *Error
	if errors.As(err, &oe) {
		return oe
	}
	return &Error{Code: CodeDecryptFailed, Message: err.Error()}
}

// NewBoxFromEnv prefers the explicit argument, then the environment variable.
func NewBoxFromEnv(explicit string) (*Box, error) {
	if strings.TrimSpace(explicit) != "" {
		return NewBox(explicit)
	}
	return NewBox(os.Getenv("GOLDENRULES_LOG_KEY"))
}

// KeyID is the current key identifier (used for new ciphertext).
func (b *Box) KeyID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.keyID
}

// Seal encrypts: it returns the nonce and the ciphertext (both base64, so they fit into JSON).
//
// The AAD is logId: it binds the ciphertext to a specific log, preventing the cheap tampering trick of "moving A's ciphertext onto B".
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

// Open decrypts. The AAD is logId, binding "this ciphertext belongs to this log" and preventing A's ciphertext from being moved onto B.
func (b *Box) Open(nonceB64, cipherB64, logID string) ([]byte, error) {
	return b.OpenWithKeyID(nonceB64, cipherB64, logID, "")
}

// OpenWithKeyID decrypts with the given key; an empty keyID means "current key + all retained keys", tried in turn.
//
// Two deliberate trade-offs:
//
//  1. **An unknown keyID must be an error, never a silent fallback to the current key**. Otherwise "unreadable after
//     rotation" would be masked as a vague GCM verification failure, and ops would never work out which key was lost.
//  2. an empty keyID tries "current key → retained keys" in order. This path only serves old data —
//     historical raw snapshots were written before there was a field recording keyId per key.
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
		// The order of retained keys is unspecified, but candidates are mutually exclusive (different keys cannot decrypt each other), so order does not affect correctness.
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
		// Not found locally: ask the managed source once (what comes back is stored into the retained set). We are deliberately **not holding the lock** here,
		// because provider may be a network round-trip and holding the lock would stall all encryption and decryption.
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

// WithKeyProvider attaches the managed key source (KMS). It returns the receiver for chaining.
func (b *Box) WithKeyProvider(p KeyProvider) *Box {
	b.mu.Lock()
	b.provider = p
	b.mu.Unlock()
	return b
}

// ProviderName self-reports the managed source name; an empty string when none is attached.
func (b *Box) ProviderName() string {
	b.mu.RLock()
	p := b.provider
	b.mu.RUnlock()
	if p == nil {
		return ""
	}
	return p.Name()
}

// adoptFromProvider asks the managed source once when the key is not available locally.
//
// What comes back is **stored into retained**: the same historical key keeps being used to decrypt old
// ciphertext, and going back to KMS every time would be slow and waste quota.
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
		// The managed source returned a different key: fail outright rather than trying the wrong key —
		// a wrong key only yields a GCM verification failure, disguising "wrong key fetched" as "corrupted data".
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

// Rotate rotates the key: new ciphertext uses the new key, the old key moves into the retained set.
//
// This only "swaps the key", it does not "swap then delete" — deleting the old key makes historical raw snapshots permanently undecryptable,
// which is irreversible data loss. To actually retire an old key, all historical ciphertext must be re-encrypted first (not in this round).
//
// Idempotent: rotating to the same key again is a no-op and does not pile up duplicate entries for the same keyID.
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

// RotateFromProvider asks the managed source for the "current key" and rotates to it (the wiring point for periodic KMS rotation).
//
// It returns the keyID after rotation; if the managed source returns the same key, that keyID is returned and nothing changes.
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

// swapIn installs the new key and sinks the old key into the retained set.
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

// RetainedKeyIDs lists the retained key identifiers (useful to self-report "which keys historical ciphertext still needs").
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

// KeySource is the source of the current key (explicit / env / kms / rotated), used for operational self-report.
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
	// Simple digest: the key cannot be derived back from keyID. The first 4 bytes of sha256 suffice to distinguish keys.
	h := sha256Short(key)
	return "k-" + h
}
