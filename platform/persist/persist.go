// Package persist 是管理平台（Go）的最小持久化层。
//
// P0 只做一件事：把**一份 JSON 文档**整体放在内存，改动时整体原子落盘
// （同目录临时文件 + rename）。选它的理由：
//
//   - 平台是治理侧、低 QPS，不需要数据库的并发能力；
//   - 规则包 / 事实目录 / 审计日志在契约里都是「文档 + 只追加」的形状，
//     整体落盘天然满足「原子 + 不可变」；
//   - 换真 DB 时只需替换本包，业务代码只依赖 View/Update 两个语义。
//
// 两条纪律：
//  1. `View` 拿到的指针**只读**，不允许在回调里改（否则写入不可控）。
//  2. `Update` 的回调返回错误时**整份文档回滚**，不留半截状态。
package persist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store 是单个 JSON 文档的存储。
type Store[T any] struct {
	path string
	mu   sync.RWMutex
	doc  T
	// writes 是落盘次数，测试用来断言「幂等读不写盘」。
	writes int
}

// Open 打开（或初始化）一个文档。文件不存在时用 init 造初值并落盘。
// 文件存在但内容损坏时返回错误——**不静默重建**，否则一次解析失败就会把线上数据清零。
func Open[T any](path string, init func() T) (*Store[T], error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("persist: 创建目录失败: %w", err)
	}
	st := &Store[T]{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(raw) > 0 {
			if uerr := json.Unmarshal(raw, &st.doc); uerr != nil {
				return nil, fmt.Errorf("persist: %s 解析失败: %w", path, uerr)
			}
			return st, nil
		}
		fallthrough
	case os.IsNotExist(err):
		st.doc = init()
		return st, st.flush()
	default:
		return nil, fmt.Errorf("persist: 读取 %s 失败: %w", path, err)
	}
}

// View 以只读方式访问文档。
func (s *Store[T]) View(fn func(*T)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.doc)
}

// Update 以写方式访问文档；回调返回错误则回滚内存到调用前状态。
func (s *Store[T]) Update(fn func(*T) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	backup, err := json.Marshal(s.doc)
	if err != nil {
		return fmt.Errorf("persist: 快照失败: %w", err)
	}
	if err := fn(&s.doc); err != nil {
		_ = json.Unmarshal(backup, &s.doc)
		return err
	}
	if err := s.flush(); err != nil {
		_ = json.Unmarshal(backup, &s.doc)
		return err
	}
	return nil
}

// Writes 返回落盘次数（测试用）。
func (s *Store[T]) Writes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writes
}

// flush 必须在持有写锁时调用。
func (s *Store[T]) flush() error {
	raw, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return fmt.Errorf("persist: 序列化失败: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp*")
	if err != nil {
		return fmt.Errorf("persist: 建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// rename 成功后该文件已不存在，这里的删除只兜底失败路径。
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("persist: 写临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("persist: fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("persist: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("persist: 原子替换失败: %w", err)
	}
	s.writes++
	return nil
}
