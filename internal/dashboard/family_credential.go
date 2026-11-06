package dashboard

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/control"
	"github.com/Laky-64/appleservices/internal/credentialvault"
)

// prepareFamilyAddAction fills only a transient, account-bound worker request.
// Resuming an accepted session does not give the worker an in-memory password;
// that is a credential requirement, not a reason to restart authentication.
func (s *Server) prepareFamilyAddAction(parent context.Context, account *Account, action *control.Action) control.Result {
	unready := control.Result{Stage: "family-worker-unready", Message: "目标账号后台尚未在线，请先恢复连接"}
	if account == nil || action.Action != "family-add-self" {
		return unready
	}
	if action.HasOneShotFamilyCredential() {
		if !action.ValidCredentialFields() || !strings.EqualFold(strings.TrimSpace(action.TargetAppleID), account.AppleID) {
			return control.Result{Stage: "family-binding", Message: "目标账号凭据绑定无效"}
		}
		return s.familyAddCredentialReadiness(parent, account)
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var worker control.View
	if err := s.callFor(ctx, account.Dir, "/view", nil, &worker); err != nil {
		return unready
	}
	var status Status
	if json.Unmarshal(worker.Status, &status) != nil || !status.Running || !status.Authenticated || status.VerificationRequired {
		return unready
	}
	if slices.Contains(worker.Capabilities, control.FamilyAddReadyCapability) {
		return control.Result{OK: true, Stage: "family-add-ready", Message: "目标账号后台已就绪"}
	}
	if !slices.Contains(worker.Capabilities, control.FamilyAddOneShotCredentialCapability) {
		return control.Result{Stage: "family-worker-unready", Message: "当前后台版本不支持补充家庭操作密码，请更新后台"}
	}
	if !verifiedGSACheckpointReady(account.Dir, account.AppleID) || !acceptedCheckpointReady(account.Dir, account.AppleID) {
		return control.Result{Stage: "family-binding", Message: "目标账号的已验证会话绑定不完整，请先检查激活状态"}
	}
	load := s.familyCredentialLoader
	if load == nil {
		load = credentialvault.Load
	}
	password, err := load(account.Dir, account.AppleID)
	if err != nil || !control.ValidOneShotFamilyPassword(password) {
		return control.Result{Stage: "family-credential-required", Message: "目标账号已在线，但尚未保存加入家庭所需的密码。请补充当前子号密码，无需重新连接后台"}
	}
	action.TargetAppleID, action.TargetPassword = account.AppleID, password
	return control.Result{OK: true, Stage: "family-add-ready", Message: "目标账号的家庭操作凭据已就绪"}
}

func (s *Server) familyAddReadiness(ctx context.Context, account *Account) control.Result {
	action := control.Action{Action: "family-add-self"}
	defer func() { action.TargetAppleID, action.TargetPassword = "", "" }()
	return s.prepareFamilyAddAction(ctx, account, &action)
}
