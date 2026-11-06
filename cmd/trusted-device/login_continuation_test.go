package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/internal/control"
)

type activationStore struct {
	mu       sync.Mutex
	accepted appleservices.RestoredICloudSession
	loads    chan struct{}
}

func (s *activationStore) LoadAcceptedICloud() (appleservices.RestoredICloudSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.loads <- struct{}{}:
	default:
	}
	if s.accepted.DSID == "" {
		return appleservices.RestoredICloudSession{}, os.ErrNotExist
	}
	return s.accepted, nil
}

func (s *activationStore) activate(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted = appleservices.RestoredICloudSession{DSID: "fixture-cloud", ADSID: "fixture-account", MMEAuthToken: token, ServiceData: map[string]any{"quota": true}}
}

type activationLogin struct {
	err         error
	clientCalls atomic.Int32
	checks      atomic.Int32
	checked     chan struct{}
	retryOK     bool
}

func (l *activationLogin) Client() (*appleservices.Client, error) {
	if l.clientCalls.Add(1) > 1 && l.retryOK {
		return &appleservices.Client{ADSID: "fixture-account"}, nil
	}
	return nil, l.err
}

func (l *activationLogin) ContinueWithICloudSession(ctx context.Context, accepted appleservices.RestoredICloudSession) (*appleservices.Client, error) {
	l.checks.Add(1)
	defer func() {
		if l.checked != nil {
			l.checked <- struct{}{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accepted.MMEAuthToken != "accepted" {
		return nil, errors.New("expired fixture")
	}
	return &appleservices.Client{DSID: accepted.DSID, ADSID: accepted.ADSID}, nil
}

func waitActivationTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("activation continuation timed out")
		var zero T
		return zero
	}
}

func TestDelegateRefusalKeepsWorkerAliveAndContinuesWithCheckpoint(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &activationStore{loads: make(chan struct{}, 1)}
	login := &activationLogin{err: appleservices.ErrICloudSessionPending}
	status := runtimeStatus{Running: true, State: "signing_in"}
	done := make(chan error, 1)
	go func() {
		client, err := completeICloudLogin(ctx, login, store, nil, &status, dir, true)
		if err == nil && (client == nil || client.ADSID != "fixture-account") {
			err = errors.New("no accepted client")
		}
		done <- err
	}()
	waitActivationTest(t, store.loads)
	data, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pending runtimeStatus
	if json.Unmarshal(data, &pending) != nil || !pending.Running || pending.Authenticated || pending.VerificationRequired || pending.State != "awaiting_icloud_activation" {
		t.Fatal("verified worker stopped or claimed full authentication")
	}
	store.activate("accepted")
	if err := waitActivationTest(t, done); err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.State != "signing_in" || login.clientCalls.Load() != 1 || login.checks.Load() != 1 {
		t.Fatal("continuation restarted authentication or did not resume startup")
	}
}

func TestActivationWaitRejectsStaleCheckpointWithoutPollingApple(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &activationStore{loads: make(chan struct{}, 1)}
	store.activate("expired")
	login := &activationLogin{checked: make(chan struct{}, 4)}
	status := runtimeStatus{Running: true}
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() { _, err := awaitICloudActivation(ctx, login, store, nil, ticks, &status, dir); done <- err }()
	waitActivationTest(t, login.checked)
	waitActivationTest(t, store.loads)
	for i := 0; i < 3; i++ {
		ticks <- time.Now()
		waitActivationTest(t, store.loads)
	}
	if login.checks.Load() != 1 {
		t.Fatal("unchanged invalid checkpoint caused repeated network verification")
	}
	store.activate("accepted")
	ticks <- time.Now()
	if err := waitActivationTest(t, done); err != nil {
		t.Fatal(err)
	}
	if login.checks.Load() != 2 || status.ICloudIssue != "" {
		t.Fatal("changed checkpoint did not clear the failed check")
	}
}

func TestPendingLoginSupportsRetryAndRejectsDuplicateSMS(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &activationStore{loads: make(chan struct{}, 1)}
	login := &activationLogin{err: appleservices.ErrICloudSessionPending, retryOK: true}
	status := runtimeStatus{Running: true}
	commands := make(chan control.Command)
	done := make(chan error, 1)
	go func() { _, err := completeICloudLogin(ctx, login, store, commands, &status, dir, true); done <- err }()
	waitActivationTest(t, store.loads)
	for _, action := range []string{"submit-sms", "resend-sms", "continue-login"} {
		reply := make(chan control.Result, 1)
		commands <- control.Command{Action: control.Action{Action: action}, Context: ctx, Reply: reply}
		result := waitActivationTest(t, reply)
		if result.OK != (action == "continue-login") {
			t.Fatalf("unexpected result for %s", action)
		}
	}
	if err := waitActivationTest(t, done); err != nil {
		t.Fatal(err)
	}
	if login.clientCalls.Load() != 2 {
		t.Fatal("retry did not use the original delegate exchange")
	}
}

func TestPendingLoginCanStopAndDoesNotHideOtherFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &activationStore{loads: make(chan struct{}, 1)}
	status := runtimeStatus{Running: true}
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := completeICloudLogin(ctx, &activationLogin{err: appleservices.ErrICloudSessionPending}, store, nil, &status, dir, true)
		done <- err
	}()
	waitActivationTest(t, store.loads)
	cancel()
	if err := waitActivationTest(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal("stop did not cancel activation wait")
	}
	for _, test := range []struct {
		err  error
		wait bool
	}{
		{errors.New("password rejected"), true},
		{errors.New("verification rejected"), true},
		{appleservices.ErrICloudSessionPending, false},
	} {
		status := runtimeStatus{State: "signing_in"}
		_, err := completeICloudLogin(context.Background(), &activationLogin{err: test.err}, store, nil, &status, dir, test.wait)
		if !errors.Is(err, test.err) || status.State != "signing_in" {
			t.Fatal("non-continuable error was hidden")
		}
	}
}
