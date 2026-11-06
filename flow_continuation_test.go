package appleservices

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Laky-64/appleservices/gsa"
	"github.com/Laky-64/appleservices/icloud"
)

type activationTransport func(*http.Request) (*http.Response, error)

func (f activationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestActivationContinuationRequiresLiveReadOnlyQuotaAcceptance(t *testing.T) {
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	for _, status := range []int{http.StatusUnauthorized, http.StatusOK} {
		login, backend, _ := verifiedPendingLogin(t)
		accepted := activationFixture()
		accepted.ServiceData = map[string]any{"com.apple.Dataclass.Quota": map[string]any{"storageInfoURL": "https://p01-quota.icloud.com/quota"}}
		calls := 0
		http.DefaultTransport = activationTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != http.MethodGet || r.URL.Host != "p01-quota.icloud.com" || r.Header.Get("X-Apple-ADSID") != accepted.ADSID {
				t.Fatal("activation verification was not an account-bound quota read")
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"storage_data":{"storage_info_in_bytes":{"commerce_storage_in_bytes":0},"quota_info_in_bytes":{"total_quota":5368709120,"total_used":0,"total_available":5368709120}}}`))}, nil
		})
		client, err := login.ContinueWithICloudSession(context.Background(), accepted)
		if (err == nil) != (status == http.StatusOK) || (client != nil) != (status == http.StatusOK) || (login.client != nil) != (status == http.StatusOK) {
			t.Fatal("online state did not follow live quota acceptance")
		}
		if calls != 1 || backend.calls != 2 || len(backend.requests) != 1 {
			t.Fatal("activation repeated authentication")
		}
	}
}

func verifiedPendingLogin(t *testing.T) (*Login, *fakeBackend, *memoryStore) {
	t.Helper()
	store := &memoryStore{}
	backend := &fakeBackend{results: []*gsa.LoginResult{testResult(true), testResult(false)}}
	login, err := BeginLogin(Credentials{"owner@example.test", "fixture-password"}, store, testOptions(backend, func(map[string]string, string, string, string) (icloud.DelegateTokens, error) {
		return icloud.DelegateTokens{}, errors.New("fixture delegate refusal")
	})...)
	if err != nil {
		t.Fatal(err)
	}
	if err = login.RequestCode(SMS); err != nil {
		t.Fatal(err)
	}
	if err = login.SubmitCode(SMS, "123456"); err != nil {
		t.Fatal(err)
	}
	if client, err := login.Client(); client != nil || !errors.Is(err, ErrICloudSessionPending) {
		t.Fatal("verified delegate refusal must remain pending")
	}
	return login, backend, store
}

func activationFixture() RestoredICloudSession {
	return RestoredICloudSession{DSID: "fixture-cloud", ADSID: "test-adsid", MMEAuthToken: "fixture-mme", ServiceData: map[string]any{"quota": true}}
}

func TestSMSLoginContinuesInPlaceAfterActivation(t *testing.T) {
	login, backend, store := verifiedPendingLogin(t)
	checks := 0
	client, err := login.continueWithICloudSession(context.Background(), activationFixture(), func(candidate *Client) error {
		checks++
		if login.client != nil || candidate.ADSID != login.adsid {
			t.Fatal("candidate exposed before verification")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if client != login.client || checks != 1 || login.creds.Password != "fixture-password" || login.backend != backend {
		t.Fatal("activation lost original credentials/backend")
	}
	if backend.calls != 2 || len(backend.requests) != 1 || len(backend.submits) != 1 || login.NeedsTwoFactor() {
		t.Fatal("activation repeated account login or SMS verification")
	}
	if err := login.SaveSession(); err != nil || store.session.AppleID != "owner@example.test" {
		t.Fatal("continued login cannot save its original session")
	}
	if got, err := login.Client(); err != nil || got != client {
		t.Fatal("continued login reentered delegate exchange")
	}
}

func TestActivationContinuationRejectsInvalidSessions(t *testing.T) {
	for _, kind := range []string{"wrong-adsid", "wrong-saved-account", "wrong-saved-adsid", "missing-token", "missing-service-data", "missing-dsid", "unverified", "expired", "canceled", "canceled-during-check"} {
		t.Run(kind, func(t *testing.T) {
			login, backend, store := verifiedPendingLogin(t)
			accepted := activationFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "wrong-adsid":
				accepted.ADSID = "another-account"
			case "wrong-saved-account":
				store.session.AppleID = "another@example.test"
			case "wrong-saved-adsid":
				store.session.DSID = "another-account"
			case "missing-token":
				accepted.MMEAuthToken = ""
			case "missing-service-data":
				accepted.ServiceData = nil
			case "missing-dsid":
				accepted.DSID = ""
			case "unverified":
				login.needsTwoFactor = true
			case "canceled":
				cancel()
			}
			checked := false
			client, err := login.continueWithICloudSession(ctx, accepted, func(*Client) error {
				checked = true
				if kind == "expired" {
					return &icloud.StorageError{Stage: "quota", HTTPStatus: 401}
				}
				if kind == "canceled-during-check" {
					cancel()
					return nil
				}
				t.Fatal("invalid session reached online verification")
				return nil
			})
			if err == nil || client != nil || login.client != nil {
				t.Fatal("invalid session reported authenticated")
			}
			if checked != (kind == "expired" || kind == "canceled-during-check") {
				t.Fatal("unexpected live check")
			}
			if backend.calls != 2 || len(backend.requests) != 1 {
				t.Fatal("invalid checkpoint triggered another login")
			}
		})
	}
}
