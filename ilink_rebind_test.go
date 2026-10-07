package main

// 网页重绑流程的离线端到端测试：用假 iLink 服务端跑完整扫码状态机。
// 覆盖 ilink_rebind.go 的成功/失败/幂等三条路径（否则这部分新代码无人验证）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeILinkLogin 造一个假 iLink 登录服务端：qrcode 接口 + 扫码状态接口。
// 状态轮询第 2 次起返回 confirmBody（为空则一直 wait）；statusSeq 记录轮询次数。
func fakeILinkLogin(t *testing.T, confirmBody string, statusSeq *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/ilink/bot/get_bot_qrcode":
			_, _ = w.Write([]byte(`{"ret":0,"qrcode":"QRKEY","qrcode_img_content":"https://liteapp.weixin.qq.com/q/abc?qrcode=QRKEY&bot_type=3"}`))
		case r.URL.Path == "/ilink/bot/get_qrcode_status":
			n := atomic.AddInt32(statusSeq, 1)
			if n >= 2 && confirmBody != "" {
				_, _ = w.Write([]byte(confirmBody))
				return
			}
			_, _ = w.Write([]byte(`{"ret":0,"status":"wait"}`))
		default:
			_, _ = w.Write([]byte(`{"ret":1,"errmsg":"unexpected path"}`))
		}
	}))
}

func newRebindClient(t *testing.T, baseURL string) *ILinkClient {
	t.Helper()
	c := NewILinkClient(filepath.Join(t.TempDir(), "ilink_credentials.json"))
	c.baseURL = baseURL
	t.Cleanup(c.Shutdown)
	return c
}

// B1：完整成功路径 —— 扫码 confirmed 后必须落盘新凭据、清掉过期位、状态可被网页读到。
func TestRebindSuccessPath(t *testing.T) {
	var seq int32
	confirm := `{"ret":0,"status":"confirmed","bot_token":"NEWTOKEN","ilink_bot_id":"newbot@im.bot","ilink_user_id":"newuser@im.wechat","baseurl":"https://example.invalid"}`
	srv := fakeILinkLogin(t, confirm, &seq)
	defer srv.Close()

	c := newRebindClient(t, srv.URL)
	// 前置：已有旧凭据且会话过期
	c.mu.Lock()
	c.botToken, c.botID, c.sessionExpired, c.cursor = "OLD", "oldbot", true, "OLDCURSOR"
	c.mu.Unlock()
	if err := c.SaveCredentials(); err != nil {
		t.Fatal(err)
	}

	started, err := c.StartRebind()
	if err != nil || !started {
		t.Fatalf("StartRebind 应启动成功，实得 started=%v err=%v", started, err)
	}
	// 第二次调用必须幂等：不重复取码（防刷接口触发限流）
	if again, _ := c.StartRebind(); again {
		t.Fatal("重绑进行中时第二次 StartRebind 不应再启动")
	}

	// 等到终态（假服务端第 2 次轮询即 confirmed）
	deadline := time.Now().Add(30 * time.Second)
	var snap map[string]interface{}
	for time.Now().Before(deadline) {
		snap = c.RebindSnapshot()
		if snap["status"] == "confirmed" || snap["status"] == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if snap["status"] != "confirmed" {
		t.Fatalf("重绑终态应为 confirmed，实得 %+v", snap)
	}
	if snap["logged_in"] != true {
		t.Fatalf("成功后应已登录，实得 %+v", snap)
	}
	if snap["session_expired"] != false {
		t.Fatal("成功后必须清掉会话过期位，否则主循环仍认为不可用")
	}
	if snap["qr_image"] == "" {
		t.Log("注意：终态下二维码已保留或清空均可，此处不作断言")
	}
	// 新凭据必须落盘（否则重启又要扫一次）
	data, err := os.ReadFile(c.tokenPath)
	if err != nil {
		t.Fatalf("新凭据应落盘: %v", err)
	}
	var cred ILinkCredentials
	if err := json.Unmarshal(data, &cred); err != nil {
		t.Fatal(err)
	}
	if cred.BotToken != "NEWTOKEN" || cred.BotID != "newbot@im.bot" {
		t.Fatalf("落盘凭据应为新会话的，实得 %+v", cred)
	}
	if !c.IsLoggedIn() {
		t.Fatal("IsLoggedIn 应为真")
	}
	if c.CommittedCursor() != "" {
		t.Fatalf("重绑后游标必须从头开始（旧会话游标无意义），实得 %q", c.CommittedCursor())
	}
}

// B2：取码失败路径 —— 服务端返回 ret!=0 时状态必须是 failed 并带上原因，且不挂死。
func TestRebindFailurePath(t *testing.T) {
	// 所有路径都返回 ret!=0 → 取码失败
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":1,"errmsg":"服务不可用"}`))
	}))
	defer srv.Close()

	c := newRebindClient(t, srv.URL)
	if _, err := c.StartRebind(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var snap map[string]interface{}
	for time.Now().Before(deadline) {
		snap = c.RebindSnapshot()
		if snap["status"] == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if snap == nil || snap["status"] != "failed" {
		t.Fatalf("取码失败应落终态 failed，实得 %+v", snap)
	}
	if snap["error"] == "" || snap["error"] == nil {
		t.Fatal("failed 状态必须带可读原因，供网页提示")
	}
	if snap["active"] != false {
		t.Fatal("失败后必须释放 active，否则主循环会永久挂起重绑分支")
	}
}

// B3：未初始化/nil 客户端读取快照不得 panic（API 层依赖其可观测性）。
func TestRebindSnapshotShape(t *testing.T) {
	c := newRebindClient(t, "http://127.0.0.1:1")
	snap := c.RebindSnapshot()
	for _, k := range []string{"active", "status", "qr_image", "error", "updated_at", "session_expired", "logged_in"} {
		if _, ok := snap[k]; !ok {
			t.Fatalf("快照缺少字段 %s（网页契约依赖它）", k)
		}
	}
	if snap["status"] != "idle" {
		t.Fatalf("初始状态应为 idle，实得 %v", snap["status"])
	}
}
