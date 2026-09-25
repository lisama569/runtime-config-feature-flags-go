package config

import (
	"fmt"
	"time"
)

// Loader 按来源优先级顺序加载并合并配置。
// 来源列表中越靠后的来源优先级越高（覆盖前者）。
type Loader struct {
	Sources []Source
	Schema  Schema
}

// Load 加载、合并并校验配置，返回不可变快照。
// 任一来源加载失败或校验被拒绝都会返回错误，不产生半成品快照。
func (l *Loader) Load(version int64) (*Snapshot, error) {
	merged := make(map[string]any)
	for _, src := range l.Sources {
		vals, err := src.Load()
		if err != nil {
			return nil, fmt.Errorf("load source %s: %w", src.Name(), err)
		}
		// 后加载的来源覆盖先加载的来源，优先级由列表顺序确定。
		for k, v := range vals {
			merged[k] = v
		}
	}
	if err := l.Schema.Validate(merged); err != nil {
		return nil, err
	}
	// 复制一份，保证快照不可变。
	frozen := make(map[string]any, len(merged))
	for k, v := range merged {
		frozen[k] = v
	}
	return &Snapshot{Version: version, Values: frozen, UpdatedAt: time.Now()}, nil
}
