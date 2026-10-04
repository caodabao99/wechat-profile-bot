package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 运行模式一键切换预设（特性③）。
//
// 预设 = 一组命名参数束，一键写入既有设置表：assistant_settings（提醒/巡检/情绪/待跟进/
// 关系趋势阈值）+ archive_settings（归档开关与保留天数）。切换预设即切换整套运行行为。
//
// 回复语气/推演人设不作为预设维度：现有改写/推演提示词固定产出「稳妥/简洁/亲切/委婉」四种风格
// 且顺序锁定，改动会牵动多处以精确结构为准的提示词与回归测试，属于半吊子改造，明确不做。
//
// mode_presets 是增值表：备份/恢复走动态全表清单，自动被纳入，无需在核心表清单登记。

// ModePresetPayload 一个预设覆盖的运行参数快照。
type ModePresetPayload struct {
	Assistant AssistantSettings `json:"assistant"`
	Archive   ArchiveSettings   `json:"archive"`
}

// ModePreset 对外展示的预设（内置或用户自定义）。
type ModePreset struct {
	Name      string            `json:"name"`
	Builtin   bool              `json:"builtin"`
	UpdatedAt string            `json:"updatedAt"`
	Payload   ModePresetPayload `json:"payload"`
}

// ensureModePresetTables 建预设表（幂等）。
func ensureModePresetTables(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS mode_presets (
		name TEXT PRIMARY KEY,
		payload_json TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

// builtinPayload 以默认助手/归档设置为底，套用一组覆盖项，派生一个预设 payload。
func builtinPayload(aOver func(*AssistantSettings), archOver func(*ArchiveSettings)) ModePresetPayload {
	a := defaultAssistantSettings()
	arch := ArchiveSettings{Enabled: false, RetentionDays: defaultArchiveRetentionDays}
	if aOver != nil {
		aOver(&a)
	}
	if archOver != nil {
		archOver(&arch)
	}
	a.normalize()
	arch = normalizeArchiveSettings(arch)
	return ModePresetPayload{Assistant: a, Archive: arch}
}

// builtinModePresets 内置运行模式预设。
func builtinModePresets() []ModePreset {
	defs := []struct {
		name     string
		aOver    func(*AssistantSettings)
		archOver func(*ArchiveSettings)
	}{
		{
			// 家人：常联系、多提醒，趋势判定宽松（不易误报“沉寂/降温”）。
			name: "家人",
			aOver: func(a *AssistantSettings) {
				a.Enabled = true
				a.RemindBirthday, a.RemindCooling, a.RemindFollowup = true, true, true
				a.EmotionAlert = true
				a.BirthdayAdvanceDays = 7
				a.SilenceDays, a.CoolingMinPrior, a.WarmingMinPrior = 45, 8, 5
			},
			archOver: func(ar *ArchiveSettings) { ar.Enabled = false; ar.RetentionDays = 365 },
		},
		{
			// 好友：均衡默认。
			name: "好友",
			aOver: func(a *AssistantSettings) {
				a.Enabled = true
				a.RemindBirthday, a.RemindCooling, a.RemindFollowup = true, true, true
				a.EmotionAlert = true
			},
			archOver: func(ar *ArchiveSettings) { ar.Enabled = false; ar.RetentionDays = 365 },
		},
		{
			// 同事：以工作待跟进为主，弱化情绪/生日打扰，趋势判定收紧一点。
			name: "同事",
			aOver: func(a *AssistantSettings) {
				a.Enabled = true
				a.RemindBirthday, a.EmotionAlert = false, false
				a.RemindCooling, a.RemindFollowup = true, true
				a.FollowupEnabled = true
				a.SilenceDays, a.CoolingMinPrior, a.WarmingMinPrior = 21, 6, 4
			},
			archOver: func(ar *ArchiveSettings) { ar.Enabled = true; ar.RetentionDays = 180 },
		},
		{
			// 客户：高灵敏（早降温/早沉默/自动抽待跟进），归档更激进节省主库。
			name: "客户",
			aOver: func(a *AssistantSettings) {
				a.Enabled = true
				a.RemindBirthday, a.RemindCooling, a.RemindFollowup = true, true, true
				a.EmotionAlert, a.FollowupEnabled = true, true
				a.BirthdayAdvanceDays = 5
				a.SilenceDays, a.CoolingMinPrior, a.WarmingMinPrior = 14, 4, 3
			},
			archOver: func(ar *ArchiveSettings) { ar.Enabled = true; ar.RetentionDays = 90 },
		},
		{
			// 静音专注：关掉一切主动提醒/推送，只入库，归档积极。
			name: "静音专注",
			aOver: func(a *AssistantSettings) {
				a.Enabled = false
				a.RemindBirthday, a.RemindCooling, a.RemindFollowup = false, false, false
				a.EmotionAlert, a.FollowupEnabled, a.BlessingDraft = false, false, false
			},
			archOver: func(ar *ArchiveSettings) { ar.Enabled = true; ar.RetentionDays = 730 },
		},
	}
	out := make([]ModePreset, 0, len(defs))
	for _, d := range defs {
		out = append(out, ModePreset{
			Name:    d.name,
			Builtin: true,
			Payload: builtinPayload(d.aOver, d.archOver),
		})
	}
	return out
}

func isBuiltinPresetName(name string) bool {
	for _, p := range builtinModePresets() {
		if p.Name == name {
			return true
		}
	}
	return false
}

// ListModePresets 返回内置 + 用户自定义预设（同名时自定义优先展示，内置始终在列）。
func ListModePresets(db *sql.DB) ([]ModePreset, error) {
	list := builtinModePresets()
	rows, err := db.Query(`SELECT name, payload_json, COALESCE(updated_at,'') FROM mode_presets ORDER BY name`)
	if err != nil {
		return list, nil // 表不存在等：只回内置，不阻断
	}
	defer rows.Close()
	for rows.Next() {
		var name, raw, upd string
		if rows.Scan(&name, &raw, &upd) != nil {
			continue
		}
		var p ModePresetPayload
		if json.Unmarshal([]byte(raw), &p) != nil {
			continue
		}
		list = append(list, ModePreset{Name: name, Builtin: false, UpdatedAt: upd, Payload: p})
	}
	return list, rows.Err()
}

// resolveModePreset 按名字找预设（内置优先，其次用户自定义）。
func resolveModePreset(db *sql.DB, name string) (ModePresetPayload, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ModePresetPayload{}, false, errors.New("预设名称不能为空")
	}
	for _, p := range builtinModePresets() {
		if p.Name == name {
			return p.Payload, true, nil
		}
	}
	var raw string
	err := db.QueryRow(`SELECT payload_json FROM mode_presets WHERE name=?`, name).Scan(&raw)
	if err == sql.ErrNoRows {
		return ModePresetPayload{}, false, nil
	}
	if err != nil {
		return ModePresetPayload{}, false, err
	}
	var p ModePresetPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ModePresetPayload{}, false, fmt.Errorf("预设数据损坏: %w", err)
	}
	return p, true, nil
}

