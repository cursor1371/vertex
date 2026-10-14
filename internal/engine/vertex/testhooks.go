package vertex

import "github.com/bsfdsagfadg/vertex/internal/engine/recaptcha"

// SetBatchGraphqlURL 覆盖 batchGraphql 端点（测试专用）；传空串恢复动态计算。
func (c *VertexAIClient) SetBatchGraphqlURL(url string) {
	c.batchURL = url
}

// SetTokenPool replaces the token pool for testing.
// 替换前先关闭客户端自建的旧池并移交所有权：构造时传 nil 会自建池并启动后台
// 协程，直接覆盖指针会让旧池的协程永远无法关闭（测试夹具的真实泄漏点）。
func (c *VertexAIClient) SetTokenPool(pool *recaptcha.TokenPool) {
	if c.ownsPool && c.pool != nil && c.pool != pool {
		c.pool.Close()
	}
	c.ownsPool = false
	c.pool = pool
}
