package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/internal/control"
)

func resumedFamilyFixture(t *testing.T) (*Server, *Account, *control.Server) {
	t.Helper()
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Dir: dir, Worker: executable, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.listener.Close() })
	account := &Account{ID: "target-07", Dir: dir, AppleID: "target@example.test"}
	s.accounts = []*Account{account}
	worker, err := control.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { worker.Close() })
	worker.SetFamilyAddOneShotCredentialReady(true)
	saved := appleservices.Session{AppleID: account.AppleID, DSID: "fixture-gsa", Tokens: map[string]string{"com.apple.gs.idms.pet": "fixture-pet"}}
	accepted := acceptedICloudCheckpoint{Session: appleservices.Session{AppleID: account.AppleID, DSID: "fixture-cloud", Tokens: map[string]string{"mmeAuthToken": "fixture-mme"}}, ADSID: saved.DSID, ServiceData: map[string]any{"verified": true}}
	for name, value := range map[string]any{"session.json": saved, "icloud-accepted.json": accepted, "runtime.json": Status{PID: os.Getpid(), Running: true, Authenticated: true, State: "online"}} {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return s, account, worker
}

func TestResumedFamilyReadinessDistinguishesPasswordFromOffline(t *testing.T) {
	for _, kind := range []string{"missing", "saved", "in-memory", "unverified", "wrong-binding", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			s, account, worker := resumedFamilyFixture(t)
			loads := 0
			s.familyCredentialLoader = func(dir, appleID string) (string, error) {
				loads++
				if dir != account.Dir || appleID != account.AppleID {
					t.Fatal("vault lookup crossed account binding")
				}
				if kind == "saved" {
					return "fixture-saved-password", nil
				}
				return "", errors.New("not saved")
			}
			wantStage, wantOK := "family-credential-required", false
			switch kind {
			case "saved", "in-memory":
				wantStage, wantOK = "family-add-ready", true
			case "unverified", "unsupported":
				wantStage = "family-worker-unready"
			case "wrong-binding":
				wantStage = "family-binding"
			}
			if kind == "in-memory" {
				worker.SetFamilyAddReady(true)
			}
			if kind == "unsupported" {
				worker.SetFamilyAddOneShotCredentialReady(false)
			}
			if kind == "unverified" {
				data, _ := json.Marshal(Status{PID: os.Getpid(), Running: true, VerificationRequired: true})
				if err := os.WriteFile(filepath.Join(account.Dir, "runtime.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "wrong-binding" {
				if err := os.WriteFile(filepath.Join(account.Dir, "icloud-accepted.json"), []byte(`{"adsid":"another-account"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result := s.familyAddReadiness(context.Background(), account)
			if result.OK != wantOK || result.Stage != wantStage {
				t.Fatalf("unexpected readiness: %+v", result)
			}
			if (loads > 0) != (kind == "missing" || kind == "saved") {
				t.Fatal("vault read before verifying worker and account")
			}
			data, _ := json.Marshal(result)
			if strings.Contains(string(data), "fixture-saved-password") {
				t.Fatal("readiness exposed a credential")
			}
			select {
			case <-worker.Commands:
				t.Fatal("readiness submitted a family action")
			default:
			}
		})
	}
}

func TestSavedFamilyPasswordIsForwardedOnlyInBoundTransientRequest(t *testing.T) {
	s, account, worker := resumedFamilyFixture(t)
	secret := "fixture-encrypted-vault-password"
	s.familyCredentialLoader = func(dir, appleID string) (string, error) {
		if dir != account.Dir || appleID != account.AppleID {
			t.Fatal("wrong credential binding")
		}
		return secret, nil
	}
	received := make(chan control.Action, 1)
	go func() {
		command := <-worker.Commands
		received <- command.Action
		command.Reply <- control.Result{OK: true, Stage: "family-added"}
	}()
	request := httptest.NewRequest(http.MethodPost, s.URL()+"/api/action?account="+account.ID, strings.NewReader(`{"action":"family-add-self","code":"organizer-01"}`))
	request.Header.Set("X-App-Token", s.token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	var result control.Result
	if json.Unmarshal(recorder.Body.Bytes(), &result) != nil || !result.OK || strings.Contains(recorder.Body.String(), secret) {
		t.Fatal("saved credential request failed or leaked")
	}
	select {
	case action := <-received:
		if action.Action != "family-add-self" || action.Code != "organizer-01" || action.TargetAppleID != account.AppleID || action.TargetPassword != secret {
			t.Fatal("worker request lost exact account or organizer binding")
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive credential")
	}
	files, err := os.ReadDir(account.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(account.Dir, file.Name()))
		if err != nil || strings.Contains(string(data), secret) {
			t.Fatal("family password persisted")
		}
	}
}

func TestMissingFamilyPasswordStopsBeforeMutation(t *testing.T) {
	s, account, worker := resumedFamilyFixture(t)
	s.familyCredentialLoader = func(string, string) (string, error) { return "", errors.New("missing") }
	request := httptest.NewRequest(http.MethodPost, s.URL()+"/api/action?account="+account.ID, strings.NewReader(`{"action":"family-add-self","code":"organizer-01"}`))
	request.Header.Set("X-App-Token", s.token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	var result control.Result
	if json.Unmarshal(recorder.Body.Bytes(), &result) != nil || result.OK || result.Stage != "family-credential-required" {
		t.Fatal("online worker missing a password was reported offline")
	}
	select {
	case <-worker.Commands:
		t.Fatal("missing password reached worker mutation")
	default:
	}
}
