package api

import (
	"net/http"

	"github.com/ZhcChen/qfy-agent/agent/backend"
)

// ownedBy /v1/models 列表项的固定归属标识（R1）。
const ownedBy = "qfy-agent"

// modelListItem OpenAI /v1/models 列表项（R1）。
type modelListItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// modelList OpenAI /v1/models 响应结构（R1：object=list + data 数组）。
type modelList struct {
	Object string          `json:"object"`
	Data   []modelListItem `json:"data"`
}

// handleModels GET /v1/models（R1）：返回注册表中的模型列表（声明顺序，R6）。
// created 无时间戳语义，固定为 0（R1 允许 0 或当前时间，取确定值便于断言）。
func (h *handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, backend.ErrorBody{
			Message: "方法不允许，期望 GET",
			Type:    errTypeInvalidRequest,
			Code:    errCodeMethodNotAllowed,
		})
		return
	}
	models := h.cfg.Registry.List()
	data := make([]modelListItem, 0, len(models))
	for _, m := range models {
		data = append(data, modelListItem{
			ID:      m.ID,
			Object:  "model",
			Created: 0,
			OwnedBy: ownedBy,
		})
	}
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: data})
}
