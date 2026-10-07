package main

// 消息收取可靠性（at-least-once）支撑层。
//
// 背景（投产前审计 P1）：旧实现是「先去重后处理」——GetUpdates 一拿到消息就把 message id
// 写进内存 recentIDs 并当场推进游标，之后才交给主循环处理。后果是：
//   - 处理中途 panic / 进程崩溃 / DB 写失败 → 游标已前进、id 已标记，消息**永久丢失**；
//   - 游标只在内存里（凭据文件不含游标）→ 重启时空游标按协议**可能重放整段历史**，
//     只剩 5 分钟内存表挡着，超出窗口就重复入库。
//
// 现做法：
//   - 游标后置提交：GetUpdates 只记 pendingBuf；整批处理成功后 CommitCursor() 才推进并落盘。
//   - 持久幂等账本 ingest_ledger：重拉时 done 的直接跳过，毒丸按 attempts 上限放弃并告警。
//   - recentIDs 退化为「已成功处理」的 5 分钟快路径，绝不前置标记。

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// processedWindow 是「已处理」内存快路径的滑窗长度。持久真相在 ingest_ledger，
// 这里只为省掉同批次/短时间内的重复数据库查询。
const processedWindow = 5 * time.Minute

// messageKey 给一条入站消息算稳定去重键：优先用服务端 message_id，缺失时退回
// 「发送者 + 创建毫秒」组合（实测拆分发送的长记录里 message_id 存在，此分支是兜底）。
func messageKey(msg *ILinkMessage) string {
	if msg == nil {
		return ""
	}
	if id := msg.MessageID.String(); id != "" && id != "0" {
		return id
	}
	if msg.FromUserID == "" {
		return ""
	}
	return fmt.Sprintf("%s-%d", msg.FromUserID, msg.CreateTimeMs)
}

// syncBufPath 是长轮询游标的落盘路径（与凭据同目录，独立文件避免改动凭据结构）。
func (c *ILinkClient) syncBufPath() string {
	return filepath.Join(filepath.Dir(c.tokenPath), "ilink_syncbuf.json")
}

// LoadCursor 读回上次已提交的游标。文件不存在是首次启动的正常情况。
// 启动时调用它，才能既不漏消息、也不把历史重放一遍。
func (c *ILinkClient) LoadCursor() {
	data, err := os.ReadFile(c.syncBufPath())
	if err != nil {
		return
	}
	var v struct {
		Cursor string `json:"cursor"`
	}
	if json.Unmarshal(data, &v) != nil || v.Cursor == "" {
		return
	}
	c.mu.Lock()
	c.cursor = v.Cursor
	c.mu.Unlock()
}

// CommitCursor 把待提交游标提交为已提交游标并落盘。只应在整批消息处理成功后调用。
// 没有待提交游标时是安全的空操作（返回 nil），便于调用方无脑跟一行。
func (c *ILinkClient) CommitCursor() error {
	c.mu.Lock()
	next := c.pendingBuf
	c.mu.Unlock()
	if next == "" {
		return nil
	}
	data, err := json.Marshal(map[string]string{"cursor": next})
	if err != nil {
		return err
	}
	// 落盘失败也推进内存游标：否则同一批消息会被无限重拉；文件写失败通常是磁盘/权限
	// 问题，已另有告警通道，不该让收取停摆。
	if err := writeFileAtomic(c.syncBufPath(), data); err != nil {
		c.mu.Lock()
		c.cursor, c.pendingBuf = next, ""
		c.mu.Unlock()
		return fmt.Errorf("游标落盘失败（内存游标已推进）: %w", err)
	}
	c.mu.Lock()
	c.cursor, c.pendingBuf = next, ""
	c.mu.Unlock()
	return nil
}

// PendingCursor 返回当前待提交游标（测试与诊断用）。
func (c *ILinkClient) PendingCursor() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pendingBuf
}

// CommittedCursor 返回已提交游标（测试与诊断用）。
func (c *ILinkClient) CommittedCursor() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cursor
}

// ResetCursor 放弃当前游标（内存 + 磁盘），下次从头拉。
// 用于整库恢复之后：恢复进来的会话与本地游标不属于同一会话（投产前审计 F2）。
// 不会丢数据：重放的消息由 ingest_ledger 去重，内容层还有 msg_hash 兜底。
func (c *ILinkClient) ResetCursor() {
	c.mu.Lock()
	c.cursor, c.pendingBuf = "", ""
	c.mu.Unlock()
	if err := os.Remove(c.syncBufPath()); err != nil && !os.IsNotExist(err) {
		slog.Warn("清除游标文件失败", "err", err)
	}
}

