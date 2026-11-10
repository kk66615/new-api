package controller

import (
	"encoding/base64"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// A billing group a user cannot select is hidden from that user: its name,
// its performance figures and the conditions written for it in billing
// expressions. Administrators see every group. Visibility is computed from
// the current group settings on each request, so granting or revoking a group
// takes effect without further steps.

func requesterSeesAllGroups(c *gin.Context) bool {
	return c.GetInt("role") >= common.RoleAdminUser
}

// requesterUsableGroups returns the billing groups the requester can select;
// an anonymous requester gets the groups offered to everyone.
func requesterUsableGroups(c *gin.Context) map[string]string {
	var userGroup string
	if userId := c.GetInt("id"); userId != 0 {
		if user, err := model.GetUserCache(userId); err == nil {
			userGroup = user.Group
		}
	}
	return service.GetUserUsableGroups(userGroup)
}

// requesterHiddenGroups lists the billing groups the requester must not see.
func requesterHiddenGroups(c *gin.Context) []string {
	if requesterSeesAllGroups(c) {
		return nil
	}
	return hiddenGroupsOutside(requesterUsableGroups(c))
}

func hiddenGroupsOutside(usableGroup map[string]string) []string {
	var hidden []string
	for group := range ratio_setting.GetGroupRatioCopy() {
		if _, usable := usableGroup[group]; !usable {
			hidden = append(hidden, group)
		}
	}
	return hidden
}

// requesterPerfGroups lists the groups whose performance samples the
// requester may see.
func requesterPerfGroups(c *gin.Context) []string {
	groups := []string{"auto"}
	if requesterSeesAllGroups(c) {
		for group := range ratio_setting.GetGroupRatioCopy() {
			groups = append(groups, group)
		}
		return groups
	}
	for group := range requesterUsableGroups(c) {
		if ratio_setting.ContainsGroupRatio(group) {
			groups = append(groups, group)
		}
	}
	return groups
}

// hideGroupsInLog rewrites the billing expression and request rules stored
// with a consume log for a reader who must not see hiddenGroups. The group the
// request itself was billed in is already on the log and stays. The rule trace
// keeps only the rules the shown expression still declares; details that
// cannot be shown without a hidden name are removed. The charge is untouched.
func hideGroupsInLog(log *model.Log, hiddenGroups []string) {
	encoded := gjson.Get(log.Other, "expr_b64")
	if len(hiddenGroups) == 0 || !encoded.Exists() {
		return
	}
	hidden := slices.DeleteFunc(slices.Clone(hiddenGroups), func(group string) bool { return group == log.Group })
	expression, err := base64.StdEncoding.DecodeString(encoded.String())
	shown, ok := "", false
	if err == nil {
		shown, ok = billingexpr.HideGroups(string(expression), hidden)
	}
	if ok && shown == string(expression) {
		return
	}
	rules := gjson.Get(log.Other, "request_rules").Array()
	other, _ := sjson.Delete(log.Other, "request_rules")
	if !ok {
		log.Other, _ = sjson.Delete(other, "expr_b64")
		return
	}
	other, _ = sjson.Set(other, "expr_b64", base64.StdEncoding.EncodeToString([]byte(shown)))
	log.Other = other

	// Without a trace the reader falls back to the rules of the expression,
	// so a trace that does not line up with it is simply left out.
	declared, err := billingexpr.RequestRules(shown)
	if err != nil {
		return
	}
	kept := make([]string, 0, len(declared))
	for _, rule := range rules {
		condition, clean := billingexpr.HideGroups(rule.Get("cond").String(), hidden)
		if !clean {
			return
		}
		if !slices.ContainsFunc(declared, func(d billingexpr.RequestRuleTrace) bool { return d.Cond == condition }) {
			continue
		}
		// The next declared rule, condition and multiplier, or no trace at all.
		if len(kept) == len(declared) || condition != declared[len(kept)].Cond || rule.Get("multiplier").Float() != declared[len(kept)].Multiplier {
			return
		}
		raw, _ := sjson.Set(rule.Raw, "cond", condition)
		kept = append(kept, raw)
	}
	if len(kept) == len(declared) {
		log.Other, _ = sjson.SetRaw(other, "request_rules", "["+strings.Join(kept, ",")+"]")
	}
}
