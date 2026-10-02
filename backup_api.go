package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// backupMaxUpload 备份导入允许的最大 zip 体积
const backupMaxUpload = 200 << 20 // 200MB

// backupPasswordRequest POST /api/backup/export 的请求体
type backupPasswordRequest struct {
	Password string `json:"password"`
}

// botSidecarFiles 服务端备份随带的旁路文件：
// 模型配置、微信登录凭据、2FA 密钥——换服务器部署时免去重新扫码和重新配置。
// web_sessions.json（网页会话）刻意不备份，新机要求重新登录。
var botSidecarFiles = []string{"config.json", "ilink_credentials.json", "totp_secret.json"}

// backupSource 判断请求来自网页端还是桌面端：
// 网页端使用会话令牌（session token），桌面端使用 apiToken。
// UA 辅助判断兜底（如移动端浏览器直接粘贴 URL）。
func backupSource(r *http.Request, sessions *webSessionStore) string {
	tok := bearerToken(r)
	if sessions != nil && sessions.valid(tok) {
		return "web"
	}
	if ua := r.UserAgent(); strings.Contains(ua, "Mozilla") || strings.Contains(ua, "Chrome") || strings.Contains(ua, "Safari") {
		return "web"
	}
	return "desktop"
}

// hBackupExport 导出备份 zip。
//
// GET  —— 明文备份（口令放查询串会进浏览器历史、访问日志和 Referer，故不支持）
// POST —— 请求体 {"password":"..."}，非空时把密钥文件加密后再打包
func (s *apiServer) hBackupExport(w http.ResponseWriter, r *http.Request) {
	var password string
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var req backupPasswordRequest
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeErr(w, http.StatusBadRequest, "请求体不是有效的 JSON")
			return
		}
		password = strings.TrimSpace(req.Password)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET 或 POST")
		return
	}

	zipPath, cleanup, err := BuildBackupZipWithPassword(s.db, dataDir(), botSidecarFiles, password)
	if err != nil {
		slog.Error("导出备份失败", "err", err)
		LogBackupAction(s.db, "export", backupSource(r, s.sessions), "", 0, err.Error(), false)
		writeErr(w, http.StatusInternalServerError, "导出备份失败: "+err.Error())
		return
	}
	defer cleanup()

	name := BackupFileName(time.Now())
	detail := ""
	if password != "" {
		detail = "密钥文件已加密"
	}
	if fi, err := os.Stat(zipPath); err == nil {
		LogBackupAction(s.db, "export", backupSource(r, s.sessions), name, fi.Size(), detail, true)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, name, url.PathEscape(name)))
	// ServeFile 支持断点续传并自动处理 304
	w.Header().Del("Last-Modified")
	http.ServeFile(w, r, zipPath)
}

// hBackupImport 导入备份 zip（multipart 字段名 file，可选字段 password）。
// 聊天数据在事务内即时生效；config/凭据/2FA 密钥写入磁盘后需重启进程。
func (s *apiServer) hBackupImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, backupMaxUpload)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "备份文件过大（上限 200MB）或不是有效的上传请求")
		return
	}
	up, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少上传文件（multipart 字段名应为 file）")
		return
	}
	defer up.Close()
	password := strings.TrimSpace(r.FormValue("password"))

	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	tmp, err := os.CreateTemp("", "wp-upload-*"+ext)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建临时文件失败")
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, http.MaxBytesReader(w, up, backupMaxUpload)); err != nil {
		tmp.Close()
		writeErr(w, http.StatusRequestEntityTooLarge, "备份文件过大（上限 200MB）")
		return
	}
	tmp.Close()

	summary, err := restoreBotBackup(s.db, tmpPath, dataDir(), true, password)
	if err != nil {
		slog.Warn("导入备份失败", "err", err)
		LogBackupAction(s.db, "import", backupSource(r, s.sessions), hdr.Filename, hdr.Size, err.Error(), false)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Info("备份导入完成",
		"contacts", summary.Contacts, "messages", summary.Messages,
		"histories", summary.Histories, "files", summary.Files,
		"encrypted", password != "",
		"needRestart", summary.NeedRestart, "safety", summary.SafetyBackup)
	detail := fmt.Sprintf("联系人 %d，消息 %d，画像历史 %d",
		summary.Contacts, summary.Messages, summary.Histories)
	if len(summary.Files) > 0 {
		detail += "；恢复配置文件 " + strings.Join(summary.Files, "、")
		if password != "" {
			detail += "（密钥文件已解密）"
		}
	}
	LogBackupAction(s.db, "import", backupSource(r, s.sessions), hdr.Filename, hdr.Size, detail, true)
	writeJSON(w, http.StatusOK, summary)
}

// restoreBotBackup 的锁顺序为 deleteRestoreMu → totpMu → 共享恢复内部的 dbMu。
func restoreBotBackup(db *sql.DB, zipPath, dir string, makeSafety bool, password string) (*BackupSummary, error) {
	deleteRestoreMu.Lock()
	defer deleteRestoreMu.Unlock()
	totpMu.Lock()
	defer totpMu.Unlock()
	summary, err := RestoreBackupZipWithPassword(db, zipPath, dir, makeSafety, password)
	if err == nil {
		pendingDeleteMu.Lock()
		clear(pendingDeletes)
		pendingDeleteMu.Unlock()
	}
	return summary, err
}

// hBackupLogs 查询备份/恢复历史
func (s *apiServer) hBackupLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	logs, err := ListBackupLogs(s.db, 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, logs)
}
