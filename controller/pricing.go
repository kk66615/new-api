package controller

import (
	"maps"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

// filterPricingByUsableGroups keeps the models the viewer can use. Groups the
// viewer cannot use are hidden from each model, by name and in its expression,
// unless showHiddenGroups is set (administrators).
func filterPricingByUsableGroups(pricing []model.Pricing, usableGroup map[string]string, showHiddenGroups bool) []model.Pricing {
	if len(pricing) == 0 {
		return pricing
	}
	if len(usableGroup) == 0 {
		return []model.Pricing{}
	}

	filtered := make([]model.Pricing, 0, len(pricing))
	for _, item := range pricing {
		// item is a copy of the shared pricing cache entry; assign a fresh
		// slice so the cached EnableGroup is never mutated across requests.
		usableEnableGroups := make([]string, 0, len(item.EnableGroup))
		for _, group := range item.EnableGroup {
			if _, ok := usableGroup[group]; ok || group == "all" {
				usableEnableGroups = append(usableEnableGroups, group)
			}
		}
		if len(usableEnableGroups) == 0 {
			continue
		}
		if !showHiddenGroups {
			item.EnableGroup = usableEnableGroups
		}
		filtered = append(filtered, item)
	}
	if showHiddenGroups {
		return filtered
	}
	// A group condition in an expression names its group, so show each
	// expression as it applies to the groups this viewer can use.
	hiddenGroups := hiddenGroupsOutside(usableGroup)
	if len(hiddenGroups) == 0 {
		return filtered
	}
	for i := range filtered {
		// An expression that cannot be shown without a hidden name is withheld.
		filtered[i].BillingExpr, _ = billingexpr.HideGroups(filtered[i].BillingExpr, hiddenGroups)
		if len(filtered[i].BillingPluginVariants) == 0 {
			continue
		}
		variants := slices.Clone(filtered[i].BillingPluginVariants)
		for j := range variants {
			variants[j].BillingExpr, _ = billingexpr.HideGroups(variants[j].BillingExpr, hiddenGroups)
		}
		filtered[i].BillingPluginVariants = variants
	}
	return filtered
}

func GetPricing(c *gin.Context) {
	pricing := model.GetPricing()
	userId, exists := c.Get("id")
	usableGroup := map[string]string{}
	groupRatio := map[string]float64{}
	maps.Copy(groupRatio, ratio_setting.GetGroupRatioCopy())
	var group string
	if exists {
		user, err := model.GetUserCache(userId.(int))
		if err == nil {
			group = user.Group
			for g := range groupRatio {
				ratio, ok := ratio_setting.GetGroupGroupRatio(group, g)
				if ok {
					groupRatio[g] = ratio
				}
			}
		}
	}

	usableGroup = service.GetUserUsableGroups(group)
	pricing = filterPricingByUsableGroups(pricing, usableGroup, requesterSeesAllGroups(c))
	// check groupRatio contains usableGroup
	for group := range ratio_setting.GetGroupRatioCopy() {
		if _, ok := usableGroup[group]; !ok {
			delete(groupRatio, group)
		}
	}

	c.JSON(200, gin.H{
		"success":            true,
		"data":               pricing,
		"vendors":            model.GetVendors(),
		"group_ratio":        groupRatio,
		"usable_group":       usableGroup,
		"supported_endpoint": model.GetSupportedEndpointMap(),
		"auto_groups":        service.GetUserAutoGroup(group),
		"pricing_version":    "a42d372ccf0b5dd13ecf71203521f9d2",
	})
}

func ResetModelRatio(c *gin.Context) {
	defaultStr := ratio_setting.DefaultModelRatio2JSONString()
	err := model.UpdateOption("ModelRatio", defaultStr)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	err = ratio_setting.UpdateModelRatioByJSONString(defaultStr)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccessT(c, "Model ratios reset", nil)
}
