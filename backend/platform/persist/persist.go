// Package persist is the minimal persistence layer of the management platform (Go).
//
// P0 does one thing only: keep **one JSON document** entirely in memory and write the whole thing to disk atomically on every change
// (temp file in the same directory + rename). Reasons for choosing it:
//
//   - the platform is on the governance side with low QPS and does not need a database's concurrency;
//   - rule packages / fact catalogs / audit trails are all shaped as "document + append-only" in the contracts,
//     so whole-document writes naturally satisfy "atomic + immutable";
//   - switching to a real DB requires replacing only this package, since business code depends only on the View/Update semantics.
//
// Two disciplines:
//  1. the pointer obtained by `View` is **read-only**; mutating it inside the callback is forbidden (otherwise writes become uncontrollable).
//  2. when the `Update` callback returns an error the **whole document is rolled back**, leaving no half-written state.
package persist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store is the storage of a single JSON document.
type Store[T any] struct {
	path string
	mu   sync.RWMutex
	doc  T
	// writes is the number of disk writes, used by tests to assert "idempotent reads do not write to disk".
	writes int
}

// Open opens (or initialises) a document. When the file does not exist, init builds the initial value and it is written to disk.
// When the file exists but its content is corrupt an error is returned — it is **never silently rebuilt**, otherwise one parse failure would wipe production data.
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

// View accesses the document read-only.
func (s *Store[T]) View(fn func(*T)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.doc)
}

// Update accesses the document for writing; if the callback returns an error, memory is rolled back to the state before the call.
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

// Writes returns the number of disk writes (for tests).
func (s *Store[T]) Writes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writes
}

// flush must be called while holding the write lock.
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
		// after a successful rename the file no longer exists; this removal only covers the failure path.
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
