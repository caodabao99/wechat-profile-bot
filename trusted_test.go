package main

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestTrustedStore 用临时目录构造 store，避免碰真实 dataDir() 文件
func newTestTrustedStore(t *testing.T) *trustedClientStore {
	t.Helper()
	return &trustedClientStore{
		clients: map[string]*TrustedClient{},
		path:    filepath.Join(t.TempDir(), "trusted_clients.json"),
	}
}

func TestTrustedStoreCreateValidateRevoke(t *testing.T) {
	st := newTestTrustedStore(t)
	tc, err := st.create("Windows · Chrome", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Token) != 64 {
		t.Errorf("令牌应为 32 字节 hex（64 字符），实际 %d", len(tc.Token))
	}
	// 有效令牌：校验通过并滑动续期
	got := st.validate(tc.Token, "5.6.7.8")
	if got == nil {
		t.Fatal("有效令牌校验失败")
	}
	if got.LastUsedAt == "" || got.LastIP != "5.6.7.8" {
		t.Errorf("续期信息未更新: %+v", got)
	}
	exp, _ := time.Parse(time.RFC3339, got.ExpiresAt)
	if time.Until(exp) < trustedClientTTL-time.Hour {
		t.Error("校验后到期时间应滑动续期到接近完整 TTL")
	}
	// 无效/空令牌
	if st.validate("not-exist", "") != nil || st.validate("", "") != nil {
		t.Error("无效令牌不应通过校验")
	}
	// 按前缀吊销（管理页只展示打码令牌）
	if !st.revoke(tc.Token[:8]) {
		t.Error("前缀吊销失败")
	}
	if st.validate(tc.Token, "") != nil {
		t.Error("吊销后令牌不应再有效")
	}
	// 重复吊销 / 全部吊销
	if st.revoke(tc.Token) {
		t.Error("重复吊销应返回 false")
	}
	tc2, _ := st.create("设备2", "")
	st.revokeAll()
	if st.validate(tc2.Token, "") != nil {
		t.Error("revokeAll 后不应有存活令牌")
	}
	if len(st.list()) != 0 {
		t.Error("revokeAll 后列表应为空")
	}
}

func TestTrustedStoreExpiry(t *testing.T) {
	st := newTestTrustedStore(t)
	expired := &TrustedClient{
		Name: "过期设备", Token: "aaaa1111bbbb2222",
		CreatedAt: time.Now().Add(-100 * 24 * time.Hour).Format(time.RFC3339),
		ExpiresAt: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}
	st.clients[expired.Token] = expired
	if st.validate(expired.Token, "") != nil {
		t.Error("过期令牌不应通过校验")
	}
	if _, ok := st.clients[expired.Token]; ok {
		t.Error("过期令牌校验时应被清除")
	}
}

func TestTrustedStorePersistence(t *testing.T) {
	st := newTestTrustedStore(t)
	tc, _ := st.create("持久化设备", "9.9.9.9")
	// 手工塞一条过期记录，load 时应被丢弃
	st.clients["deadbeefdeadbeef"] = &TrustedClient{
		Name: "旧的", Token: "deadbeefdeadbeef",
		CreatedAt: time.Now().Add(-200 * 24 * time.Hour).Format(time.RFC3339),
		ExpiresAt: time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
	}
	st.mu.Lock()
	st.saveLocked()
	st.mu.Unlock()

	// 重新加载：有效条目在、过期条目被丢弃
	st2 := &trustedClientStore{clients: map[string]*TrustedClient{}, path: st.path}
	st2.load()
	if st2.validate(tc.Token, "") == nil {
		t.Error("重启后有效令牌应能通过校验")
	}
	if st2.validate("deadbeefdeadbeef", "") != nil {
		t.Error("重启后过期令牌不应复活")
	}
}

func TestTrustedStoreMaxLimitEviction(t *testing.T) {
	st := newTestTrustedStore(t)
	// 填满上限，CreatedAt 递增（第一条最旧）
	base := time.Now().Add(-time.Duration(maxTrustedClients) * time.Hour)
	for i := 0; i < maxTrustedClients; i++ {
		tok := string(rune('a'+i)) + "tok"
		st.clients[tok] = &TrustedClient{
			Name: tok, Token: tok,
			CreatedAt: base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339),
		}
	}
	// 把「CreatedAt 最旧」的一条标记为刚使用过：淘汰按 LastUsedAt（缺省
	// CreatedAt）最旧者，应落在第二旧的 btok 上而不是 atok
	newest := base.Add(time.Duration(maxTrustedClients-1) * time.Hour)
	st.clients["atok"].LastUsedAt = newest.Format(time.RFC3339)

	tc, err := st.create("新设备", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.clients) != maxTrustedClients {
		t.Errorf("设备数应保持在 %d，实际 %d", maxTrustedClients, len(st.clients))
	}
	if _, ok := st.clients["atok"]; !ok {
		t.Error("刚使用过的最旧设备不应被淘汰（LastUsedAt 新）")
	}
	if _, ok := st.clients["btok"]; ok {
		t.Error("应淘汰最久未使用的设备 btok")
	}
	if _, ok := st.clients[tc.Token]; !ok {
		t.Error("新签发的设备应在列表中")
	}
}

func TestTrustedListMasked(t *testing.T) {
	st := newTestTrustedStore(t)
	tc, _ := st.create("打码测试", "")
	list := st.list()
	if len(list) != 1 {
		t.Fatalf("列表应有 1 条，实际 %d", len(list))
	}
	row := list[0]
	if row["token"] != tc.Token[:8]+"…" {
		t.Errorf("令牌应打码，实际 %q", row["token"])
	}
	if row["tokenPrefix"] != tc.Token[:8] {
		t.Errorf("tokenPrefix 不符: %q", row["tokenPrefix"])
	}
	if row["name"] != "打码测试" {
		t.Errorf("设备名不符: %q", row["name"])
	}
}
