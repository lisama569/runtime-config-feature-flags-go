package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Source 是一个配置来源。多个来源按优先级顺序叠加。
type Source interface {
	Name() string
	Load() (map[string]any, error)
}

// FileSource 从本地 JSON 文件加载配置。
type FileSource struct {
	Path string
}

func (s FileSource) Name() string { return "file:" + s.Path }

func (s FileSource) Load() (map[string]any, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read source %s: %w", s.Name(), err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse source %s: %w", s.Name(), err)
	}
	return m, nil
}

// MemorySource 是进程内配置源，常用于覆盖默认值或注入动态配置。
type MemorySource struct {
	Label  string
	Values map[string]any
}

func (s MemorySource) Name() string {
	if s.Label != "" {
		return "memory:" + s.Label
	}
	return "memory"
}

func (s MemorySource) Load() (map[string]any, error) {
	out := make(map[string]any, len(s.Values))
	for k, v := range s.Values {
		out[k] = v
	}
	return out, nil
}
