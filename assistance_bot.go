package main

import (
	"context"
	"strings"
)

var assistBotLimiter = newRateLimiter(ingestRateLimit, ingestRateWindow)

func (b *Bot) assistCommand(cmd, args string) string {
	n := 2
	if cmd == "改写" {
		n = 3
	}
	if cmd == "画像变化" {
		n = 1
	}
	parts := strings.SplitN(args, "|", n)
	if len(parts) != n || strings.TrimSpace(parts[0]) == "" {
		return "用法：改写 昵称 | 更自然 | 原回复\n草稿检查 昵称 | 准备发送的话\n画像变化 昵称"
	}
	id, _, err := FindContactID(b.db, strings.TrimSpace(parts[0]))
	if err != nil {
		return "未找到联系人"
	}
	if cmd == "画像变化" {
		out, err := GetProfileChanges(b.db, id)
		if err != nil {
			return err.Error()
		}
		return formatProfileChanges(out)
	}
	text := strings.TrimSpace(parts[n-1])
	style := ""
	if cmd == "改写" {
		style = strings.TrimSpace(parts[1])
	}
	if err := validateAssist(text, style, cmd == "改写"); err != nil {
		return err.Error()
	}
	if ok, _ := assistBotLimiter.Allow("reply-assistance"); !ok {
		return "请求过于频繁，请稍后重试"
	}
	ctx, cancel := context.WithTimeout(context.Background(), botTaskTimeout)
	defer cancel()
	if cmd == "改写" {
		out, err := RewriteReply(ctx, b.db, b.llm, id, text, style)
		if err != nil {
			return "改写失败，原回复未改变：" + err.Error()
		}
		return "改写版本：\n" + out + "\n\n仅供查看和复制，请自行决定是否发送。"
	}
	out, err := ReviewDraft(ctx, b.db, b.llm, id, text)
	if err != nil {
		return "检查失败，原草稿未改变：" + err.Error()
	}
	return formatDraftReview(out)
}
