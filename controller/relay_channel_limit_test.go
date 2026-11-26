package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	relay "github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// relayRpmTestId returns a channel id unique to this process run so the
// in-process RPM window (keyed by channel id) never observes values left by
// a previous run of the test binary (go test -count=2).
var relayRpmTestIdSeq atomic.Int64

func relayRpmTestId() int {
	return int(960700 + relayRpmTestIdSeq.Add(1))
}

// channelLimitTestChannel describes one channel of a limit test fixture.
type channelLimitTestChannel struct {
	id       int
	priority int64
	rpm      int64
}

// useChannelLimitTestDB swaps in an in-memory database and memory channel
// cache holding the given channels for modelName in group "default".
func useChannelLimitTestDB(t *testing.T, modelName string, channels []channelLimitTestChannel) {
	t.Helper()
	require.NoError(t, i18n.Init())
	originalDB := model.DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB = db
	common.MemoryCacheEnabled = true
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	t.Cleanup(func() {
		model.DB = originalDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
		if originalMemoryCacheEnabled && originalDB != nil &&
			originalDB.Migrator().HasTable(&model.Channel{}) && originalDB.Migrator().HasTable(&model.Ability{}) {
			model.InitChannelCache()
		}
		sqlDB, err := db.DB()
		if err == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	weight := uint(100)
	for _, ch := range channels {
		priority, rpm := ch.priority, ch.rpm
		row := &model.Channel{
			Id: ch.id, Type: constant.ChannelTypeOpenAI, Key: fmt.Sprintf("sk-%d", ch.id),
			Status: common.ChannelStatusEnabled, Name: fmt.Sprintf("limit-test-%d", ch.id),
			Weight: &weight, Models: modelName, Group: "default", Priority: &priority,
		}
		if rpm > 0 {
			row.RpmLimit = &rpm
		}
		require.NoError(t, db.Create(row).Error)
		require.NoError(t, db.Create(&model.Ability{
			Group: "default", Model: modelName, ChannelId: ch.id,
			Enabled: true, Priority: &priority, Weight: weight,
		}).Error)
	}
	model.InitChannelCache()
}

// channelLimitTestRequest builds a relay request context as the distributor
// leaves it after choosing distributorId.
func channelLimitTestRequest(modelName string, distributorId int) (*gin.Context, *relaycommon.RelayInfo, *service.RetryParam) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelId, distributorId)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelName, fmt.Sprintf("limit-test-%d", distributorId))
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	c.Set("auto_ban", false)
	// The distributor registers these filters for every request, including
	// retries.
	constraints := service.GetChannelConstraints(c)
	constraints.AddFilter(taskdto.ChannelFilter{Kind: taskdto.FilterRequestPath, RequestPath: "/v1/chat/completions"})
	constraints.AddFilter(taskdto.ChannelFilter{Kind: taskdto.FilterChannelLimits})

	info := &relaycommon.RelayInfo{OriginModelName: modelName, UsingGroup: "default"}
	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  "default",
		ModelName:   modelName,
		RequestPath: "/v1/chat/completions",
		Retry:       common.GetPointer(0),
	}
	return c, info, retryParam
}

// TestRpmRejectionMovesToTheSameTierWithoutUsingARetry covers the relay loop
// after the strict RPM admission refuses the distributor-selected channel:
// the request moves on to the other channel of the same priority tier (not
// the lower tier), and the retry counter is not advanced.
func TestRpmRejectionMovesToTheSameTierWithoutUsingARetry(t *testing.T) {
	const modelName = "rpm-rejection-model"
	distributorId, sameTierId, lowerTierId := relayRpmTestId(), relayRpmTestId(), relayRpmTestId()
	useChannelLimitTestDB(t, modelName, []channelLimitTestChannel{
		{id: distributorId, priority: 0, rpm: 1},
		{id: sameTierId, priority: 0},
		{id: lowerTierId, priority: -1},
	})
	rpm := int64(1)
	// Saturate the distributor channel's window (limit 1).
	require.True(t, model.ChannelRpmTryConsume(&model.Channel{Id: distributorId, RpmLimit: &rpm}))

	c, info, retryParam := channelLimitTestRequest(modelName, distributorId)
	first, firstErr := getChannel(c, info, retryParam)
	require.Nil(t, firstErr)
	require.Equal(t, distributorId, first.Id, "the first attempt reuses the distributor-selected channel")
	require.False(t, model.ChannelRpmTryConsume(first), "the saturated channel must refuse the dispatch")

	// What the relay loop does with the refusal, then its post statement.
	rejection := service.ChannelAtCapacityError(c)
	require.True(t, service.SkipChannelAtCapacity(c, first.Id, rejection, retryParam))
	retryParam.IncreaseRetry()
	assert.Equal(t, 0, retryParam.GetRetry(), "a capacity refusal must not use up a retry")

	second, secondErr := getChannel(c, info, retryParam)
	require.Nil(t, secondErr)
	assert.Equal(t, sameTierId, second.Id, "the next candidate comes from the same priority tier")
	assert.Equal(t, []string{fmt.Sprintf("%d", distributorId)}, c.GetStringSlice("use_channel"))

	assert.Equal(t, http.StatusTooManyRequests, rejection.StatusCode)
	assert.Equal(t, model.ChannelLimitExceededCode, rejection.GetErrorCode())
	assert.NotContains(t, rejection.Error(), fmt.Sprintf("%d", distributorId), "the client-facing message must not name the channel")
}

