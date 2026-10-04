package main

import (
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// AssistantSMTP 邮件发送配置。
// Port=465（或 SSL=true）走隐式 TLS；587/25 走 STARTTLS（服务端支持时自动升级）。
type AssistantSMTP struct {
	Host string   `json:"host"`
	Port int      `json:"port"`
	SSL  bool     `json:"ssl"`
	User string   `json:"user"`
	Pass string   `json:"pass"`
	From string   `json:"from"`
	To   []string `json:"to"`
}

// Ready 判断 SMTP 配置是否完整可用（密码允许为空：本地中继/免认证邮箱网关）
func (s AssistantSMTP) Ready() bool {
	return strings.TrimSpace(s.Host) != "" && s.Port > 0 &&
		strings.TrimSpace(s.From) != "" && len(recipients(s.To)) > 0
}

// recipients 清洗收件人列表（去空白、去空项）
func recipients(to []string) []string {
	out := make([]string, 0, len(to))
	for _, t := range to {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// SMTP 超时常量。
//
// 关系助手调度器是同步调用发信的（assistant.go 的 ticker 里直接 checkAssistantSchedule），
// 一旦 SMTP 服务器变成黑洞（SYN 不回、或连上后不吐 greeting），没有超时就会
// 把整个调度 goroutine 永久挂住，日报/周报/冷却提醒全部停摆且没有任何报错。
const (
	smtpDialTimeout = 15 * time.Second // 建连（含 TLS 握手）超时
	smtpIOTimeout   = 60 * time.Second // 整轮 SMTP 会话的读写截止时间
)

// headerSafe 去掉邮件头里的 CR/LF。
// 收件人和发件人来自网页端配置，带换行就能注入任意头（Bcc、伪造 From），
// 邮件头里出现裸换行本身就是非法的，直接压成空格即可。
func headerSafe(s string) string {
	r := strings.NewReplacer("\r", " ", "\n", " ")
	return r.Replace(s)
}

// sendAssistantMail 发送一封 HTML 邮件。不引入第三方库，仅用 net/smtp + crypto/tls。
func sendAssistantMail(s AssistantSMTP, subject, htmlBody string) error {
	if !s.Ready() {
		return fmt.Errorf("SMTP 配置不完整（需要 host/port/from/to）")
	}
	to := recipients(s.To)
	addr := net.JoinHostPort(s.Host, fmt.Sprintf("%d", s.Port))

	// RFC2047 编码主题，中文标题才不会在收件箱里变乱码
	msg := &strings.Builder{}
	msg.WriteString("From: " + headerSafe(s.From) + "\r\n")
	msg.WriteString("To: " + headerSafe(strings.Join(to, ", ")) + "\r\n")
	msg.WriteString("Subject: " + mime.QEncoding.Encode("UTF-8", subject) + "\r\n")
	msg.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(htmlBody)

	var auth smtp.Auth
	if strings.TrimSpace(s.User) != "" {
		auth = smtp.PlainAuth("", s.User, s.Pass, s.Host)
	}

	if s.SSL || s.Port == 465 {
		return sendMailImplicitTLS(addr, s.Host, s.From, to, auth, []byte(msg.String()))
	}
	// 不用 smtp.SendMail：它内部 net.Dial 没有超时，也没法给连接设 deadline
	return sendMailStartTLS(addr, s.Host, s.From, to, auth, []byte(msg.String()))
}

// sendMailImplicitTLS 隐式 TLS（465 端口）：先建 TLS 连接再走 SMTP 协议。
// smtp.SendMail 只会 STARTTLS，不支持这种模式，必须手动实现。
func sendMailImplicitTLS(addr, host, from string, to []string, auth smtp.Auth, body []byte) error {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: smtpDialTimeout}, "tcp", addr,
		&tls.Config{ServerName: host})
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(smtpIOTimeout)); err != nil {
		conn.Close()
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	return deliverMail(c, host, from, to, auth, body)
}

// sendMailStartTLS 明文建连后按服务端能力自动升级 STARTTLS。
func sendMailStartTLS(addr, host, from string, to []string, auth smtp.Auth, body []byte) error {
	conn, err := net.DialTimeout("tcp", addr, smtpDialTimeout)
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(smtpIOTimeout)); err != nil {
		conn.Close()
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("STARTTLS 升级失败: %w", err)
		}
	}
	return deliverMail(c, host, from, to, auth, body)
}

// deliverMail 在已建好的 SMTP 客户端上完成认证与投递（两种加密模式共用）。
func deliverMail(c *smtp.Client, host, from string, to []string, auth smtp.Auth, body []byte) error {
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return fmt.Errorf("SMTP 服务器不支持认证")
		}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, t := range to {
		if err := c.Rcpt(t); err != nil {
			return fmt.Errorf("收件人 %s 被拒绝: %w", t, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return w.Close()
}
