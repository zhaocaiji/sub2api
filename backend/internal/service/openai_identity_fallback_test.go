package service

// 身份兜底逻辑的回归测试。
// 场景来源：2026-10-19 账号 6 的 credentials 只剩 4 个 token 字段，
// chatgpt_account_id 丢失导致重置次数查询 400。

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// buildTestIDToken 造一个含 auth claim 的假 id_token（签名段随便填，本逻辑不验签）。
func buildTestIDToken(t *testing.T, accountID, userID, planType, email string) string {
	t.Helper()
	payload := map[string]any{
		"email": email,
		"sub":   "test",
		openAIAuthClaimKey: map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    userID,
			"chatgpt_plan_type":  planType,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestOpenAIIdentityFromIDToken(t *testing.T) {
	tok := buildTestIDToken(t, "acct-123", "user-456", "prolite", "a@b.c")
	accountID, userID, planType, email := openAIChatGPTIdentity(tok)
	if accountID != "acct-123" || userID != "user-456" || planType != "prolite" || email != "a@b.c" {
		t.Fatalf("解出身份不对: %q %q %q %q", accountID, userID, planType, email)
	}
}

func TestEnsureOpenAIIdentityCredentialsFillsMissing(t *testing.T) {
	tok := buildTestIDToken(t, "acct-123", "user-456", "prolite", "a@b.c")
	creds := map[string]any{
		"access_token":  "at",
		"refresh_token": "rt",
		"id_token":      tok,
	}
	if !EnsureOpenAIIdentityCredentials(creds) {
		t.Fatal("应当补齐并返回 true")
	}
	if creds["chatgpt_account_id"] != "acct-123" {
		t.Fatalf("chatgpt_account_id 未补齐: %v", creds["chatgpt_account_id"])
	}
	if creds["chatgpt_user_id"] != "user-456" {
		t.Fatalf("chatgpt_user_id 未补齐: %v", creds["chatgpt_user_id"])
	}
	if creds["plan_type"] != "prolite" {
		t.Fatalf("plan_type 未补齐: %v", creds["plan_type"])
	}
	if creds["email"] != "a@b.c" {
		t.Fatalf("email 未补齐: %v", creds["email"])
	}
}

func TestEnsureOpenAIIdentityCredentialsKeepsExisting(t *testing.T) {
	tok := buildTestIDToken(t, "acct-from-token", "user-from-token", "prolite", "token@b.c")
	creds := map[string]any{
		"id_token":           tok,
		"chatgpt_account_id": "acct-explicit",
		"plan_type":          "pro",
	}
	if EnsureOpenAIIdentityCredentials(creds) {
		// 只有缺失的 email 会被补，仍返回 true；但已有值不能被覆盖
	}
	if creds["chatgpt_account_id"] != "acct-explicit" {
		t.Fatalf("已有的 chatgpt_account_id 被覆盖: %v", creds["chatgpt_account_id"])
	}
	if creds["plan_type"] != "pro" {
		t.Fatalf("已有的 plan_type 被覆盖: %v", creds["plan_type"])
	}
	if creds["email"] != "token@b.c" {
		t.Fatalf("email 应被补齐: %v", creds["email"])
	}
}

func TestResolveOpenAIChatGPTAccountIDFallbacks(t *testing.T) {
	tok := buildTestIDToken(t, "acct-from-token", "u", "prolite", "a@b.c")

	// 1) 显式字段优先
	a := &Account{Credentials: map[string]any{"chatgpt_account_id": "explicit", "id_token": tok}}
	if got := ResolveOpenAIChatGPTAccountID(a); got != "explicit" {
		t.Fatalf("应优先用 chatgpt_account_id，得到 %q", got)
	}

	// 2) 退到 organization_id
	a = &Account{Credentials: map[string]any{"organization_id": "org-1", "id_token": tok}}
	if got := ResolveOpenAIChatGPTAccountID(a); got != "org-1" {
		t.Fatalf("应退到 organization_id，得到 %q", got)
	}

	// 3) 只剩 token —— 2026-10-19 的实际故障状态
	a = &Account{Credentials: map[string]any{"id_token": tok, "access_token": "at"}}
	if got := ResolveOpenAIChatGPTAccountID(a); got != "acct-from-token" {
		t.Fatalf("应从 id_token 兜底，得到 %q", got)
	}

	// 4) 什么都没有
	a = &Account{Credentials: map[string]any{}}
	if got := ResolveOpenAIChatGPTAccountID(a); got != "" {
		t.Fatalf("应返回空，得到 %q", got)
	}
}
