package controller

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// useHiddenNightGroup configures three groups of which "night" is not offered
// to ordinary users.
func useHiddenNightGroup(t *testing.T) {
	t.Helper()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":2,"night":0.15}`))
	t.Cleanup(func() {
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalRatios))
	})
}

func TestFilterPricingByUsableGroupsHidesUnusableGroups(t *testing.T) {
	useHiddenNightGroup(t)
	const expression = `tier("base", p * 1 + c * 4) * (group != "night" && hour("Asia/Shanghai") >= 9 ? 2 : 1)`
	pricing := []model.Pricing{
		{ModelName: "model-mixed", EnableGroup: []string{"night", "default"}, BillingExpr: expression, BillingPluginVariants: []model.PricingPluginVariant{{PluginKey: "plugin", BillingExpr: expression}}},
		{ModelName: "model-night-only", EnableGroup: []string{"night"}, BillingExpr: expression},
		{ModelName: "model-all", EnableGroup: []string{"all", "night"}, BillingExpr: `tier("base", p * 1) * (group startsWith "night" ? 1 : 2)`},
	}

	filtered := filterPricingByUsableGroups(pricing, map[string]string{"default": "Default", "vip": "VIP"}, false)

	require.Len(t, filtered, 2)
	assert.Equal(t, "model-mixed", filtered[0].ModelName)
	assert.Equal(t, []string{"default"}, filtered[0].EnableGroup)
	const shown = `tier("base", p * 1 + c * 4) * (hour("Asia/Shanghai") >= 9 ? 2 : 1)`
	assert.Equal(t, shown, filtered[0].BillingExpr)
	assert.Equal(t, shown, filtered[0].BillingPluginVariants[0].BillingExpr)
	assert.Equal(t, "model-all", filtered[1].ModelName)
	assert.Equal(t, []string{"all"}, filtered[1].EnableGroup)
	assert.Empty(t, filtered[1].BillingExpr, "an expression that still names a hidden group is withheld")
	assert.Equal(t, []string{"night", "default"}, pricing[0].EnableGroup, "the shared pricing cache must stay untouched")
	assert.Equal(t, expression, pricing[0].BillingExpr, "the shared pricing cache must stay untouched")
	assert.Equal(t, expression, pricing[0].BillingPluginVariants[0].BillingExpr, "the shared pricing cache must stay untouched")

	entitled := filterPricingByUsableGroups(pricing, map[string]string{"default": "Default", "night": "Night"}, false)
	require.Len(t, entitled, 3)
	assert.Equal(t, []string{"night", "default"}, entitled[0].EnableGroup)
	assert.Equal(t, expression, entitled[0].BillingExpr)

	administrator := filterPricingByUsableGroups(pricing, map[string]string{"default": "Default", "vip": "VIP"}, true)
	require.Len(t, administrator, 2, "an administrator still lists only the models they can use")
	assert.Equal(t, []string{"night", "default"}, administrator[0].EnableGroup)
	assert.Equal(t, expression, administrator[0].BillingExpr)

	assert.Empty(t, filterPricingByUsableGroups(pricing, map[string]string{}, false))
}

func TestGetPerfMetricsReturnsOnlyViewerGroups(t *testing.T) {
	useHiddenNightGroup(t)
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.PerfMetric{}))
	require.NoError(t, db.Create(&model.User{Id: 7, Username: "night-user", AffCode: "night-user", Password: "password", Group: "night", Status: common.UserStatusEnabled}).Error)

	const modelName = "zz-perf-hidden-group-model"
	for _, group := range []string{"default", "night"} {
		perfmetrics.Record(perfmetrics.Sample{Model: modelName, Group: group, LatencyMs: 100, Success: true})
	}

	groupsSeenBy := func(userID, role int) []string {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/perf-metrics?model="+modelName, nil)
		if userID != 0 {
			ctx.Set("id", userID)
			ctx.Set("role", role)
		}
		GetPerfMetrics(ctx)
		require.Equal(t, http.StatusOK, recorder.Code)
		var response struct {
			Data perfmetrics.QueryResult `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		groups := make([]string, 0, len(response.Data.Groups))
		for _, group := range response.Data.Groups {
			groups = append(groups, group.Group)
		}
		return groups
	}

	require.NoError(t, db.Create(&model.User{Id: 8, Username: "default-user", AffCode: "default-user", Password: "password", Group: "default", Status: common.UserStatusEnabled}).Error)
	modelsSeenBy := func(userID, role int) []string {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/perf-metrics/summary", nil)
		if userID != 0 {
			ctx.Set("id", userID)
			ctx.Set("role", role)
		}
		GetPerfMetricsSummary(ctx)
		require.Equal(t, http.StatusOK, recorder.Code)
		var response struct {
			Data perfmetrics.SummaryAllResult `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		names := make([]string, 0, len(response.Data.Models))
		for _, summary := range response.Data.Models {
			names = append(names, summary.ModelName)
		}
		return names
	}
	const nightOnlyModel = "zz-perf-night-only-model"
	perfmetrics.Record(perfmetrics.Sample{Model: nightOnlyModel, Group: "night", LatencyMs: 100, Success: true})
	assert.NotContains(t, modelsSeenBy(0, 0), nightOnlyModel)
	assert.Contains(t, modelsSeenBy(0, 0), modelName)
	assert.Contains(t, modelsSeenBy(7, common.RoleCommonUser), nightOnlyModel)
	assert.Contains(t, modelsSeenBy(8, common.RoleAdminUser), nightOnlyModel)

	// Visibility follows the group settings as they are at request time.
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP","night":"Night"}`))
	assert.ElementsMatch(t, []string{"default", "night"}, groupsSeenBy(0, 0))
	assert.Contains(t, modelsSeenBy(0, 0), nightOnlyModel)
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP"}`))
	assert.NotContains(t, modelsSeenBy(0, 0), nightOnlyModel)

	assert.Equal(t, []string{"default"}, groupsSeenBy(0, 0))
	assert.Equal(t, []string{"default"}, groupsSeenBy(8, common.RoleCommonUser))
	assert.ElementsMatch(t, []string{"default", "night"}, groupsSeenBy(7, common.RoleCommonUser))
	assert.ElementsMatch(t, []string{"default", "night"}, groupsSeenBy(8, common.RoleAdminUser))
}