// MarkProcessed 把消息记入「已处理」滑窗。只在处理成功后调用。
func (c *ILinkClient) MarkProcessed(key string) {
	if key == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	if c.recentIDs == nil {
		c.recentIDs = make(map[string]time.Time)
	}
	c.recentIDs[key] = now
	// 顺手清理过期项，避免这张表随运行时长无限增长
	for id, t := range c.recentIDs {
		if now.Sub(t) > processedWindow {
			delete(c.recentIDs, id)
		}
	}
	c.mu.Unlock()
}

// AlreadyProcessed 判断消息是否在滑窗内已被成功处理过。
func (c *ILinkClient) AlreadyProcessed(key string) bool {
	if key == "" {
		return false
	}
	c.mu.RLock()
	t, seen := c.recentIDs[key]
	c.mu.RUnlock()
	return seen && time.Since(t) < processedWindow
}

// AllowProbe 清掉「会话过期」粘性位，让主循环还能再发一次探测请求。
//
// 这是 C1（重登死端）的根因所在：旧代码一旦 sessionExpired 置位就永远 continue、
// 再不调用 GetUpdates，于是提示用户发的「重登」消息根本收不到，机器人静默死亡。
// 清位后若会话确实仍无效，服务端会再次返回 -14 并重新置位，不会造成额外负担。
func (c *ILinkClient) AllowProbe() {
	c.mu.Lock()
	c.sessionExpired = false
	c.mu.Unlock()
}

// credStash 是重绑前对会话文件的暂存（投产前审计 N2）。
//
// 为什么必须能回滚：-14 并不永远是「令牌真的死了」，也可能是服务端瞬时抛错；
// 旧实现 runRebind 一进来就 ClearCredentials，用户“点了重绑但没扫成”就把一个
// 可能仍然可用的会话彻底毁掉，只能重新扫码。现在先暂存，失败/放弃时原样恢复。
type credStash struct {
	files map[string][]byte // 原文件内容（不存在的项不收录）
}

// stashCredentials 读入会话相关文件的内容后删除原文件并清空内存会话（等同 ClearCredentials）。
// 返回的 stash 可用于 restore；成功扫码后应调 discard 释放。
func (c *ILinkClient) stashCredentials() (*credStash, error) {
	st := &credStash{files: map[string][]byte{}}
	var firstErr error
	for _, p := range []string{c.tokenPath, c.contextTokenPath(), c.syncBufPath()} {
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("暂存 %s 失败: %w", filepath.Base(p), err)
			}
			continue
		}
		st.files[p] = data
	}
	for p := range st.files {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("删除 %s 失败: %w", filepath.Base(p), err)
		}
	}
	c.mu.Lock()
	c.botToken, c.botID, c.userID = "", "", ""
	c.cursor, c.pendingBuf = "", ""
	c.sessionExpired = false
	c.recentIDs = make(map[string]time.Time)
	c.contextTokens = make(map[string]string)
	c.mu.Unlock()
	return st, firstErr
}

// restore 把暂存的会话文件与内存字段装回去（扫码失败/放弃时调用）。
// 若会话确实已过期，下一轮 GetUpdates 会再次收到 -14 并重新置位，不会错过真实状态。
func (st *credStash) restore(c *ILinkClient) error {
	var firstErr error
	for p, data := range st.files {
		if err := writeFileAtomic(p, data); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("恢复 %s 失败: %w", filepath.Base(p), err)
		}
	}
	_ = c.LoadCredentials()
	_ = c.LoadContextTokens()
	c.LoadCursor()
	for p := range st.files {
		delete(st.files, p)
	}
	return firstErr
}

// discard 确认新会话已建立后释放暂存（仅内存引用，不落盘）。
func (st *credStash) discard() { st.files = nil }

var rebindClearMu sync.Mutex

// ClearCredentials 删除磁盘上的凭据、context_token 缓存与游标文件，并清空内存会话。
//
// 为什么必须真删文件：启动逻辑是「凭据存在 → IsLoggedIn 为真 → 跳过扫码」，
// 只清内存标记的话，重启后依然带着过期 token 去请求，再次 -14，形成死循环。
// 新会话的游标也不能沿用旧会话的，故一并删除。
func (c *ILinkClient) ClearCredentials() error {
	rebindClearMu.Lock()
	defer rebindClearMu.Unlock()

	var firstErr error
	for _, p := range []string{c.tokenPath, c.contextTokenPath(), c.syncBufPath()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("删除 %s 失败: %w", filepath.Base(p), err)
		}
	}
	c.mu.Lock()
	c.botToken, c.botID, c.userID = "", "", ""
	c.cursor, c.pendingBuf = "", ""
	c.sessionExpired = false
	c.recentIDs = make(map[string]time.Time)
	c.contextTokens = make(map[string]string)
	c.mu.Unlock()
	return firstErr
}
