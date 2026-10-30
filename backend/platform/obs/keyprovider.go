package obs

import (
	"os"
	"strings"
)

// KeyProvider is a managed key source.
//
// Why this abstraction exists: in P0 the key is read only from the command line/environment, so rotating a key in ops requires a process restart;
// whereas `contracts/03` §8.2 requires KMS hosting and periodic rotation. Once "where the key comes from" is abstracted,
// the environment variable is just one implementation and KMS (Vault / cloud KMS / internal key service) can be injected,
// so the encryption/decryption path never needs to know whose key it is holding.
//
// Conventions:
//   - empty keyID → take the "current" key;
//   - non-empty keyID → take that specific key (possibly a historical key, used to decrypt old ciphertext);
//   - the id returned by the implementation must match the requested keyID, otherwise the caller refuses to use it —
//     otherwise "a wrong key was fetched" is taken for "corrupted data" and the investigation goes straight off track.
type KeyProvider interface {
	Fetch(keyID string) (key []byte, id string, source string, err error)
	// Name self-reports the name (env / kms:xxx); used only by the ops panel.
	Name() string
}

// EnvKeyProvider reads the key from an environment variable (the P0 default implementation, no external dependencies).
type EnvKeyProvider struct {
	Env string
}

// NewEnvKeyProvider uses the standard environment variable name.
func NewEnvKeyProvider() *EnvKeyProvider {
	return &EnvKeyProvider{Env: "GOLDENRULES_LOG_KEY"}
}

func (p *EnvKeyProvider) Name() string { return "env" }

// Fetch: the environment holds only one key, so it only answers for the "current key"; a historical key is always NotFound.
func (p *EnvKeyProvider) Fetch(keyID string) ([]byte, string, string, error) {
	raw := strings.TrimSpace(os.Getenv(p.Env))
	if raw == "" {
		return nil, "", "", errf(CodeKeyMissing, "环境变量 "+p.Env+" 未设置")
	}
	key, err := decodeKey(raw)
	if err != nil {
		return nil, "", "", err
	}
	if keyID != "" && keyIDOf(key) != keyID {
		return nil, "", "", errf(CodeKeyUnknown, "环境变量提供的密钥与请求的 keyID 不匹配")
	}
	return key, keyIDOf(key), "env", nil
}

// StaticKeyProvider holds a fixed key (for tests and single-machine deployments; also used to inject a key fetched from KMS).
type StaticKeyProvider struct {
	Encoded string
	Source  string
}

func (p *StaticKeyProvider) Name() string { return p.Source }

func (p *StaticKeyProvider) Fetch(keyID string) ([]byte, string, string, error) {
	key, err := decodeKey(strings.TrimSpace(p.Encoded))
	if err != nil {
		return nil, "", "", err
	}
	if keyID != "" && keyIDOf(key) != keyID {
		return nil, "", "", errf(CodeKeyUnknown, "静态密钥与请求的 keyID 不匹配")
	}
	return key, keyIDOf(key), p.Source, nil
}
