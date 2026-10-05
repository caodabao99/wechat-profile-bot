package main

import (
	"database/sql"
	"strings"
	"testing"
)

// countMsgs 统计某联系人当前在 messages 表里的条数（合并/撤销的可观测副作用）。
func countMsgs(t *testing.T, db *sql.DB, contactID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE contact_id = ?`, contactID).Scan(&n); err != nil {
		t.Fatalf("统计消息条数失败: %v", err)
	}
	return n
}

func otherMsgCount(t *testing.T, db *sql.DB, contactID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT other_msg_count FROM contacts WHERE id = ?`, contactID).Scan(&n); err != nil {
		t.Fatalf("读取 other_msg_count 失败: %v", err)
	}
	return n
}

// isSelfName：空名/普通名不算自己；"我"/"me"（忽略大小写）与配置的本人昵称算自己。
func TestIsSelfName(t *testing.T) {
	if config == nil {
		config = &Config{}
	}
	old := config.MyName
	defer func() { config.MyName = old }()

	cases := []struct {
		name   string
		myName string
		want   bool
	}{
		{"我", "", true},      // 字面「我」恒判定为自己
		{"me", "", true},     // 英文 me
		{"ME", "", true},     // 大小写不敏感
		{"  我  ", "", true},  // 前后空白会被裁剪
		{"张三", "", false},    // 普通联系人名
		{"", "张三", false},    // 空名不是自己
		{"张三", "张三", true},   // 命中配置的本人昵称
		{"李四", "张三", false},  // 与本人昵称不同
		{" 张三 ", "张三", true}, // 裁剪空白后命中本人昵称
	}
	for _, c := range cases {
		config.MyName = c.myName
		if got := isSelfName(c.name); got != c.want {
			t.Errorf("isSelfName(%q) with MyName=%q = %v, 期望 %v", c.name, c.myName, got, c.want)
		}
	}
}

