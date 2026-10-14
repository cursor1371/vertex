package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/engine/recaptcha"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// newStopTestFixture 建一个已登录的 admin 测试服务，并绑定内存统计仓储
// （在途注册表与取消控制块都依赖 stats 已 Init）。
func newStopTestFixture(t *testing.T) (srvURL, cookie string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stats.Init(store)

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})
	return fx.server.URL, adminLogin(t, fx.server.URL)
}

// TestAdminStopRequest_CancelsInflight 强制停止接口命中在途请求时取消其上下文。
func TestAdminStopRequest_CancelsInflight(t *testing.T) {
	srvURL, cookie := newStopTestFixture(t)

	const reqID = "req-stop-1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stats.StartReq(reqID, "model-a", "", "k", true)
	stats.SetReqCancel(reqID, cancel)
	defer stats.StopReq(reqID)

	resp := adminRequest(t, srvURL, "/requests/stop", cookie, map[string]any{"req_id": reqID})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["stopped"] != true {
		t.Fatalf("stopped=%v, want true", body["stopped"])
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("接口应取消该请求的上下文")
	}
	if !stats.ReqStoppedByAdmin(reqID) {
		t.Fatal("停止后应标记为被管理员停止（用于返回明确错误）")
	}
}

// TestAdminStopRequest_UnknownAndBadBody 未知请求返回 stopped=false；缺 req_id 返回 400；
// GET 不被允许（只接受 POST）。
func TestAdminStopRequest_UnknownAndBadBody(t *testing.T) {
	srvURL, cookie := newStopTestFixture(t)

	resp := adminRequest(t, srvURL, "/requests/stop", cookie, map[string]any{"req_id": "not-exist"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未知请求 status=%d, want 200（stopped=false）", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["stopped"] != false {
		t.Fatalf("未知请求 stopped=%v, want false", body["stopped"])
	}

	resp2 := adminRequest(t, srvURL, "/requests/stop", cookie, map[string]any{})
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 req_id status=%d, want 400", resp2.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, srvURL+"/api/admin/requests/stop", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie})
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405", resp3.StatusCode)
	}
}

// TestSubstituteAdminStop 管理员停止时替换为可区分的明确错误（503 + 明确文案）；
// 客户端自己断开或普通故障时原样透传。
func TestSubstituteAdminStop(t *testing.T) {
	initTestStatsForSubstitute(t)

	const reqID = "req-sub-1"
	stats.StartReq(reqID, "m", "", "k", false)
	defer stats.StopReq(reqID)

	generic := vertex.NewInternalError("context canceled", nil)
	if got := substituteAdminStop(reqID, generic); got != generic {
		t.Fatal("未标记为管理员停止时应原样返回")
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	stats.SetReqCancel(reqID, cancel)
	stats.CancelReq(reqID)

	got := substituteAdminStop(reqID, generic)
	if got == generic {
		t.Fatal("管理员停止时应替换错误")
	}
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503", got.Code)
	}
	if !strings.Contains(got.Message, "强制停止") {
		t.Fatalf("文案应说明被强制停止，实际 %q", got.Message)
	}
	if substituteAdminStop(reqID, nil) != nil {
		t.Fatal("nil 错误应原样返回")
	}
}

// initTestStatsForSubstitute 绑定内存仓储，保证在途注册表可用。
func initTestStatsForSubstitute(t *testing.T) {
	t.Helper()
	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stats.Init(store)
}

// fakeRTTokenSource 是 RTTokenSource 的测试替身。
type fakeRTTokenSource struct{ events []recaptcha.TokenEvent }

func (f *fakeRTTokenSource) RecentTokenEvents(limit int) []recaptcha.TokenEvent {
	if limit <= 0 || limit > len(f.events) {
		limit = len(f.events)
	}
	return f.events[:limit]
}

func (f *fakeRTTokenSource) WarmStats() recaptcha.WarmSnapshot { return recaptcha.WarmSnapshot{} }

// TestAdminGetRTTokens 记录流接口：按 limit 返回最近事件；未装配来源时返回空数组而非 null。
func TestAdminGetRTTokens(t *testing.T) {
	events := []recaptcha.TokenEvent{
		{Seq: 2, Source: recaptcha.TokenSourceLive, NodeName: "hk-01", LatencyMs: 320, OK: true},
		{Seq: 1, Source: recaptcha.TokenSourcePrefetch, LatencyMs: 180, OK: false, Err: "dial timeout"},
	}

	t.Run("已装配来源", func(t *testing.T) {
		adm := &AdminHandler{handler{deps: ServerDeps{RTTok: &fakeRTTokenSource{events: events}}}}
		rec := httptest.NewRecorder()
		adm.adminGetRTTokens(rec, httptest.NewRequest(http.MethodGet, "/api/admin/stats/rt-token?limit=1", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", rec.Code)
		}
		var body struct {
			Events []recaptcha.TokenEvent `json:"events"`
			Count  int                    `json:"count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Count != 1 || len(body.Events) != 1 || body.Events[0].Seq != 2 {
			t.Fatalf("limit=1 应只返回最新一条: %+v", body)
		}
		if body.Events[0].NodeName != "hk-01" || body.Events[0].LatencyMs != 320 {
			t.Fatalf("事件字段应原样透出: %+v", body.Events[0])
		}
	})

	t.Run("未装配来源返回空数组", func(t *testing.T) {
		adm := &AdminHandler{} //nolint:exhaustruct
		rec := httptest.NewRecorder()
		adm.adminGetRTTokens(rec, httptest.NewRequest(http.MethodGet, "/api/admin/stats/rt-token", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"events":[]`) {
			t.Fatalf("应返回空数组而非 null: %s", rec.Body.String())
		}
	})
}
