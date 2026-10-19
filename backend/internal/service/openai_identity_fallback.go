package service

// 从 id_token 兜底恢复 ChatGPT 身份字段。
//
// 背景（2026-10-19 实例）：账号 6 的 credentials 一度只剩
//   access_token / id_token / model_mapping / refresh_token
// chatgpt_account_id、chatgpt_user_id、plan_type、email 全部消失，
// 导致重置次数/额度查询直接 400：
//   OPENAI_QUOTA_MISSING_ACCOUNT_ID "chatgpt_account_id is missing; please re-authorize"
//
// id_token 是一个 JWT，payload 里的 "https://api.openai.com/auth" claim 始终包含
// chatgpt_account_id / chatgpt_user_id / chatgpt_plan_type / email，因此即使
// credentials 缺字段也能无损恢复，并且可以在每次 token 刷新后自动补齐，避免再次出现。

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const openAIAuthClaimKey = "https://api.openai.com/auth"

// decodeJWTPayloadUnverified 读取 JWT 的 payload（不校验签名；这些 token 来自上游，
// 签名校验不是本工具的职责，这里只用于读取身份元數據）。
func decodeJWTPayloadUnverified(token string) map[string]any {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	// base64url 无 padding
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil
	}
	return claims
}

// openAIChatGPTIdentity 从 id_token 提取身份字段。缺失的字段返回空字符串。
func openAIChatGPTIdentity(idToken string) (accountID, userID, planType, email string) {
	claims := decodeJWTPayloadUnverified(idToken)
	if claims == nil {
		return "", "", "", ""
	}
	email, _ = claims["email"].(string)
	auth, _ := claims[openAIAuthClaimKey].(map[string]any)
	if auth == nil {
		return "", "", "", email
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := auth[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	accountID = pick("chatgpt_account_id", "chatgpt_accountId")
	userID = pick("chatgpt_user_id", "chatgpt_userId", "user_id")
	planType = pick("chatgpt_plan_type", "plan_type")
	return accountID, userID, planType, email
}

// ResolveOpenAIChatGPTAccountID 解析该账号的 ChatGPT account id：
// credentials.chatgpt_account_id → credentials.organization_id → id_token 的 auth claim。
// 前两条都空时（正是 2026-10-19 那次故障的状态），第三级兜底能救回来。
func ResolveOpenAIChatGPTAccountID(account *Account) string {
	if account == nil {
		return ""
	}
	if v := strings.TrimSpace(account.GetCredential("chatgpt_account_id")); v != "" {
		return v
	}
	if v := strings.TrimSpace(account.GetCredential("organization_id")); v != "" {
		return v
	}
	accountID, _, _, _ := openAIChatGPTIdentity(account.GetCredential("id_token"))
	return accountID
}

// EnsureOpenAIIdentityCredentials 用 id_token 补齐 credentials 中缺失的身份字段。
// 只补空缺，不覆盖已有值。返回是否发生了补充。
func EnsureOpenAIIdentityCredentials(creds map[string]any) bool {
	if creds == nil {
		return false
	}
	idToken, _ := creds["id_token"].(string)
	if strings.TrimSpace(idToken) == "" {
		return false
	}
	accountID, userID, planType, email := openAIChatGPTIdentity(idToken)
	if accountID == "" && userID == "" && planType == "" && email == "" {
		return false
	}

	changed := false
	fill := func(key, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if existing, ok := creds[key].(string); ok && strings.TrimSpace(existing) != "" {
			return
		}
		creds[key] = value
		changed = true
	}
	fill("chatgpt_account_id", accountID)
	fill("chatgpt_user_id", userID)
	fill("plan_type", planType)
	fill("email", email)
	return changed
}
