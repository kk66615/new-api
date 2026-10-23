package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFormatUserLogsStripsQuotaSaturation verifies the admin-only quota
// saturation marker (nested under other.admin_info) is removed for non-admin
// log views, since formatUserLogs strips the whole admin_info object.
func TestFormatUserLogsStripsQuotaSaturation(t *testing.T) {
	other := common.MapToJsonStr(map[string]any{
		"model_price": 0.004,
		"admin_info": map[string]any{
			"quota_saturation": map[string]any{
				"op":      "QuotaFromDecimal",
				"kind":    "overflow",
				"clamped": common.MaxQuota,
			},
		},
	})
	logs := []*Log{{Other: other}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	_, hasAdminInfo := parsed["admin_info"]
	require.False(t, hasAdminInfo, "admin_info (and nested quota_saturation) must be stripped for non-admin views")
	// Non-admin billing fields remain visible.
	require.Contains(t, parsed, "model_price")
}

func TestTaskPluginLogVisibilityIsRoleSeparated(t *testing.T) {
	other := common.MapToJsonStr(map[string]any{
		"model_price": 1.25,
		"admin_info": map[string]any{
			"task_plugin": map[string]any{
				"key":     "document-parser",
				"name":    "Document Parser",
				"version": "1.2.3",
			},
		},
		"root_info": map[string]any{
			"upstream_task_id": "upstream-private",
			"task_plugin": map[string]any{
				"generation": 42,
			},
		},
	})

	t.Run("user", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		formatUserLogs(logs, 0)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.NotContains(t, parsed, "admin_info")
		assert.NotContains(t, parsed, "root_info")
		assert.Equal(t, 1.25, parsed["model_price"])
	})

	t.Run("admin", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		FormatAdminLogs(logs)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Contains(t, parsed, "admin_info")
		assert.NotContains(t, parsed, "root_info")
	})

	t.Run("root", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		FormatRootLogs(logs)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Contains(t, parsed, "admin_info")
		assert.Contains(t, parsed, "root_info")
	})
}

func TestLegacyLogOtherVisibilityIsRoleSeparated(t *testing.T) {
	other := common.MapToJsonStr(map[string]any{
		"request_path":  "/v1/chat/completions",
		"channel_id":    202,
		"channel_name":  "legacy-secret-channel",
		"channel_type":  1,
		"reject_reason": "legacy-policy-rejection",
		"admin_info": map[string]any{
			"existing_admin_field": "preserved",
		},
		"root_info": map[string]any{
			"upstream_request_id": "upstream-private",
		},
		"audit_info": map[string]any{
			"method": "POST",
		},
	})

	t.Run("user", func(t *testing.T) {
		logs := []*Log{{
			Id:          99,
			ChannelId:   77,
			ChannelName: "resolved-secret-channel",
			Other:       other,
		}}

		formatUserLogs(logs, 10)

		assert.Equal(t, 11, logs[0].Id)
		// Local policy divergence from upstream: the numeric channel id is also
		// operator-internal here (see TestFormatUserLogsHidesChannelIdentity).
		assert.Equal(t, 0, logs[0].ChannelId)
		assert.Empty(t, logs[0].ChannelName)
		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Equal(t, "/v1/chat/completions", parsed["request_path"])
		for _, key := range []string{
			"channel_id",
			"channel_name",
			"channel_type",
			"reject_reason",
			"admin_info",
			"root_info",
			"audit_info",
		} {
			assert.NotContains(t, parsed, key)
		}
	})

	t.Run("admin", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		FormatAdminLogs(logs)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Equal(t, "legacy-secret-channel", parsed["channel_name"])
		assert.NotContains(t, parsed, "reject_reason")
		assert.NotContains(t, parsed, "root_info")
		assert.Contains(t, parsed, "audit_info")
		adminInfo, ok := parsed["admin_info"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "preserved", adminInfo["existing_admin_field"])
		assert.Equal(t, "legacy-policy-rejection", adminInfo["reject_reason"])
	})

	t.Run("root", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		FormatRootLogs(logs)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Equal(t, "legacy-secret-channel", parsed["channel_name"])
		assert.NotContains(t, parsed, "reject_reason")
		assert.Contains(t, parsed, "root_info")
		assert.Contains(t, parsed, "audit_info")
		adminInfo, ok := parsed["admin_info"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "preserved", adminInfo["existing_admin_field"])
		assert.Equal(t, "legacy-policy-rejection", adminInfo["reject_reason"])
	})
}

