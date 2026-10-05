package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeFileAtomic：原子写内容正确、权限 0600、覆写不留残留与临时文件。
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	if err := writeFileAtomic(path, []byte("hello")); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("内容 = %q, 期望 %q", got, "hello")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0600 {
		t.Fatalf("权限 = %o, 期望 600", fi.Mode().Perm())
	}

	// 覆写应完整替换
	if err := writeFileAtomic(path, []byte("world!")); err != nil {
		t.Fatalf("覆写失败: %v", err)
	}
	if got, _ = os.ReadFile(path); string(got) != "world!" {
		t.Fatalf("覆写后内容 = %q, 期望 %q", got, "world!")
	}

	// 目录里应只剩目标文件，不留 .tmp-* 残留（rename 成功后的清理生效）
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "data.json" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("目录残留临时文件: %v", names)
	}
}

// writeFileAtomic：目标目录不存在时返回错误，且不会凭空造出目标文件。
func TestWriteFileAtomic_NonexistentDir(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing-sub", "x.json")
	if err := writeFileAtomic(bad, []byte("data")); err == nil {
		t.Fatal("目录不存在时写入应返回错误")
	}
	if _, err := os.Stat(bad); err == nil {
		t.Fatal("写入失败时不应创建目标文件")
	}
}

// 凭据落盘后可被新客户端完整读回，且 IsLoggedIn 反映登录态。
func TestILinkClient_CredentialsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")

	c := NewILinkClient(path)
	defer c.Shutdown()
	c.mu.Lock()
	c.botToken = "tok-123"
	c.botID = "bot-456"
	c.userID = "user-789"
	c.mu.Unlock()
	if c.IsLoggedIn() != true {
		t.Fatal("设置了 botToken+botID 后应视为已登录")
	}
	if err := c.SaveCredentials(); err != nil {
		t.Fatalf("保存凭据失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("凭据文件应已写出: %v", err)
	}

	c2 := NewILinkClient(path)
	defer c2.Shutdown()
	if c2.IsLoggedIn() {
		t.Fatal("新客户端在载入凭据前不应是已登录")
	}
	if err := c2.LoadCredentials(); err != nil {
		t.Fatalf("载入凭据失败: %v", err)
	}
	if c2.GetBotID() != "bot-456" || c2.GetUserID() != "user-789" {
		t.Errorf("载入的 ID 不符：bot=%q user=%q", c2.GetBotID(), c2.GetUserID())
	}
	c2.mu.RLock()
	tok := c2.botToken
	c2.mu.RUnlock()
	if tok != "tok-123" {
		t.Errorf("载入的 botToken = %q, 期望 tok-123", tok)
	}
	if !c2.IsLoggedIn() {
		t.Error("载入有效凭据后应视为已登录")
	}
}

// context_token 缓存按目录持久化，重启后可读回。
func TestILinkClient_ContextTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")

	c := NewILinkClient(path)
	defer c.Shutdown()
	c.SaveContextToken("userA", "tokA")

	// 缓存文件应与凭据同目录，名为 context_tokens.json
	ctxFile := filepath.Join(dir, "context_tokens.json")
	if _, err := os.Stat(ctxFile); err != nil {
		t.Fatalf("context_tokens.json 应已落盘: %v", err)
	}

	c2 := NewILinkClient(path)
	defer c2.Shutdown()
	if err := c2.LoadContextTokens(); err != nil {
		t.Fatalf("载入 context_token 失败: %v", err)
	}
	c2.mu.RLock()
	got := c2.contextTokens["userA"]
	c2.mu.RUnlock()
	if got != "tokA" {
		t.Errorf("contextTokens[userA] = %q, 期望 tokA", got)
	}
}

// ResetSession 清空内存登录态并把磁盘上的 context_tokens.json 一并删除，
// 否则重启后会把过期 token 载入回来。
func TestILinkClient_ResetSessionClearsDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	ctxFile := filepath.Join(dir, "context_tokens.json")

	c := NewILinkClient(path)
	defer c.Shutdown()
	c.mu.Lock()
	c.botToken = "tok"
	c.botID = "bot"
	c.mu.Unlock()
	c.SaveContextToken("u", "tk")
	if _, err := os.Stat(ctxFile); err != nil {
		t.Fatalf("前置：context_tokens.json 应存在: %v", err)
	}

	c.ResetSession()

	if c.IsLoggedIn() {
		t.Error("ResetSession 后不应仍视为已登录")
	}
	c.mu.RLock()
	n := len(c.contextTokens)
	c.mu.RUnlock()
	if n != 0 {
		t.Errorf("ResetSession 后内存 contextTokens 应为空，实际 %d 项", n)
	}
	if _, err := os.Stat(ctxFile); !os.IsNotExist(err) {
		t.Errorf("ResetSession 应删除磁盘 context_tokens.json，_stat  err=%v", err)
	}
}

// Shutdown 取消内部 context（用于优雅关闭长轮询）。
func TestILinkClient_ShutdownCancelsContext(t *testing.T) {
	c := NewILinkClient(filepath.Join(t.TempDir(), "creds.json"))
	c.Shutdown()
	if err := c.ctx.Err(); err != context.Canceled {
		t.Fatalf("Shutdown 后 ctx.Err() = %v, 期望 context.Canceled", err)
	}
}
