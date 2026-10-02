package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/skip2/go-qrcode"
)

// generateASCIQR 从 iLink 返回的二维码链接中提取 payload 并生成 ASCII 二维码
func generateASCIQR(qrcodeURL string) (string, error) {
	payload := extractQRPayload(qrcodeURL)
	// Low 纠错级别：payload 是短 URL，Low 的模块数更少、码更小，
	// 终端近距离扫码不需要高纠错冗余
	qr, err := qrcode.New(payload, qrcode.Low)
	if err != nil {
		return "", err
	}
	// ToString(true) 用全块字符 ▄▀█，每行一个模块行，全宽显示。
	// 以前 ToSmallString(半块字符) 在部分终端里会错位导致无法识别。
	return qr.ToString(true), nil
}

// extractQRPayload 从 iLink 返回的二维码链接里提取二维码的真正 payload。
//
// 当前接口（GET /ilink/bot/get_bot_qrcode）返回的 qrcode_img_content 形如
//
//	https://liteapp.weixin.qq.com/q/7GiQu1?qrcode=<hex>&bot_type=3
//
// 整条 URL 就是要编码进二维码的内容，没有 data 参数，所以直接原样返回。
// 保留 data 分支是为了兼容「服务端返回第三方二维码图片服务链接」这种历史形态
// （例如 https://api.qrserver.com/...?data=<真正的payload>），此时需要取出 data 的值。
func extractQRPayload(qrcodeURL string) string {
	u, err := url.Parse(qrcodeURL)
	if err != nil {
		return qrcodeURL
	}
	q := u.Query()
	payload := q.Get("data")
	if payload != "" {
		return payload
	}
	return qrcodeURL
}

// printQRCode 在终端打印 ASCII 二维码
func printQRCode(qrcodeURL string) {
	ascii, err := generateASCIQR(qrcodeURL)
	if err != nil {
		slog.Error("生成二维码失败", "err", err)
		return
	}
	// go-qrcode 的 ToString(true) 已经生成了黑白块组成的 ASCII 艺术
	// 每行前面缩进一点，使其在终端更居中
	lines := strings.Split(ascii, "\n")
	for _, line := range lines {
		fmt.Println("  " + line)
	}
}
