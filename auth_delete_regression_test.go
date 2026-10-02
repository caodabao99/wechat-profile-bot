package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuthEnableCannotReplaceBinding(t *testing.T) {
	dir := t.TempDir()
	original := totpSecretPath
	totpSecretPath = func() string { return filepath.Join(dir, "totp_secret.json") }
	defer func() { totpSecretPath = original }()
	s := &apiServer{cfg: &Config{APIToken: "test-token"}, sessions: &webSessionStore{sessions: map[string]time.Time{}, path: filepath.Join(dir, "sessions.json")}}
	enable := func(secret string) *httptest.ResponseRecorder {
		code, err := totpCodeAt(secret, time.Now().Unix()/totpPeriod)
		if err != nil {
			panic(err)
		}
		body, _ := json.Marshal(twofaEnableRequest{Token: "test-token", Secret: secret, Code: code})
		w := httptest.NewRecorder()
		s.hAuthEnable(w, httptest.NewRequest(http.MethodPost, "/api/auth/2fa/enable", bytes.NewReader(body)))
		return w
	}
	secrets := []string{"JBSWY3DPEHPK3PXP", "GEZDGNBVGY3TQOJQ"}
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 16)
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; results[i] = enable(secrets[i%2]) }(i)
	}
	close(start)
	wg.Wait()
	winner, successes := -1, 0
	for i, w := range results {
		switch w.Code {
		case http.StatusOK:
			winner = i
			successes++
		case http.StatusConflict:
			if strings.Contains(w.Body.String(), "session") {
				t.Fatal("rejected request issued a session")
			}
		default:
			t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
		}
	}
	if successes != 1 {
		t.Fatalf("successful bindings = %d", successes)
	}
	f, err := totpLoadSecret()
	if err != nil {
		t.Fatal(err)
	}
	if f.Secret != secrets[winner%2] || f.LastUsedStep == 0 {
		t.Fatalf("winning binding was not persisted: %+v", f)
	}
	before, err := os.ReadFile(totpSecretPath())
	if err != nil {
		t.Fatal(err)
	}
	if w := enable(secrets[(winner+1)%2]); w.Code != http.StatusConflict {
		t.Fatalf("replacement status = %d", w.Code)
	}
	after, _ := os.ReadFile(totpSecretPath())
	if !bytes.Equal(before, after) || len(s.sessions.sessions) != 1 {
		t.Fatal("replacement changed binding or issued session")
	}
	if err := os.WriteFile(totpSecretPath(), []byte("broken binding"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := enable(secrets[0]); w.Code != http.StatusConflict {
		t.Fatalf("corrupt binding replacement status = %d", w.Code)
	}
}

func TestBotRestoreInvalidatesDeleteOnlyOnSuccess(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			src, dst := regressionDB(t), regressionDB(t)
			newID := regressionContact(t, src, "new person")
			oldID := regressionContact(t, dst, "old person")
			if newID != oldID {
				t.Fatal("fixture must reuse ID")
			}
			srcDir, dstDir := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(srcDir, "totp_secret.json"), []byte("test-only"), 0600); err != nil {
				t.Fatal(err)
			}
			zip, cleanup, err := BuildBackupZip(src, srcDir, []string{"totp_secret.json"})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			b := &Bot{db: dst}
			if reply := b.deleteContact(&ILinkMessage{FromUserID: "restore-user"}, "old person"); !strings.Contains(reply, "确认删除") {
				t.Fatal(reply)
			}
			originalRename := restoreRename
			entered, release := make(chan struct{}), make(chan struct{})
			restoreRename = func(from, to string) error {
				close(entered)
				<-release
				if fail {
					return errors.New("injected install failure")
				}
				return originalRename(from, to)
			}
			defer func() { restoreRename = originalRename }()
			restored := make(chan error, 1)
			go func() { _, err := restoreBotBackup(dst, zip, dstDir, false, ""); restored <- err }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("restore did not reach install")
			}
			confirmed := make(chan string, 1)
			go func() { confirmed <- b.ConfirmDelete("restore-user", "old person") }()
			select {
			case reply := <-confirmed:
				close(release)
				t.Fatalf("confirmation escaped restore lock: %s", reply)
			case <-time.After(30 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-restored:
				if (err != nil) != fail {
					t.Fatalf("restore error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("restore deadlocked")
			}
			var reply string
			select {
			case reply = <-confirmed:
			case <-time.After(5 * time.Second):
				t.Fatal("confirmation deadlocked")
			}
			if fail {
				if !strings.Contains(reply, "已删除") {
					t.Fatalf("rollback discarded valid confirmation: %s", reply)
				}
				if _, err := GetContactByID(dst, oldID); err == nil {
					t.Fatal("old person not deleted")
				}
			} else {
				if !strings.Contains(reply, "没有待确认") {
					t.Fatal(reply)
				}
				c, err := GetContactByID(dst, newID)
				if err != nil || c.Name != "new person" {
					t.Fatalf("restored contact damaged: %v, %v", c, err)
				}
				b.deleteContact(&ILinkMessage{FromUserID: "restore-user"}, "new person")
				if reply := b.ConfirmDelete("restore-user", "new person"); !strings.Contains(reply, "已删除") {
					t.Fatal(reply)
				}
			}
		})
	}
}
