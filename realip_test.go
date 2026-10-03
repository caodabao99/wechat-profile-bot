package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func proxyReq(remoteAddr, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestRealClientIP(t *testing.T) {
	trusted := []string{"127.0.0.1", "10.0.0.0/8"}
	for _, tt := range []struct {
		name       string
		remoteAddr string
		xff        string
		trusted    []string
		want       string
	}{
		{"直连客户端-无XFF", "203.0.113.9:51234", "", trusted, "203.0.113.9"},
		{"直连客户端-伪造XFF必须忽略", "203.0.113.9:51234", "1.2.3.4", trusted, "203.0.113.9"},
		{"未配置可信代理-XFF一律忽略", "10.0.0.5:51234", "203.0.113.9", nil, "10.0.0.5"},
		{"单级反代", "127.0.0.1:51234", "203.0.113.9", trusted, "203.0.113.9"},
		{"单级反代-客户端伪造最左IP", "127.0.0.1:51234", "1.2.3.4, 203.0.113.9", trusted, "203.0.113.9"},
		{"两级可信代理链", "127.0.0.1:51234", "203.0.113.9, 10.0.0.7", trusted, "203.0.113.9"},
		{"可信代理但无XFF头", "127.0.0.1:51234", "", trusted, "127.0.0.1"},
		{"XFF带空格", "127.0.0.1:51234", " 203.0.113.9 , 10.0.0.7 ", trusted, "203.0.113.9"},
		{"IPv6直连", "[2001:db8::1]:51234", "", trusted, "2001:db8::1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := realClientIP(proxyReq(tt.remoteAddr, tt.xff), tt.trusted)
			if got != tt.want {
				t.Fatalf("realClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckIPWhitelistWithProxy(t *testing.T) {
	whitelist := []string{"203.0.113.9"}
	trusted := []string{"127.0.0.1"}
	for _, tt := range []struct {
		name       string
		remoteAddr string
		xff        string
		want       bool
	}{
		{"反代转发白名单IP", "127.0.0.1:51234", "203.0.113.9", true},
		{"反代转发非白名单IP", "127.0.0.1:51234", "198.51.100.7", false},
		{"直连来源伪造XFF为白名单IP-必须拒绝", "198.51.100.7:51234", "203.0.113.9", false},
		{"反代链中伪造白名单IP在最左-必须拒绝", "127.0.0.1:51234", "203.0.113.9, 198.51.100.7", false},
		{"白名单为空不限制", "8.8.8.8:1", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wl := whitelist
			if tt.name == "白名单为空不限制" {
				wl = nil
			}
			if got := checkIPWhitelist(proxyReq(tt.remoteAddr, tt.xff), wl, trusted); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
