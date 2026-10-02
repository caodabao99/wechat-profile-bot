package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func regressionDB(t *testing.T) *sql.DB {
	t.Helper()
	if config == nil {
		config = &Config{}
	}
	db, err := InitDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func regressionContact(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	id, err := GetOrCreateContact(db, name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func regressionMessages(t *testing.T, db *sql.DB, id int64, texts ...string) {
	t.Helper()
	for _, text := range texts {
		_, err := SaveMessages(db, id, []Message{{Sender: "other", Content: text, Timestamp: time.Date(2025, 6, 10, 10, 23, 0, 0, time.Local)}})
		if err != nil {
			t.Fatal(err)
		}
	}
}
func TestMergeUndoRestoresDuplicateAndProfile(t *testing.T) {
	db := regressionDB(t)
	a, b := regressionContact(t, db, "source"), regressionContact(t, db, "target")
	regressionMessages(t, db, a, "duplicate", "source-only")
	regressionMessages(t, db, b, "duplicate")
	if err := SaveProfile(db, b, `{"summary":"before"}`, "before", "initial"); err != nil {
		t.Fatal(err)
	}
	var originalID int64
	var originalTime, originalCaptured string
	if err := db.QueryRow(`SELECT id,msg_time,captured_at FROM messages WHERE contact_id=? AND content='duplicate'`, a).Scan(&originalID, &originalTime, &originalCaptured); err != nil {
		t.Fatal(err)
	}
	merged, err := MergeContacts(db, a, b, MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if merged.MovedMessages != 1 {
		t.Fatalf("moved %d", merged.MovedMessages)
	}
	msgs, err := GetAllMessages(db, b)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages=%v err=%v", msgs, err)
	}
	if err := SaveProfile(db, b, `{"summary":"merged"}`, "merged", "generated"); err != nil {
		t.Fatal(err)
	}
	if err := UndoMerge(db, merged.MergeLogID); err != nil {
		t.Fatal(err)
	}
	var cid int64
	var mt, cap string
	if err := db.QueryRow(`SELECT contact_id,msg_time,captured_at FROM messages WHERE id=?`, originalID).Scan(&cid, &mt, &cap); err != nil {
		t.Fatal(err)
	}
	if cid != a || mt != originalTime || cap != originalCaptured {
		t.Fatalf("snapshot changed: %d %q %q", cid, mt, cap)
	}
	c, err := GetContactByID(db, b)
	if err != nil || c.ProfileSummary != "before" {
		t.Fatalf("profile=%+v err=%v", c, err)
	}
	err = GenerateOrUpdateProfile(context.Background(), db, nil, b, "target", msgs)
	if !errors.Is(err, ErrProfileStale) {
		t.Fatalf("stale prefetched messages: %v", err)
	}
}

func TestProfileInFlightInvalidated(t *testing.T) {
	for _, supplement := range []bool{false, true} {
		t.Run(fmt.Sprint(supplement), func(t *testing.T) {
			db := regressionDB(t)
			a, b := regressionContact(t, db, "source"), regressionContact(t, db, "target")
			regressionMessages(t, db, a, "source")
			merged, err := MergeContacts(db, a, b, MergeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := GetAllMessages(db, b)
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(entered); <-release })
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"summary\":\"stale\"}"}}]}`)
			}))
			defer server.Close()
			cfg := &Config{}
			cfg.LLM.BaseURL = server.URL
			client := NewLLMClient(cfg)
			done := make(chan error, 1)
			go func() {
				if supplement {
					done <- SupplementProfile(context.Background(), db, client, b, "target", "note")
				} else {
					done <- GenerateOrUpdateProfile(context.Background(), db, client, b, "target", msgs)
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("LLM not entered")
			}
			undoDone := make(chan error, 1)
			go func() { undoDone <- UndoMerge(db, merged.MergeLogID) }()
			select {
			case err := <-undoDone:
				if err != nil {
					close(release)
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("undo blocked behind LLM")
			}
			close(release)
			if err := <-done; !errors.Is(err, ErrProfileStale) {
				t.Fatalf("result=%v", err)
			}
			c, err := GetContactByID(db, b)
			if err != nil || c.ProfileSummary == "stale" {
				t.Fatalf("profile=%+v err=%v", c, err)
			}
		})
	}
}

func TestRestoreRollsBackSidecarsAndDB(t *testing.T) {
	src, dst := regressionDB(t), regressionDB(t)
	regressionContact(t, src, "backup")
	id := regressionContact(t, dst, "live")
	srcDir, dstDir := t.TempDir(), t.TempDir()
	names := []string{"config.json", "totp_secret.json"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("new"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dstDir, name), []byte("old"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	zip, cleanup, err := BuildBackupZip(src, srcDir, names)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	original := restoreRename
	defer func() { restoreRename = original }()
	restoreRename = func(from, to string) error {
		if filepath.Base(to) == "totp_secret.json" {
			return errors.New("injected install failure")
		}
		return os.Rename(from, to)
	}
	epoch := currentProfileEpoch()
	if _, err := RestoreBackupZip(dst, zip, dstDir, true); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := GetContactByID(dst, id); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := dst.QueryRow(`SELECT name FROM contacts WHERE id=?`, id).Scan(&name); err != nil || name != "live" {
		t.Fatalf("db not rolled back: %s %v", name, err)
	}
	for _, name := range names {
		path := filepath.Join(dstDir, name)
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "old" {
			t.Fatalf("sidecar %s: %q %v", name, data, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0640 {
			t.Fatalf("permissions: %v %v", info, err)
		}
	}
	if currentProfileEpoch() != epoch {
		t.Fatal("failed restore changed epoch")
	}
	restoreRename = original
	summary, err := RestoreBackupZip(dst, zip, dstDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.NeedRestart || summary.SafetyBackup == "" || currentProfileEpoch() == epoch {
		t.Fatalf("summary=%+v", summary)
	}
	if err := saveProfileAtEpoch(dst, id, `{}`, "stale", "stale", epoch); !errors.Is(err, ErrProfileStale) {
		t.Fatalf("restore accepted stale result: %v", err)
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dstDir, name))
		if err != nil || string(data) != "new" {
			t.Fatalf("sidecar=%q err=%v", data, err)
		}
	}
}

func TestHyphenDate(t *testing.T) {
	for _, date := range []string{"2025-6-10 10:23", "2025 - 6 - 10 10:23:00", "2025-6-10 10:23AM", "2025年6月10日 10:23上午"} {
		msgs := ParseClipboard("张三 "+date+"\n你好", "我")
		if len(msgs) != 1 || msgs[0].Timestamp.Format("2006-01-02 15:04") != "2025-06-10 10:23" {
			t.Fatalf("%s: %+v", date, msgs)
		}
	}
}

func TestLegacyMergeLogStillUndoable(t *testing.T) {
	db := regressionDB(t)
	a, b := regressionContact(t, db, "source"), regressionContact(t, db, "target")
	regressionMessages(t, db, a, "unique")
	result, err := MergeContacts(db, a, b, MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE merge_log SET deleted_messages='[]',target_profile='' WHERE id=?`, result.MergeLogID); err != nil {
		t.Fatal(err)
	}
	if err := UndoMerge(db, result.MergeLogID); err != nil {
		t.Fatal(err)
	}
	msgs, err := GetAllMessages(db, a)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("%v %v", msgs, err)
	}
}

func TestRestoreAndReadsDoNotDeadlock(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "live")
	zip, cleanup, err := BuildBackupZip(db, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	done := make(chan error, 2)
	go func() {
		for i := 0; i < 20; i++ {
			if _, err := GetContactByID(db, id); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	dir := t.TempDir()
	go func() {
		for i := 0; i < 5; i++ {
			if _, err := RestoreBackupZip(db, zip, dir, false); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("deadlock")
		}
	}
}

func TestMigrationV5(t *testing.T) {
	db := regressionDB(t)
	for _, q := range []string{`ALTER TABLE merge_log DROP COLUMN deleted_messages`, `ALTER TABLE merge_log DROP COLUMN target_profile`, `PRAGMA user_version=5`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var columns string
	if err := db.QueryRow(`SELECT group_concat(name) FROM pragma_table_info('merge_log')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(columns, "deleted_messages") || !strings.Contains(columns, "target_profile") {
		t.Fatal(columns)
	}
}