func TestHideGroupsInLogCleansTheStoredExpression(t *testing.T) {
	const expression = `tier("base", p * 1 + c * 4) * (group != "night" && hour("Asia/Shanghai") >= 9 ? 2 : 1)`
	const shown = `tier("base", p * 1 + c * 4) * (hour("Asia/Shanghai") >= 9 ? 2 : 1)`
	const condition = `group != "night" && hour("Asia/Shanghai") >= 9`
	stored := func(expression, condition string) string {
		other, err := common.Marshal(map[string]any{
			"billing_mode":  "tiered_expr",
			"expr_b64":      base64.StdEncoding.EncodeToString([]byte(expression)),
			"matched_tier":  "base",
			"request_rules": []map[string]any{{"cond": condition, "multiplier": 2, "matched": false}},
		})
		require.NoError(t, err)
		return string(other)
	}
	read := func(log *model.Log) (expression string, condition string) {
		decoded, err := base64.StdEncoding.DecodeString(gjson.Get(log.Other, "expr_b64").String())
		require.NoError(t, err)
		return string(decoded), gjson.Get(log.Other, "request_rules.0.cond").String()
	}

	ordinary := &model.Log{Group: "default", Other: stored(expression, condition)}
	hideGroupsInLog(ordinary, []string{"night"})
	gotExpression, gotCondition := read(ordinary)
	assert.Equal(t, shown, gotExpression)
	assert.Equal(t, `hour("Asia/Shanghai") >= 9`, gotCondition)
	assert.NotContains(t, ordinary.Other, "night")
	assert.Equal(t, "base", gjson.Get(ordinary.Other, "matched_tier").String())
	assert.False(t, gjson.Get(ordinary.Other, "request_rules.0.matched").Bool())

	billedInNight := &model.Log{Group: "night", Other: stored(expression, condition)}
	hideGroupsInLog(billedInNight, []string{"night"})
	gotExpression, gotCondition = read(billedInNight)
	assert.Equal(t, expression, gotExpression, "a request billed in the group keeps the expression it was billed by")
	assert.Equal(t, condition, gotCondition)

	administrator := &model.Log{Group: "default", Other: stored(expression, condition)}
	hideGroupsInLog(administrator, nil)
	gotExpression, _ = read(administrator)
	assert.Equal(t, expression, gotExpression)

	const unclean = `tier("base", p * 1) * (group startsWith "night" ? 1 : 2)`
	withheld := &model.Log{Group: "default", Other: stored(unclean, `group startsWith "night"`)}
	hideGroupsInLog(withheld, []string{"night"})
	assert.False(t, gjson.Get(withheld.Other, "expr_b64").Exists())
	assert.False(t, gjson.Get(withheld.Other, "request_rules").Exists())
	assert.NotContains(t, withheld.Other, "night")
	assert.Equal(t, "tiered_expr", gjson.Get(withheld.Other, "billing_mode").String())

	// The trace keeps only the rules the shown expression still declares:
	// neither a rule decided by the hidden group nor one inside its branch.
	traced := func(expression, rules string) *model.Log {
		return &model.Log{Group: "default", Other: `{"billing_mode":"tiered_expr","expr_b64":"` + base64.StdEncoding.EncodeToString([]byte(expression)) + `","request_rules":` + rules + `}`}
	}
	const fastRule = `{"cond":"param(\"fast\") == true","multiplier":2,"matched":true}`
	for name, tc := range map[string]struct{ expression, rules, want string }{
		"rule decided by the hidden group": {
			`tier("base", p * 1) * (group == "night" ? 0.15 : 1) * (param("fast") == true ? 2 : 1)`,
			`[{"cond":"group == \"night\"","multiplier":0.15,"matched":false},` + fastRule + `]`,
			`[` + fastRule + `]`,
		},
		"rule that applied to every visible group": {
			`tier("base", p * 1) * (group != "night" ? 2 : 1)`,
			`[{"cond":"group != \"night\"","multiplier":2,"matched":true}]`,
			`[]`,
		},
		"rule inside the hidden branch": {
			`group == "night" ? tier("a", p * 1) * (param("slow") == true ? 3 : 1) : tier("b", p * 2) * (param("fast") == true ? 2 : 1)`,
			`[{"cond":"param(\"slow\") == true","multiplier":3,"matched":false},` + fastRule + `]`,
			`[` + fastRule + `]`,
		},
		"hidden-branch rule worded like a visible one": {
			`(group == "night" ? tier("a", p * 1) * (param("fast") == true ? 3 : 1) : tier("b", p * 2)) * (param("fast") == true ? 2 : (group == "night" ? 7 : 1))`,
			`[{"cond":"param(\"fast\") == true","multiplier":3,"matched":false},{"cond":"group == \"night\"","multiplier":7,"matched":false}]`,
			``,
		},
	} {
		log := traced(tc.expression, tc.rules)
		hideGroupsInLog(log, []string{"night"})
		if tc.want == "" {
			assert.False(t, gjson.Get(log.Other, "request_rules").Exists(), name)
		} else {
			assert.JSONEq(t, tc.want, gjson.Get(log.Other, "request_rules").Raw, name)
		}
		assert.NotContains(t, log.Other, "night", name)
	}

	unreadable := &model.Log{Group: "default", Other: `{"billing_mode":"tiered_expr","expr_b64":"!not base64!","request_rules":[` + fastRule + `]}`}
	hideGroupsInLog(unreadable, []string{"night"})
	assert.False(t, gjson.Get(unreadable.Other, "expr_b64").Exists())
	assert.False(t, gjson.Get(unreadable.Other, "request_rules").Exists())
}