// 合并主流程：消息搬到目标、源标记 merged_into、别名登记、计数重算，且可完整撤销。
func TestMergeContacts_HappyPathAndUndo(t *testing.T) {
	db := regressionDB(t)
	src := regressionContact(t, db, "AliceSrc")
	tgt := regressionContact(t, db, "BobTgt")
	regressionMessages(t, db, src, "src-1", "src-2")
	regressionMessages(t, db, tgt, "tgt-1")

	if n := countMsgs(t, db, src); n != 2 {
		t.Fatalf("前置：源应有 2 条消息，实际 %d", n)
	}

	res, err := MergeContacts(db, src, tgt, MergeOptions{})
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if res.MovedMessages != 2 {
		t.Errorf("MovedMessages = %d, 期望 2", res.MovedMessages)
	}
	if res.MergeLogID == 0 {
		t.Errorf("应返回有效的 merge_log ID")
	}

	// 消息归属：源清空，目标收拢全部 3 条
	if n := countMsgs(t, db, src); n != 0 {
		t.Errorf("合并后源消息数 = %d, 期望 0", n)
	}
	if n := countMsgs(t, db, tgt); n != 3 {
		t.Errorf("合并后目标消息数 = %d, 期望 3", n)
	}
	// 目标对方消息数应全量重算为 3（都是 sender=other）
	if n := otherMsgCount(t, db, tgt); n != 3 {
		t.Errorf("目标 other_msg_count = %d, 期望 3", n)
	}

	// 源被标记为已合并到目标
	merged, into, err := IsMerged(db, src)
	if err != nil {
		t.Fatal(err)
	}
	if !merged || into != tgt {
		t.Errorf("IsMerged(src) = (%v, %d), 期望 (true, %d)", merged, into, tgt)
	}

	// 别名：源原名与目标原名都登记到目标名下
	aliases, err := GetAliases(db, tgt)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(aliases, ",")
	if !strings.Contains(joined, "AliceSrc") || !strings.Contains(joined, "BobTgt") {
		t.Errorf("目标别名应含源/目标原名，实际: %v", aliases)
	}

	// 合并日志存在
	logs, err := GetMergeLogs(db, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].SourceID != src || logs[0].TargetID != tgt {
		t.Fatalf("合并日志异常: %+v", logs)
	}

	// 撤销：消息搬回、merged_into 清空、目标未撤销日志归零
	if err := UndoMerge(db, res.MergeLogID); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if n := countMsgs(t, db, src); n != 2 {
		t.Errorf("撤销后源消息数 = %d, 期望 2", n)
	}
	if n := countMsgs(t, db, tgt); n != 1 {
		t.Errorf("撤销后目标消息数 = %d, 期望 1", n)
	}
	if merged, _, _ := IsMerged(db, src); merged {
		t.Errorf("撤销后源不应仍处于已合并状态")
	}
	undone, err := GetMergeLogsForTarget(db, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if len(undone) != 0 {
		t.Errorf("撤销后不应再查得到未撤销的合并日志，实际 %d 条", len(undone))
	}
}

// 合并的各种前置校验必须在动数据之前拦截，返回明确错误。
func TestMergeContacts_ValidationGuards(t *testing.T) {
	db := regressionDB(t)
	a := regressionContact(t, db, "ValidA")
	b := regressionContact(t, db, "ValidB")
	self := regressionContact(t, db, "我")

	t.Run("不能合并到自身", func(t *testing.T) {
		if _, err := MergeContacts(db, a, a, MergeOptions{}); err == nil ||
			!strings.Contains(err.Error(), "自身") {
			t.Fatalf("期望「不能合并到自身」，实际: %v", err)
		}
	})
	t.Run("源是自己", func(t *testing.T) {
		if _, err := MergeContacts(db, self, a, MergeOptions{}); err == nil ||
			!strings.Contains(err.Error(), "不能合并自己") {
			t.Fatalf("期望拦截「源是自己」，实际: %v", err)
		}
	})
	t.Run("目标是自己", func(t *testing.T) {
		if _, err := MergeContacts(db, a, self, MergeOptions{}); err == nil ||
			!strings.Contains(err.Error(), "合并进自己") {
			t.Fatalf("期望拦截「合并进自己」，实际: %v", err)
		}
	})
	t.Run("不存在的联系人", func(t *testing.T) {
		if _, err := MergeContacts(db, 999999, a, MergeOptions{}); err == nil {
			t.Fatal("源不存在应报错")
		}
	})

	// 先正常合并 a→b，再对已被合并的源重复合并应被拦下
	if _, err := MergeContacts(db, a, b, MergeOptions{}); err != nil {
		t.Fatalf("前置合并失败: %v", err)
	}
	if _, err := MergeContacts(db, a, b, MergeOptions{}); err == nil ||
		!strings.Contains(err.Error(), "已被合并") {
		t.Fatalf("期望拦截「源联系人已被合并」，实际: %v", err)
	}
}

// 已合并（且未撤销）的联系人不应再出现在合并目标候选里。
func TestGetMergeCandidates_ExcludesMerged(t *testing.T) {
	db := regressionDB(t)
	x := regressionContact(t, db, "CandX")
	y := regressionContact(t, db, "CandY")
	z := regressionContact(t, db, "CandZ")

	if _, err := MergeContacts(db, z, y, MergeOptions{}); err != nil { // z 并入 y
		t.Fatalf("合并失败: %v", err)
	}

	cands, err := GetMergeCandidates(db, x) // 排除自己 x
	if err != nil {
		t.Fatal(err)
	}
	var hasMerged, hasSelf, hasTarget bool
	for _, c := range cands {
		switch c.ID {
		case z:
			hasMerged = true
		case x:
			hasSelf = true
		case y:
			hasTarget = true
		}
	}
	if hasMerged {
		t.Errorf("候选不应包含已合并的联系人 %d", z)
	}
	if hasSelf {
		t.Errorf("候选不应包含被排除的自身 %d", x)
	}
	if !hasTarget {
		t.Errorf("候选应包含未被合并的目标联系人 %d", y)
	}
}
