package main

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/internal/control"
)

type continuingLogin interface {
	Client() (*appleservices.Client, error)
	ContinueWithICloudSession(context.Context, appleservices.RestoredICloudSession) (*appleservices.Client, error)
}

func completeICloudLogin(ctx context.Context, login continuingLogin, store acceptedSessionLoader, commands <-chan control.Command, status *runtimeStatus, dir string, wait bool) (*appleservices.Client, error) {
	client, err := login.Client()
	if err == nil || !wait || !errors.Is(err, appleservices.ErrICloudSessionPending) {
		return client, err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	return awaitICloudActivation(ctx, login, store, commands, ticker.C, status, dir)
}

// Poll local activation checkpoints, not Apple's authentication endpoints. Only
// a changed checkpoint or an explicit retry makes another network request.
func awaitICloudActivation(ctx context.Context, login continuingLogin, store acceptedSessionLoader, commands <-chan control.Command, tick <-chan time.Time, status *runtimeStatus, dir string) (*appleservices.Client, error) {
	status.State, status.Authenticated, status.VerificationRequired = "awaiting_icloud_activation", false, false
	status.ICloudIssue = ""
	if err := status.save(dir); err != nil {
		return nil, err
	}
	var lastAttempt *appleservices.RestoredICloudSession
	tryCheckpoint := func(attemptCtx context.Context, force bool) *appleservices.Client {
		accepted, err := store.LoadAcceptedICloud()
		if err != nil || !force && lastAttempt != nil && reflect.DeepEqual(*lastAttempt, accepted) {
			return nil
		}
		lastAttempt = &accepted
		checkCtx, cancel := context.WithTimeout(attemptCtx, 45*time.Second)
		defer cancel()
		client, err := login.ContinueWithICloudSession(checkCtx, accepted)
		if err != nil {
			status.ICloudIssue = "checkpoint_unavailable"
			return nil
		}
		return client
	}
	client := tryCheckpoint(ctx, false)
	savedIssue := ""
	for client == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A local poll is not a status transition. Avoid rewriting runtime.json
		// while the dashboard is reading it, especially on Windows.
		if status.ICloudIssue != savedIssue {
			if err := status.save(dir); err != nil {
				return nil, err
			}
			savedIssue = status.ICloudIssue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick:
			client = tryCheckpoint(ctx, false)
		case command := <-commands:
			if command.Context.Err() != nil {
				continue
			}
			result := control.Result{Stage: "awaiting_icloud_activation", Message: "账号验证已完成，请先在面板继续 iCloud 激活，无需再次登录或发送验证码"}
			if command.Action.Action == "continue-login" {
				// Both stopping the worker and disconnecting the request cancel
				// read-only validation. Neither triggers a fresh GSA login.
				attemptCtx, cancel := context.WithCancel(ctx)
				stop := context.AfterFunc(command.Context, cancel)
				client = tryCheckpoint(attemptCtx, true)
				if client == nil && attemptCtx.Err() == nil {
					client, _ = login.Client()
				}
				stop()
				cancel()
				if client != nil {
					result = control.Result{OK: true, Stage: "signing_in", Message: "iCloud 会话已验证，正在继续完成登录"}
				} else if status.ICloudIssue != "" {
					result.Message = "已保存的 iCloud 会话暂未通过在线检查；账号验证仍保留，可稍后重试"
				}
			}
			select {
			case command.Reply <- result:
			case <-command.Context.Done():
			case <-ctx.Done():
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	status.State, status.ICloudIssue = "signing_in", ""
	if err := status.save(dir); err != nil {
		return nil, err
	}
	return client, nil
}