func TestUserLogReadersDoNotSeeHiddenGroups(t *testing.T) {
	useHiddenNightGroup(t)
	special := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup
	previousSpecial := special.ReadAll()
	special.Set("partner", map[string]string{"+:night": "Night", "-:vip": ""})
	t.Cleanup(func() {
		special.Clear()
		special.AddAll(previousSpecial)
	})
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	require.NoError(t, db.Create(&[]model.User{
		{Id: 8, Username: "default-user", AffCode: "default-user", Password: "password", Group: "default", Status: common.UserStatusEnabled},
		{Id: 9, Username: "partner-user", AffCode: "partner-user", Password: "password", Group: "partner", Status: common.UserStatusEnabled},
	}).Error)
	expression := base64.StdEncoding.EncodeToString([]byte(`tier("base", p * 1) * (group != "night" && c > 0 ? 2 : 1)`))
	other := `{"billing_mode":"tiered_expr","expr_b64":"` + expression + `","request_rules":[{"cond":"group != \"night\" && c > 0","multiplier":2,"matched":true}]}`
	now := common.GetTimestamp()
	for _, userId := range []int{8, 9} {
		require.NoError(t, db.Create(&model.Log{UserId: userId, CreatedAt: now - 60, Type: model.LogTypeConsume, ModelName: "zz-model", Group: "default", TokenId: 50 + userId, Quota: 1, Other: other}).Error)
	}

	// namesNight reports whether what a log reader returns names the group,
	// in the rule conditions or in the encoded expression.
	storedExpression := regexp.MustCompile(`"expr_b64":"([A-Za-z0-9+/=]+)"`)
	namesNight := func(handler gin.HandlerFunc, userId, role, tokenId int) bool {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/log/self", nil)
		ctx.Set("id", userId)
		ctx.Set("role", role)
		ctx.Set("token_id", tokenId)
		handler(ctx)
		require.Equal(t, http.StatusOK, recorder.Code)
		body := strings.ReplaceAll(recorder.Body.String(), `\"`, `"`)
		encoded := storedExpression.FindStringSubmatch(body)
		require.NotNil(t, encoded, "the log and its expression must be in the response: %s", body)
		decoded, err := base64.StdEncoding.DecodeString(encoded[1])
		require.NoError(t, err)
		return strings.Contains(body+string(decoded), "night")
	}

	assert.False(t, namesNight(GetUserLogs, 8, common.RoleCommonUser, 0), "log list of an ordinary user")
	assert.False(t, namesNight(ExportUserLogs, 8, common.RoleCommonUser, 0), "log export of an ordinary user")
	assert.False(t, namesNight(GetLogByKey, 8, common.RoleAdminUser, 58), "a token key proves no role")
	assert.True(t, namesNight(GetUserLogs, 8, common.RoleAdminUser, 0), "administrators read the stored log")
	assert.True(t, namesNight(ExportUserLogs, 8, common.RoleAdminUser, 0), "administrators export the stored log")
	assert.True(t, namesNight(GetUserLogs, 9, common.RoleCommonUser, 0), "a user group granted the group sees it")

	special.Set("partner", map[string]string{"-:vip": ""})
	assert.False(t, namesNight(GetUserLogs, 9, common.RoleCommonUser, 0), "revoking the grant hides it again")
}
