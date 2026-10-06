package maintenance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// state 是需要持久化的全部数据：资源、申请、紧急插入申请、执行中占用、
// 最终定案历史与通知记录。
type state struct {
	Resources     map[string]*Resource         `json:"resources"`
	Requests      map[string]*Request          `json:"requests"`
	Emergencies   map[string]*EmergencyRequest `json:"emergencies"`
	Occupancy     []Window                     `json:"occupancy"`
	History       []FinalRecord                `json:"history"`
	Notifications []Notification               `json:"notifications"`
}

func newState() state {
	return state{
		Resources:   make(map[string]*Resource),
		Requests:    make(map[string]*Request),
		Emergencies: make(map[string]*EmergencyRequest),
	}
}

// Store 负责把状态持久化为 JSON 文件。path 为空时仅驻留内存（测试用）。
type Store struct {
	path string
	st   state
}

// OpenStore 打开（或创建）位于 path 的存储。path 为空字符串时使用纯内存存储。
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, st: newState()}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("maintenance: read store: %w", err)
	case len(data) == 0:
		return s, nil
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, fmt.Errorf("maintenance: decode store: %w", err)
	}
	if s.st.Resources == nil {
		s.st.Resources = make(map[string]*Resource)
	}
	if s.st.Requests == nil {
		s.st.Requests = make(map[string]*Request)
	}
	if s.st.Emergencies == nil {
		s.st.Emergencies = make(map[string]*EmergencyRequest)
	}
	return s, nil
}

// save 原子写（临时文件 + rename），避免半截文件。
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return fmt.Errorf("maintenance: encode store: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("maintenance: write store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("maintenance: commit store: %w", err)
	}
	return nil
}
