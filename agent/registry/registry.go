package registry

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// doc 是注册表配置文件的根结构。
type doc struct {
	Models []*Model `yaml:"models"`
}

// Registry 是加载后不可变的模型注册表（KTD9/F5：并发只读安全）。
type Registry struct {
	models map[string]*Model
	order  []string
}

// Load 从 YAML 字节解析注册表并校验全部声明。
// 消费方负责读取配置文件；库不触碰文件系统与环境变量（R18）。
func Load(data []byte) (*Registry, error) {
	var d doc
	if err := yaml.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("解析模型注册表 YAML: %w", err)
	}
	if len(d.Models) == 0 {
		return nil, fmt.Errorf("模型注册表为空：至少声明一个模型")
	}
	r := &Registry{models: make(map[string]*Model, len(d.Models))}
	seen := make(map[string]bool, len(d.Models))
	for _, m := range d.Models {
		if err := m.validate(); err != nil {
			return nil, err
		}
		key := strings.TrimSpace(m.ID)
		if seen[key] {
			return nil, fmt.Errorf("模型 id %q 重复声明", m.ID)
		}
		seen[key] = true
		r.models[key] = m
		r.order = append(r.order, key)
	}
	return r, nil
}

// Get 返回指定对外模型 id 的声明；不存在时返回错误。
func (r *Registry) Get(id string) (*Model, error) {
	m, ok := r.models[id]
	if !ok {
		return nil, fmt.Errorf("模型 %q 不在注册表中", id)
	}
	return m, nil
}

// List 返回按声明顺序排列的全部模型（供 /v1/models）。
func (r *Registry) List() []*Model {
	out := make([]*Model, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.models[id])
	}
	return out
}