func TestLegacyRejectReasonDoesNotOverrideScopedValue(t *testing.T) {
	other := common.MapToJsonStr(map[string]any{
		"reject_reason": "legacy-value",
		"admin_info": map[string]any{
			"reject_reason": "scoped-value",
		},
	})
	logs := []*Log{{Other: other}}

	FormatRootLogs(logs)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	assert.NotContains(t, parsed, "reject_reason")
	adminInfo, ok := parsed["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "scoped-value", adminInfo["reject_reason"])
}

func TestLegacyRejectReasonHandlesNullAdminInfo(t *testing.T) {
	logs := []*Log{{Other: `{"reject_reason":"legacy-value","admin_info":null}`}}

	FormatAdminLogs(logs)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	assert.NotContains(t, parsed, "reject_reason")
	adminInfo, ok := parsed["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "legacy-value", adminInfo["reject_reason"])
}

func TestLogFormattingPreservesLargeIntegerLexemes(t *testing.T) {
	const other = `{"public_id":9007199254740993,"admin_info":{"admin_id":9007199254740995},"root_info":{"generation":18446744073709551615}}`

	t.Run("user", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		formatUserLogs(logs, 0)

		assert.Contains(t, logs[0].Other, `"public_id":9007199254740993`)
		assert.NotContains(t, logs[0].Other, "admin_id")
		assert.NotContains(t, logs[0].Other, "generation")
	})

	t.Run("admin", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		FormatAdminLogs(logs)

		assert.Contains(t, logs[0].Other, `"public_id":9007199254740993`)
		assert.Contains(t, logs[0].Other, `"admin_id":9007199254740995`)
		assert.NotContains(t, logs[0].Other, "generation")
	})

	t.Run("root", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		FormatRootLogs(logs)

		assert.Equal(t, other, logs[0].Other)
	})

	t.Run("unprivileged", func(t *testing.T) {
		const unprivileged = `{"public_id":9007199254740993,"model_price":0.004}`

		userLogs := []*Log{{Other: unprivileged}}
		formatUserLogs(userLogs, 0)
		assert.Equal(t, unprivileged, userLogs[0].Other)

		adminLogs := []*Log{{Other: unprivileged}}
		FormatAdminLogs(adminLogs)
		assert.Equal(t, unprivileged, adminLogs[0].Other)
	})
}

// TestFormatUserLogsHidesChannelIdentity is the contract for the user log
// projection: neither the channel id column nor the channel name may reach a
// non-admin reader, while the admin projection keeps both.
func TestFormatUserLogsHidesChannelIdentity(t *testing.T) {
	make := func() []*Log {
		return []*Log{{ChannelId: 158, ChannelName: "upstream-vendor", Other: common.MapToJsonStr(map[string]interface{}{"model_price": 0.1})}}
	}
	user := make()
	formatUserLogs(user, 0)
	require.Equal(t, 0, user[0].ChannelId, "channel id must be hidden from users")
	require.Equal(t, "", user[0].ChannelName, "channel name must be hidden from users")

	admin := make()
	FormatAdminLogs(admin)
	require.Equal(t, 158, admin[0].ChannelId)
	require.Equal(t, "upstream-vendor", admin[0].ChannelName)
}

// The upstream response audit (ursp) is for the super administrator only:
// admins must not see it either. It is written under root_info by
// service.appendUpstreamResponseAudit; this pins the projection contract.
func TestUpstreamResponseAuditIsRootOnly(t *testing.T) {
	other := common.MapToJsonStr(map[string]any{
		"model_price": 1.25,
		"admin_info": map[string]any{
			"use_channel": []string{"101"},
		},
		"root_info": map[string]any{
			"ursp": map[string]any{
				"sent":     "gpt-5.5",
				"model":    "gpt-5-6-luna",
				"mismatch": true,
				"kind":     "mismatch",
			},
		},
	})

	t.Run("user", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		formatUserLogs(logs, 0)
		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.NotContains(t, parsed, "root_info")
		assert.NotContains(t, logs[0].Other, "gpt-5-6-luna")
	})

	t.Run("admin", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		FormatAdminLogs(logs)
		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Contains(t, parsed, "admin_info")
		assert.NotContains(t, parsed, "root_info")
		assert.NotContains(t, logs[0].Other, "gpt-5-6-luna")
	})

	t.Run("root", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		FormatRootLogs(logs)
		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		rootInfo, ok := parsed["root_info"].(map[string]any)
		require.True(t, ok)
		ursp, ok := rootInfo["ursp"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, ursp["mismatch"])
	})
}

// The upstream refusal reason is shown to the log owner as the public
// refusal_reason, while the admin-only copy and every other admin_info field
// stay hidden from them; administrators see both.
func TestRefusalReasonIsVisibleToUsersWithoutAdminInfo(t *testing.T) {
	other := NewLogOther()
	other.SetPublic("refusal_reason", "gemini_finish_reason=IMAGE_SAFETY")
	other.SetAdmin("reject_reason", "gemini_finish_reason=IMAGE_SAFETY")
	other.SetAdmin("upstream_model_name", "vendor-internal-model")
	newLogs := func() []*Log { return []*Log{{ChannelId: 172, Other: other.JSONString()}} }

	user := newLogs()
	formatUserLogs(user, 0)
	parsed, err := common.StrToMap(user[0].Other)
	require.NoError(t, err)
	assert.Equal(t, "gemini_finish_reason=IMAGE_SAFETY", parsed["refusal_reason"])
	assert.NotContains(t, parsed, "admin_info")
	assert.NotContains(t, parsed, "reject_reason")
	assert.NotContains(t, user[0].Other, "vendor-internal-model")

	admin := newLogs()
	FormatAdminLogs(admin)
	parsed, err = common.StrToMap(admin[0].Other)
	require.NoError(t, err)
	assert.Equal(t, "gemini_finish_reason=IMAGE_SAFETY", parsed["refusal_reason"])
	adminInfo, ok := parsed["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "gemini_finish_reason=IMAGE_SAFETY", adminInfo["reject_reason"])
}
