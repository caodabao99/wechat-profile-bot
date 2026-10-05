package main

// 备份加密安全测试：口令派生 + AES-256-GCM 加解密（全功能验证补漏，此前 backup_test.go 无一测试）。
// 验证的是「数据安全性」核心属性：能用对口令解回原样、错口令/篡改/截断一律拒绝、
// 每次加密因随机盐+nonce 而密文不同，并用一份独立实现的 PBKDF2 交叉校验 deriveBackupKey 的正确性。

import (
	"archive/zip"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testPBKDF2 是 PBKDF2-HMAC-SHA256 的一份「教科书直译」独立实现，仅用于交叉校验生产实现。
func testPBKDF2(password string, salt []byte, iters, keyLen int) []byte {
	var out []byte
	block := make([]byte, 4)
	for blk := 1; len(out) < keyLen; blk++ {
		prf := hmac.New(sha256.New, []byte(password))
		prf.Write(salt)
		block[0], block[1], block[2], block[3] = byte(blk>>24), byte(blk>>16), byte(blk>>8), byte(blk)
		prf.Write(block)
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iters; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func TestDeriveBackupKeyMatchesIndependentPBKDF2(t *testing.T) {
	salt := []byte("0123456789abcdef")
	for _, pw := range []string{"hunter2", "", "口令密码-pâssw0rd"} {
		got := deriveBackupKey(pw, salt)
		want := testPBKDF2(pw, salt, backupEncIterations, backupEncKeyLen)
		if !bytes.Equal(got, want) {
			t.Fatalf("deriveBackupKey(%q) 与独立 PBKDF2 实现不一致：生产=%x 期望=%x", pw, got, want)
		}
	}
	// 长度、确定性、盐/口令敏感性
	k1 := deriveBackupKey("pw", salt)
	if len(k1) != 32 {
		t.Fatalf("密钥应为 32 字节(AES-256)，得 %d", len(k1))
	}
	if !bytes.Equal(k1, deriveBackupKey("pw", salt)) {
		t.Fatal("同口令同盐应确定性")
	}
	if bytes.Equal(k1, deriveBackupKey("pw", []byte("xxxxxxxxxxxxxxxx"))) {
		t.Fatal("不同盐应派生出不同密钥")
	}
	if bytes.Equal(k1, deriveBackupKey("pw2", salt)) {
		t.Fatal("不同口令应派生出不同密钥")
	}
}

func TestEncryptDecryptBackupBlobRoundtrip(t *testing.T) {
	plain := []byte("微信助手画像备份：{敏感数据 secret payload}")
	blob, err := encryptBackupBlob(plain, "correct horse")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	// 密文不应等于明文，且以魔数开头
	if bytes.Contains(blob, plain) {
		t.Fatal("密文里不应出现明文片段")
	}
	if string(blob[:len(backupEncMagic)]) != backupEncMagic {
		t.Fatalf("密文应以魔数 %q 开头，得 %q", backupEncMagic, blob[:4])
	}
	out, err := decryptBackupBlob(blob, "correct horse")
	if err != nil {
		t.Fatalf("正确口令解密失败: %v", err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("解回与原样不符: %q", out)
	}
	// 错口令必须失败，且错误信息不泄露内部细节
	if _, err := decryptBackupBlob(blob, "wrong horse"); err == nil {
		t.Fatal("错口令应解密失败")
	} else if bytes.Contains([]byte(err.Error()), []byte("nonce")) || bytes.Contains([]byte(err.Error()), []byte("GCM")) {
		t.Fatalf("错误信息不应暴露内部细节: %v", err)
	}
}

func TestEncryptBackupBlobNondeterministic(t *testing.T) {
	plain := []byte("same input")
	a, _ := encryptBackupBlob(plain, "pw")
	b, _ := encryptBackupBlob(plain, "pw")
	if bytes.Equal(a, b) {
		t.Fatal("相同明文+口令两次加密应因随机盐/nonce 而密文不同")
	}
	// 但都能解回
	for _, blob := range [][]byte{a, b} {
		if o, err := decryptBackupBlob(blob, "pw"); err != nil || !bytes.Equal(o, plain) {
			t.Fatalf("两个密文都应能解回: out=%q err=%v", o, err)
		}
	}
}

func TestDecryptBackupBlobRejectsTamperAndTruncate(t *testing.T) {
	plain := []byte("integrity matters")
	blob, err := encryptBackupBlob(plain, "pw")
	if err != nil {
		t.Fatal(err)
	}
	head := len(backupEncMagic) + backupEncSaltLen + backupEncNonceLen
	// 翻转密文区一个 bit → GCM 认证标签校验应失败
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := decryptBackupBlob(tampered, "pw"); err == nil {
		t.Fatal("篡改密文应解密失败")
	}
	// 篡改 salt 区（改盐→派生密钥变化→GCM 失败）
	tamperSalt := append([]byte(nil), blob...)
	tamperSalt[len(backupEncMagic)] ^= 0xFF
	if _, err := decryptBackupBlob(tamperSalt, "pw"); err == nil {
		t.Fatal("篡改盐应解密失败")
	}
	// 截断到不足最小长度 → 拒绝
	if _, err := decryptBackupBlob(blob[:head], "pw"); err == nil {
		t.Fatal("截断到无密文体应拒绝")
	}
	if _, err := decryptBackupBlob(blob[:head-1], "pw"); err == nil {
		t.Fatal("长度不足头部应拒绝")
	}
	// 魔数错误 → 拒绝
	badMagic := append([]byte(nil), blob...)
	badMagic[0] = 'X'
	if _, err := decryptBackupBlob(badMagic, "pw"); err == nil {
		t.Fatal("魔数不符应拒绝")
	}
}

func TestEncryptBackupBlobEmptyPlaintext(t *testing.T) {
	// 空明文也应能加解密往返（GCM 允许 0 长密文，仅 16 字节标签）
	blob, err := encryptBackupBlob(nil, "pw")
	if err != nil {
		t.Fatalf("加密空明文失败: %v", err)
	}
	out, err := decryptBackupBlob(blob, "pw")
	if err != nil {
		t.Fatalf("解密空明文失败: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("空明文应解回空，得 %q", out)
	}
}

func TestBackupFileNameFormat(t *testing.T) {
	ts := time.Date(2026, 3, 9, 14, 5, 6, 0, time.Local)
	got := BackupFileName(ts)
	want := "wechat-profile-backup-20260309-140506.zip"
	if got != want {
		t.Fatalf("BackupFileName=%q 期望=%q", got, want)
	}
}

func TestBackupNeedsPassword(t *testing.T) {
	dir := t.TempDir()

	writeZip := func(name string, entries []string) string {
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		zw := zip.NewWriter(f)
		for _, e := range entries {
			w, err := zw.Create(e)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte("x"))
		}
		_ = zw.Close()
		_ = f.Close()
		return path
	}

	enc := writeZip("with.enc.zip", []string{"data.sql", "config.json" + backupEncSuffix})
	if !BackupNeedsPassword(enc) {
		t.Fatal("含 .enc 文件应判定需要口令")
	}
	plainZip := writeZip("plain.zip", []string{"data.sql", "config.json"})
	if BackupNeedsPassword(plainZip) {
		t.Fatal("不含 .enc 文件不应需要口令")
	}
	// 不存在/非法 zip → 安全返回 false（不 panic）
	if BackupNeedsPassword(filepath.Join(dir, "nope.zip")) {
		t.Fatal("不存在的文件应返回 false")
	}
}
