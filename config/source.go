// Package config 提供多来源配置的分层加载、合并、校验与原子更新。
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Source 表示一个配置来源。优先级由 Loader 中来源的顺序决定，
// 索引越大的来源优先级越高（后加载覆盖先加载）。
type Source interface {
	// Name 返回来源名称，用于日志与错误定位。
	Name() string
	// Load 读取该来源的原始配置（扁平键值对，值为 JSON 编码的原始字节）。
	Load() (map[string][]byte, error)
}

// FileSource 从本地 JSON 文件加载配置。文件内容必须是扁平的 JSON 对象。
type FileSource struct {
	Path string
}

func (s *FileSource) Name() string { return "file:" + s.Path }

func (s *FileSource) Load() (map[string][]byte, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, &RejectError{Reason: ReasonSourceIO, Source: s.Name(), Detail: err.Error()}
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &RejectError{Reason: ReasonSourceIO, Source: s.Name(), Detail: "invalid JSON: " + err.Error()}
	}
	out := make(map[string][]byte, len(raw))
	for k, v := range raw {
		out[k] = append([]byte(nil), v...)
	}
	return out, nil
}

// MemorySource 是进程内配置源，可用于模拟远程下发或测试。
type MemorySource struct {
	SourceName string
	Values     map[string][]byte
}

func (s *MemorySource) Name() string { return s.SourceName }

func (s *MemorySource) Load() (map[string][]byte, error) {
	if s.Values == nil {
		return nil, &RejectError{Reason: ReasonSourceIO, Source: s.Name(), Detail: "nil values"}
	}
	out := make(map[string][]byte, len(s.Values))
	for k, v := range s.Values {
		out[k] = append([]byte(nil), v...)
	}
	return out, nil
}

// MustJSON 是测试与示例辅助：把值编码为 JSON 原始字节。
func MustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("MustJSON: %v", err))
	}
	return b
}
