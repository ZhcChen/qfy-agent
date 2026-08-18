package main

import (
	"sync"

	"github.com/qfy-agent/qfy-agent/audit"
)

// auditStore 模拟消费方落库：内存环形存储最近 N 条审计记录（R17 演示）。
// 生产消费方应在此替换为真实数据库写入；回调并发调用（F1），实现须线程安全。
type auditStore struct {
	mu    sync.Mutex
	recs  []audit.CallRecord
	limit int
}

// newAuditStore 构造审计存储，保留最近 limit 条记录。
func newAuditStore(limit int) *auditStore {
	if limit <= 0 {
		limit = 200
	}
	return &auditStore{limit: limit}
}

// OnCall 实现 audit.OnCall：回调在请求 goroutine 内同步触发（F1），
// 本实现只做内存追加（消费方落库耗时须可控，README 建议异步化）。
func (s *auditStore) OnCall(rec audit.CallRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	if len(s.recs) > s.limit {
		s.recs = s.recs[len(s.recs)-s.limit:]
	}
}

// List 返回当前全部记录（新→旧顺序）。
func (s *auditStore) List() []audit.CallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]audit.CallRecord, len(s.recs))
	for i := range s.recs {
		out[i] = s.recs[len(s.recs)-1-i]
	}
	return out
}
