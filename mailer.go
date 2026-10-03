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

// sendAssistantMail 发送一封 HTML 邮件。不引入第三方库，仅用 net/smtp + crypto/tls。
func sendAssistantMail(s AssistantSMTP, subject, htmlBody string) error {
	if !s.Ready() {
		return fmt.Errorf("SMTP 配置不完整（需要 host/port/from/to）")
	}
	to := recipients(s.To)
	addr := net.JoinHostPort(s.Host, fmt.Sprintf("%d", s.Port))

	// RFC2047 编码主题，中文标题才不会在收件箱里变乱码
	msg := &strings.Builder{}
	msg.WriteString("From: " + s.From + "\r\n")
	msg.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
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
	// smtp.SendMail 在服务端宣告 STARTTLS 时自动升级加密
	return smtp.SendMail(addr, auth, s.From, to, []byte(msg.String()))
}

// sendMailImplicitTLS 隐式 TLS（465 端口）：先建 TLS 连接再走 SMTP 协议。
// smtp.SendMail 只会 STARTTLS，不支持这种模式，必须手动实现。
func sendMailImplicitTLS(addr, host, from string, to []string, auth smtp.Auth, body []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
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