// TestRpmRejectionIgnoresTheRetryBudget: with retries disabled a channel at
// its limit still hands the request on, because the refusal never reached an
// upstream.
func TestRpmRejectionIgnoresTheRetryBudget(t *testing.T) {
	originalRetryTimes := common.RetryTimes
	common.RetryTimes = 0
	t.Cleanup(func() { common.RetryTimes = originalRetryTimes })

	c, _, retryParam := channelLimitTestRequest("budget-model", relayRpmTestId())
	assert.True(t, service.SkipChannelAtCapacity(c, 1, service.ChannelAtCapacityError(c), retryParam))
}

// TestRpmRejectionStaysOnAPinnedChannel: a pinned request stops with the
// refusal instead of leaking to another channel.
func TestRpmRejectionStaysOnAPinnedChannel(t *testing.T) {
	c, _, retryParam := channelLimitTestRequest("pinned-model", relayRpmTestId())
	service.GetChannelConstraints(c).AddPin(taskdto.ChannelPin{ChannelId: 7, Source: taskdto.PinSourceToken, RetryMode: taskdto.PinRetrySingleAttempt})
	assert.False(t, service.SkipChannelAtCapacity(c, 7, service.ChannelAtCapacityError(c), retryParam))
	assert.Empty(t, c.GetStringSlice("use_channel"))
}

// TestGetChannelAnswers429WhenEveryCandidateIsAtItsLimit: when the retry
// selection finds only channels at their limits, the request ends with 429
// channel_limit_exceeded like the distributor's answer, not 500.
func TestGetChannelAnswers429WhenEveryCandidateIsAtItsLimit(t *testing.T) {
	const modelName = "all-full-model"
	firstId, secondId := relayRpmTestId(), relayRpmTestId()
	useChannelLimitTestDB(t, modelName, []channelLimitTestChannel{
		{id: firstId, priority: 0, rpm: 1},
		{id: secondId, priority: 0, rpm: 1},
	})
	rpm := int64(1)
	for _, id := range []int{firstId, secondId} {
		require.True(t, model.ChannelRpmTryConsume(&model.Channel{Id: id, RpmLimit: &rpm}))
	}

	c, info, retryParam := channelLimitTestRequest(modelName, firstId)
	service.AppendUsedChannel(c, firstId)
	channel, channelErr := getChannel(c, info, retryParam)
	assert.Nil(t, channel)
	require.NotNil(t, channelErr)
	assert.Equal(t, http.StatusTooManyRequests, channelErr.StatusCode)
	assert.Equal(t, model.ChannelLimitExceededCode, channelErr.GetErrorCode())
}

// TestPinnedChannelOverItsLimitAnswers429: a request pinned to a channel that
// its own limits rule out gets 429 channel_limit_exceeded, not 400 "no
// available channel".
func TestPinnedChannelOverItsLimitAnswers429(t *testing.T) {
	const modelName = "pinned-full-model"
	pinnedId := relayRpmTestId()
	useChannelLimitTestDB(t, modelName, []channelLimitTestChannel{{id: pinnedId, priority: 0, rpm: 1}})
	rpm := int64(1)
	require.True(t, model.ChannelRpmTryConsume(&model.Channel{Id: pinnedId, RpmLimit: &rpm}))

	c, _, retryParam := channelLimitTestRequest(modelName, pinnedId)
	service.GetChannelConstraints(c).AddPin(taskdto.ChannelPin{ChannelId: pinnedId, Source: taskdto.PinSourceToken})
	channel, _, selectErr := service.SelectChannelForRequest(c, modelName, retryParam)
	assert.Nil(t, channel)
	require.NotNil(t, selectErr)
	assert.Equal(t, http.StatusTooManyRequests, selectErr.StatusCode)
	assert.Equal(t, model.ChannelLimitExceededCode, selectErr.Code)
}

// TestExecuteTaskSubmissionStopsOnLockedChannelRpmRejection pins the
// locked-channel contract of the strict RPM admission: the rejection is
// deterministic for the whole retry budget, so the submission stops with the
// 429 limit error instead of re-running setup against the same channel and
// burning every retry attempt.
func TestExecuteTaskSubmissionStopsOnLockedChannelRpmRejection(t *testing.T) {
	originalRetryTimes := common.RetryTimes
	common.RetryTimes = 3
	t.Cleanup(func() { common.RetryTimes = originalRetryTimes })

	rpm := int64(1)
	lockedId := relayRpmTestId()
	// Saturate the locked channel's per-minute window before dispatch.
	require.True(t, model.ChannelRpmTryConsume(&model.Channel{Id: lockedId, RpmLimit: &rpm}))

	events := make([]string, 0, 3)
	billing := &taskSubmissionTestBilling{events: &events}
	c := taskSubmissionTestContext()
	info := taskSubmissionRelayInfo(billing)
	info.TaskRelayInfo.LockedChannel = &model.Channel{
		Id: lockedId, Type: constant.ChannelTypeTaskPlugin, Name: "locked-rpm", RpmLimit: &rpm,
	}
	info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: lockedId, ChannelType: constant.ChannelTypeTaskPlugin}

	submits := 0
	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *taskdto.TaskError) {
		submits++
		return nil, nil
	})

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, string(model.ChannelLimitExceededCode), taskErr.Code)
	assert.Zero(t, submits, "the deterministic RPM rejection must not reach the submit adaptor")
	assert.Equal(t, []string{"refund"}, events, "the undelivered submission refunds its billing")
	assert.Equal(t, 1, service.RequestPolicy(c).Attempts, "the rejection stops after the first dispatch attempt")
}
