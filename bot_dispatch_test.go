package main

// P0 补齐：bot.go（产品两大入口之一，1058 行）此前无任何测试构造过 NewBot/HandleMessage。
// 这里做四件事，全部离线、零网络、零 LLM：
//  1. isCommand 判定表：命令词必须全 true、闲聊/记录分片必须 false（防非命令文本误入命令 switch）；
//  2. HandleMessage 分发：逐命令断言回复，覆盖帮助/列表/画像/备注/合并/删除/确认删除/状态/网址；
//  3. 帮助文本 ↔ 分发表一致性：新增命令忘了写进帮助会被这条钉住（历史上正是这类漂移）；
//  4. 非命令文本进缓冲不当场回复，且测试结束清空全局 pendingBatches 防计时器外溢。

import (
	"strings"
	"testing"
	"time"
)

func newTestBot(t *testing.T) *Bot {
	t.Helper()
	db := regressionDB(t)
	cfg := &Config{MyName: "我", Profile: ProfileConfig{ColdStartCount: 20, UpdateInterval: 10}}
	cli := NewILinkClient(t.TempDir() + "/ilink_credentials.json")
	return NewBot(db, NewLLMClient(cfg), cli, cfg)
}

func txtMsg(userID, text string) *ILinkMessage {
	return &ILinkMessage{FromUserID: userID, ItemList: []ILinkItem{{Type: 1, Text: &ILinkText{Text: text}}}}
}

// clearPendingBatches 清掉测试期间写入的全局缓冲，避免 AfterFunc 定时器在别的用例里触发。
func clearPendingBatches(t *testing.T) {
	t.Helper()
	pendingBatchMu.Lock()
	for uid, p := range pendingBatches {
		if p != nil && p.timer != nil {
			p.timer.Stop()
		}
		delete(pendingBatches, uid)
	}
	pendingBatchMu.Unlock()
}

func TestBotIsCommandSet(t *testing.T) {
	wants := []string{"帮助", "help", "?", "列表", "画像", "历史", "备注", "补充", "合并", "撤销合并",
		"合并记录", "删除", "统计", "重登", "状态", "确认删除", "改写", "草稿检查", "画像变化", "面板", "网址", "地址"}
	for _, w := range wants {
		if !isCommand(w) {
			t.Fatalf("命令 %q 应被识别为命令（分发表与 isCommand 已漂移）", w)
		}
	}
	notCmd := []string{"张三", "小齐", "你好", "在吗", "2026年10月5日", "workbuddy"}
	for _, s := range notCmd {
		if isCommand(s) {
			t.Fatalf("非命令 %q 被判为命令，会让聊天记录分片误入命令分支", s)
		}
	}
}

func TestBotHandleMessageDispatch(t *testing.T) {
	b := newTestBot(t)
	defer clearPendingBatches(t)

	cases := []struct {
		name         string
		text         string
		wantContains []string
	}{
		{"帮助", "帮助", []string{"列表", "画像", "改写", "草稿检查", "删除"}},
		{"help英文", "help", []string{"微信画像助手命令"}},
		{"问号", "?", []string{"列表"}},
		{"列表空库", "列表", []string{"暂无联系人"}},
		{"画像空库", "画像", []string{"暂无联系人"}},
		{"历史空库", "历史", []string{"历史", "暂无"}},
		{"备注缺参", "备注", []string{"备注"}},
		{"合并缺参", "合并", []string{"合并"}},
		{"撤销合并无记录", "撤销合并 某人", []string{"撤销", "没", "未", "无"}},
		{"合并记录空", "合并记录", []string{"合并", "暂", "没", "无", "未"}},
		{"删除不存在", "删除 查无此人", []string{"未找到", "没有", "不存在", "暂无"}},
		{"确认删除无待确认", "确认删除 某人", []string{"没有", "无", "过期", "待确认"}},
		{"统计缺参", "统计", []string{"统计", "昵称", "用法"}},
		{"状态", "状态", []string{"运行状态", "登录", "数据库"}},
		{"网址", "网址", []string{"http", "未配置", "未"}},
	}
	for _, tc := range cases {
		reply := b.HandleMessage(txtMsg("u-"+tc.name, tc.text))
		if reply == "" {
			t.Errorf("%s: 命令 %q 返回空回复", tc.name, tc.text)
			continue
		}
		ok := false
		for _, w := range tc.wantContains {
			if strings.Contains(reply, w) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("%s: 回复未含任一期望词 %v，实得：%q", tc.name, tc.wantContains, truncate(reply))
		}
	}
}

