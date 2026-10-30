package obs

import (
	"os"
	"strings"
)

// KeyProvider 托管密钥来源。
//
// 为什么要有这层抽象：P0 的密钥只从命令行/环境变量读，运维换钥匙必须重启进程；
// 而 `contracts/03` §8.2 要求支持 KMS 托管与定期轮换。把「钥匙从哪来」抽出来后，
// 环境变量只是其中一个实现，KMS（Vault / 云 KMS / 内部密钥服务）可以注入进来，
// 加解密路径完全不必知道自己拿的是哪家的钥匙。
//
// 约定：
//   - keyID 为空 → 取「当前」密钥；
//   - keyID 非空 → 取指定密钥（可能是历史钥，用于解密老密文）；
//   - 实现方返回的 id 必须与请求的 keyID 一致，否则调用方会拒绝使用——
//     否则「拿到一把不对的钥匙」会被当成「数据损坏」，排查方向直接跑偏。
type KeyProvider interface {
	Fetch(keyID string) (key []byte, id string, source string, err error)
	// Name 自述名字（env / kms:xxx），只用于运维面板。
	Name() string
}

// EnvKeyProvider 从环境变量读密钥（P0 默认实现，无外部依赖）。
type EnvKeyProvider struct {
	Env string
}

// NewEnvKeyProvider 标准环境变量名。
func NewEnvKeyProvider() *EnvKeyProvider {
	return &EnvKeyProvider{Env: "GOLDENRULES_LOG_KEY"}
}

func (p *EnvKeyProvider) Name() string { return "env" }

// Fetch 环境变量只有一把钥匙，因此只回答「当前钥」；问历史钥一律 NotFound。
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

// StaticKeyProvider 固定密钥（测试与单机部署用；也用于把 KMS 拉回来的钥注入）。
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