// ApplyModePreset 一键把某预设写入既有设置表。找不到预设返回错误。
// 助手设置与归档设置必须在同一事务、同一次 dbMu 持有内原子写入：
// 否则第一步成功、第二步失败时，助手侧已被切换、归档侧未变，却对外报失败（半更新）。
func ApplyModePreset(db *sql.DB, name string) (ModePresetPayload, error) {
	// resolveModePreset 与 loadAssistantSettings 都会自行加/解 dbMu，必须在下面持锁之前完成，避免自锁。
	payload, found, err := resolveModePreset(db, name)
	if err != nil {
		return ModePresetPayload{}, err
	}
	if !found {
		return ModePresetPayload{}, fmt.Errorf("预设不存在: %s", strings.TrimSpace(name))
	}
	// 运行模式预设只管理行为开关/阈值，不管理凭据：应用前把用户现有的 SMTP 凭据与
	// 日历订阅密钥保留下来。否则内置预设（以空 SMTP 为底）一应用就会把配好的邮箱密码/密钥冲没。
	cur, err := loadAssistantSettings(db)
	if err != nil {
		return ModePresetPayload{}, fmt.Errorf("读取当前助手设置失败: %w", err)
	}
	payload.Assistant.SMTP = cur.SMTP
	payload.Assistant.CalendarKey = cur.CalendarKey

	dbMu.Lock()
	defer dbMu.Unlock()
	tx, err := db.Begin()
	if err != nil {
		return ModePresetPayload{}, fmt.Errorf("开启应用事务失败: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := writeAssistantSettings(tx, payload.Assistant); err != nil {
		return ModePresetPayload{}, fmt.Errorf("应用助手设置失败: %w", err)
	}
	if err := writeArchiveSettings(tx, payload.Archive); err != nil {
		return ModePresetPayload{}, fmt.Errorf("应用归档设置失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ModePresetPayload{}, fmt.Errorf("提交应用事务失败: %w", err)
	}
	committed = true
	return payload, nil
}

// currentModePresetPayload 抓取当前 assistant + archive 设置，作为「另存为预设」的来源。
func currentModePresetPayload(db *sql.DB) (ModePresetPayload, error) {
	a, err := loadAssistantSettings(db)
	if err != nil {
		return ModePresetPayload{}, err
	}
	ar, err := loadArchiveSettings(db)
	if err != nil {
		return ModePresetPayload{}, err
	}
	return ModePresetPayload{Assistant: a, Archive: ar}, nil
}

// SaveModePreset 把当前运行设置另存为用户自定义预设（可覆盖同名自定义，但不得覆盖内置）。
func SaveModePreset(db *sql.DB, name string) (ModePresetPayload, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ModePresetPayload{}, errors.New("预设名称不能为空")
	}
	if len([]rune(name)) > 40 {
		return ModePresetPayload{}, errors.New("预设名称过长（最多 40 字）")
	}
	if isBuiltinPresetName(name) {
		return ModePresetPayload{}, errors.New("该名称为内置预设，请换一个名字")
	}
	payload, err := currentModePresetPayload(db)
	if err != nil {
		return ModePresetPayload{}, err
	}
	// 预设不存凭据：SMTP 密码与日历密钥不入预设表（避免明文凭据随预设落盘并随备份扩散）；
	// 应用预设时会从当前设置保留真实凭据（见 ApplyModePreset），故此处置空不影响恢复后的行为。
	payload.Assistant.SMTP.Pass = ""
	payload.Assistant.CalendarKey = ""
	raw, err := json.Marshal(payload)
	if err != nil {
		return ModePresetPayload{}, err
	}
	dbMu.Lock()
	_, err = db.Exec(
		`INSERT INTO mode_presets (name, payload_json, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(name) DO UPDATE SET payload_json = excluded.payload_json, updated_at = CURRENT_TIMESTAMP`,
		name, string(raw))
	dbMu.Unlock()
	if err != nil {
		return ModePresetPayload{}, err
	}
	return payload, nil
}

// DeleteModePreset 删除用户自定义预设（内置不可删）。
func DeleteModePreset(db *sql.DB, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("预设名称不能为空")
	}
	if isBuiltinPresetName(name) {
		return errors.New("内置预设不可删除")
	}
	dbMu.Lock()
	_, err := db.Exec(`DELETE FROM mode_presets WHERE name=?`, name)
	dbMu.Unlock()
	return err
}