// TestBotHelpMatchesDispatch 钉住「帮助文本列出所有面向用户的命令」：
// 新增命令却忘记写帮助，或帮助写了却没接进分发，都会在这里红。
func TestBotHelpMatchesDispatch(t *testing.T) {
	b := newTestBot(t)
	help := b.helpText()
	// 面向用户的命令（不含 "?"、"面板/地址" 这类别名）
	for _, cmd := range []string{"帮助", "列表", "画像", "历史", "备注", "补充", "合并", "撤销合并",
		"合并记录", "删除", "确认删除", "统计", "状态", "重登", "网址", "改写", "草稿检查", "画像变化"} {
		if !isCommand(cmd) {
			t.Fatalf("%q 未在 isCommand 中", cmd)
		}
		if !strings.Contains(help, cmd) {
			t.Fatalf("%q 已注册为命令但帮助文本里找不到（用户无从得知）", cmd)
		}
	}
}

func TestBotNonCommandTextIsBuffered(t *testing.T) {
	b := newTestBot(t)
	defer clearPendingBatches(t)
	uid := "u-buffer"
	if got := b.HandleMessage(txtMsg(uid, "张三 2026年10月5日 10:00\n你好呀")); got != "" {
		t.Fatalf("非命令文本应静默缓冲（不当场回复），实得：%q", got)
	}
	if !hasPendingBatch(uid) {
		t.Fatal("非命令文本未进入待合并缓冲")
	}
	// 第二片继续累加
	b.HandleMessage(txtMsg(uid, "张三 2026年10月5日 10:01\n在忙吗"))
	pendingBatchMu.Lock()
	n := len(pendingBatches[uid].texts)
	pendingBatchMu.Unlock()
	if n != 2 {
		t.Fatalf("缓冲应有 2 片，实得 %d", n)
	}
}

func TestBotMediaWithoutBatchHints(t *testing.T) {
	b := newTestBot(t)
	msg := &ILinkMessage{FromUserID: "u-media", ItemList: []ILinkItem{{Type: 3}}}
	if got := b.HandleMessage(msg); !strings.Contains(got, "只支持文本") {
		t.Fatalf("单独发非文本消息应给出可用提示，实得：%q", got)
	}
}

func TestBotPendingDeleteConfirmFlow(t *testing.T) {
	user := "u-del-" + time.Now().Format("150405.000")
	// setPendingDelete 会自己按 TTL 设过期，取回应有效且一次性消费
	setPendingDelete(user, pendingDelete{display: "小明"})
	got, ok := takePendingDelete(user)
	if !ok {
		t.Fatal("takePendingDelete 应取回刚登记的待确认项")
	}
	if got.display != "小明" {
		t.Fatalf("展示名应回传，实得 %q", got.display)
	}
	if _, ok := takePendingDelete(user); ok {
		t.Fatal("待确认项应已被消费，第二次不应取到（防重复删除）")
	}
	// 过期项不得取回（防隔很久误删他人联系人）：直接写入一个已过期的条目
	pendingDeleteMu.Lock()
	pendingDeletes[user] = pendingDelete{display: "小红", expiresAt: time.Now().Add(-time.Second)}
	pendingDeleteMu.Unlock()
	if _, ok := takePendingDelete(user); ok {
		t.Fatal("已过期的待确认项不应被取回")
	}
}
