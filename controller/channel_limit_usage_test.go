package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetChannelLimitUsageAnswersForLimitedChannelsOnly(t *testing.T) {
	const modelName = "limit-usage-model"
	limitedId, unlimitedId := relayRpmTestId(), relayRpmTestId()
	useChannelLimitTestDB(t, modelName, []channelLimitTestChannel{
		{id: limitedId, priority: 0, rpm: 3},
		{id: unlimitedId, priority: 0},
	})
	rpm := int64(3)
	for range 2 {
		require.True(t, model.ChannelRpmTryConsume(&model.Channel{Id: limitedId, RpmLimit: &rpm}))
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	// Unknown, malformed, repeated and unlimited ids are all left out.
	c.Request = httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/channel/limit_usage?ids=%d,%d,%d,abc,-4,99999999", limitedId, unlimitedId, limitedId), nil)
	GetChannelLimitUsage(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var body struct {
		Success bool                      `json:"success"`
		Data    []model.ChannelLimitUsage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.True(t, body.Success)
	require.Len(t, body.Data, 1)
	assert.Equal(t, limitedId, body.Data[0].ChannelId)
	require.NotNil(t, body.Data[0].Rpm)
	assert.Equal(t, model.ChannelLimitUsageItem{Used: 2, Limit: 3}, *body.Data[0].Rpm)
	assert.Nil(t, body.Data[0].Daily)

	// Only the first MaxChannelLimitUsageIds entries are read; the list asks
	// in batches of that size.
	padding := strings.Repeat("99999998,", MaxChannelLimitUsageIds)
	for ids, want := range map[string]int{
		padding + strconv.Itoa(limitedId):                                0,
		strconv.Itoa(limitedId) + "," + strings.TrimSuffix(padding, ","): 1,
	} {
		capped := httptest.NewRecorder()
		c, _ = gin.CreateTestContext(capped)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/limit_usage?ids="+ids, nil)
		GetChannelLimitUsage(c)
		var cappedBody struct {
			Data []model.ChannelLimitUsage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(capped.Body.Bytes(), &cappedBody))
		assert.Len(t, cappedBody.Data, want)
	}

	empty := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(empty)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/limit_usage", nil)
	GetChannelLimitUsage(c)
	assert.JSONEq(t, `{"success":true,"message":"","data":[]}`, empty.Body.String())
}
