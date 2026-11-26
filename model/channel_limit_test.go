package model

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func int64PtrForLimitTest(v int64) *int64 {
	return &v
}

// channelLimitTestId returns a channel id unique to this process run, so the
// exact-value counter assertions below never observe still-live in-memory
// windows left by a previous run of the test binary (go test -count=2).
var channelLimitTestIdSeq atomic.Int64

func channelLimitTestId(base int64) int {
	return int(base + channelLimitTestIdSeq.Add(1))
}

// seedChannelLimitCounters writes absolute window values for one channel using
// the same keys the production limit checks read.
func seedChannelLimitCounters(t *testing.T, channelId int, rpm int64, tpm int64, dayQuota int64, monthQuota int64) {
	t.Helper()
	require.False(t, common.RedisEnabled, "channel limit tests must run against the in-memory counter store")
	require.NoError(t, common.RollingCounterAdd(channelRpmKey(channelId), rpm))
	require.NoError(t, common.RollingCounterAdd(channelTpmKey(channelId), tpm))
	dayKey, monthKey := channelQuotaKeys(&Channel{Id: channelId}, time.Now())
	require.NoError(t, common.CounterQuotaMIncrBy([]common.CounterDelta{
		{Key: dayKey, Delta: dayQuota, TTL: time.Hour},
		{Key: monthKey, Delta: monthQuota, TTL: time.Hour},
	}))
}

// rollingSum reads one rolling counter through the production read path.
func rollingSum(key string) int64 {
	return common.RollingCounterSums([]string{key})[key]
}

// channelQuotaWindowValues reads the channel's current day and month budget
// windows through the production read path.
func channelQuotaWindowValues(ch *Channel, at time.Time) (int64, int64) {
	dayKey, monthKey := channelQuotaKeys(ch, at)
	values := common.CounterMGet([]string{dayKey, monthKey})
	return values[dayKey], values[monthKey]
}

func TestChannelWithinLimitValues(t *testing.T) {
	tests := []struct {
		name  string
		ch    *Channel
		rpm   int64
		tpm   int64
		day   int64
		month int64
		want  bool
	}{
		{
			name:  "channel without limits ignores every counter",
			ch:    &Channel{},
			rpm:   1 << 40,
			tpm:   1 << 40,
			day:   1 << 40,
			month: 1 << 40,
			want:  true,
		},
		{
			name: "rpm below the limit passes",
			ch:   &Channel{RpmLimit: int64PtrForLimitTest(5)},
			rpm:  4,
			want: true,
		},
		{
			name: "rpm at the limit blocks",
			ch:   &Channel{RpmLimit: int64PtrForLimitTest(5)},
			rpm:  5,
			want: false,
		},
		{
			name: "rpm past the limit blocks",
			ch:   &Channel{RpmLimit: int64PtrForLimitTest(5)},
			rpm:  6,
			want: false,
		},
		{
			name: "tpm at the limit blocks",
			ch:   &Channel{TpmLimit: int64PtrForLimitTest(100)},
			tpm:  100,
			want: false,
		},
		{
			name: "daily quota at the limit blocks",
			ch:   &Channel{DailyQuotaLimit: int64PtrForLimitTest(500)},
			day:  500,
			want: false,
		},
		{
			name:  "monthly quota at the limit blocks",
			ch:    &Channel{MonthlyQuotaLimit: int64PtrForLimitTest(500)},
			month: 500,
			want:  false,
		},
		{
			name: "a refund-heavy negative quota window leaves headroom",
			ch:   &Channel{DailyQuotaLimit: int64PtrForLimitTest(100)},
			day:  -50,
			want: true,
		},
		{
			name: "the first violated limit reports blocked",
			ch:   &Channel{RpmLimit: int64PtrForLimitTest(1), TpmLimit: int64PtrForLimitTest(1), DailyQuotaLimit: int64PtrForLimitTest(1)},
			rpm:  1,
			tpm:  1,
			day:  1,
			want: false,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want,
				channelWithinLimitValues(testCase.ch, testCase.rpm, testCase.tpm, testCase.day, testCase.month))
		})
	}
}

func TestChannelWithinLimitsReadsWindowCounters(t *testing.T) {
	assert.True(t, ChannelWithinLimits(nil), "a nil channel never blocks selection")

	ch := &Channel{Id: 950001, RpmLimit: int64PtrForLimitTest(5), DailyQuotaLimit: int64PtrForLimitTest(1000)}
	seedChannelLimitCounters(t, ch.Id, 4, 0, 999, 0)
	assert.True(t, ChannelWithinLimits(ch), "below both limits the channel stays selectable")

	seedChannelLimitCounters(t, ch.Id, 1, 0, 1, 0)
	assert.False(t, ChannelWithinLimits(ch), "the day window reaching its budget drops the channel")

	unlimited := &Channel{Id: 950002}
	seedChannelLimitCounters(t, unlimited.Id, 1<<40, 1<<40, 1<<40, 1<<40)
	assert.True(t, ChannelWithinLimits(unlimited), "channels without configured limits never consult counters")
}

func TestChannelRpmTryConsumeStrictAdmissionAndRollback(t *testing.T) {
	ch := &Channel{Id: channelLimitTestId(950100), RpmLimit: int64PtrForLimitTest(2)}
	assert.True(t, ChannelRpmTryConsume(ch))
	assert.True(t, ChannelRpmTryConsume(ch))
	assert.False(t, ChannelRpmTryConsume(ch), "the third dispatch within one minute must be rejected")
	assert.Equal(t, int64(2), rollingSum(channelRpmKey(ch.Id)),
		"the rejected attempt must not be counted, so the window is not self-saturating")

	unlimited := &Channel{Id: channelLimitTestId(950100)}
	assert.True(t, ChannelRpmTryConsume(unlimited))
	assert.Equal(t, int64(0), rollingSum(channelRpmKey(unlimited.Id)),
		"channels without a limit must not consume the counter")
}

func TestChannelWithinLimitsBatchSplitsLimitedAndUnlimited(t *testing.T) {
	limited := &Channel{Id: 950201, RpmLimit: int64PtrForLimitTest(1)}
	unlimited := &Channel{Id: 950202}
	seedChannelLimitCounters(t, limited.Id, 1, 0, 0, 0)

	allowed := ChannelWithinLimitsBatch([]*Channel{limited, unlimited, nil})
	assert.False(t, allowed[limited.Id], "the over-limit channel is rejected")
	assert.True(t, allowed[unlimited.Id], "the unlimited channel stays selectable")
	_, ok := allowed[999999]
	assert.False(t, ok, "nil channels must be skipped, not marked allowed")
}

func TestRecordChannelTokenUsageAccumulatesTpmWindow(t *testing.T) {
	channelId := channelLimitTestId(950300)
	RecordChannelTokenUsage(channelId, 120)
	RecordChannelTokenUsage(channelId, 80)
	assert.Equal(t, int64(200), rollingSum(channelTpmKey(channelId)))

	RecordChannelTokenUsage(channelId, 0)
	RecordChannelTokenUsage(channelId, -5)
	RecordChannelTokenUsage(0, 10)
	assert.Equal(t, int64(200), rollingSum(channelTpmKey(channelId)),
		"non-positive token counts and missing channels must not change the window")
}

func TestUpdateChannelUsedQuotaMirrorsQuotaWindows(t *testing.T) {
	truncateTables(t)
	channel := Channel{
		Id: channelLimitTestId(950400), Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		DailyQuotaLimit: int64PtrForLimitTest(1 << 40),
		Name:            "quota-mirror", Models: "mirror-model", Group: "default", Key: "test-key",
	}
	require.NoError(t, channel.Insert())

	UpdateChannelUsedQuota(channel.Id, 500, ChannelSpendNow(1))
	UpdateChannelUsedQuota(channel.Id, -200, ChannelSpendNow(1))

	day, month := channelQuotaWindowValues(&channel, time.Now())
	assert.Equal(t, int64(300), day)
	assert.Equal(t, int64(300), month)

	updated, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, int64(300), updated.UsedQuota, "the lifetime column keeps its own net total")

	// A refund larger than what the window holds clamps the windows at zero
	// instead of loosening the budget; the durable column keeps the exact net.
	UpdateChannelUsedQuota(channel.Id, -400, ChannelSpendNow(1))
	day, month = channelQuotaWindowValues(&channel, time.Now())
	assert.Equal(t, int64(0), day, "an over-refund clamps the day window at zero")
	assert.Equal(t, int64(0), month, "an over-refund clamps the month window at zero")

	updated, err = GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, int64(-100), updated.UsedQuota, "the lifetime column keeps its own net total")

	UpdateChannelUsedQuota(channel.Id, 0, ChannelSpendNow(1))
	day, _ = channelQuotaWindowValues(&channel, time.Now())
	assert.Equal(t, int64(0), day, "zero deltas record nothing")
}

func TestChannelBudgetCountsListPriceNotGroupPrice(t *testing.T) {
	truncateTables(t)
	channel := Channel{
		Id: channelLimitTestId(950700), Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		DailyQuotaLimit: int64PtrForLimitTest(1 << 40),
		Name:            "list-price", Models: "list-model", Group: "default", Key: "test-key",
	}
	require.NoError(t, channel.Insert())

	// A group charged at 3.5x pays 3500 quota for 1000 quota of list price;
	// a group discounted to 0.5x pays 500 for the same 1000.
	UpdateChannelUsedQuota(channel.Id, 3500, ChannelSpendNow(3.5))
	UpdateChannelUsedQuota(channel.Id, 500, ChannelSpendNow(0.5))
	day, month := channelQuotaWindowValues(&channel, time.Now())
	assert.Equal(t, int64(2000), day, "the budget counts list price, whatever the group paid")
	assert.Equal(t, int64(2000), month)

	// A free group (ratio 0) and a refund whose charge ratio is unknown leave
	// the budget as it is.
	UpdateChannelUsedQuota(channel.Id, 700, ChannelSpendNow(0))
	UpdateChannelUsedQuota(channel.Id, -700, ChannelSpend{})
	day, _ = channelQuotaWindowValues(&channel, time.Now())
	assert.Equal(t, int64(2000), day)

	updated, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, int64(4000), updated.UsedQuota, "used_quota still records what customers were charged")
}

func TestChannelBudgetRefundReturnsToTheChargeWindow(t *testing.T) {
	truncateTables(t)
	channel := Channel{
		Id: channelLimitTestId(950800), Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		DailyQuotaLimit: int64PtrForLimitTest(1 << 40),
		Name:            "charge-window", Models: "window-model", Group: "default", Key: "test-key",
	}
	require.NoError(t, channel.Insert())

	yesterday := time.Now().Add(-24 * time.Hour)
	UpdateChannelUsedQuota(channel.Id, 600, ChannelSpend{GroupRatio: 2, ChargedAt: yesterday})
	UpdateChannelUsedQuota(channel.Id, 400, ChannelSpendNow(2))

	// The task charged yesterday is refunded today: yesterday's window gives
	// the quota back, today's budget is untouched.
	UpdateChannelUsedQuota(channel.Id, -600, ChannelSpend{GroupRatio: 2, ChargedAt: yesterday})
	yesterdayDay, _ := channelQuotaWindowValues(&channel, yesterday)
	today, _ := channelQuotaWindowValues(&channel, time.Now())
	assert.Equal(t, int64(0), yesterdayDay)
	assert.Equal(t, int64(200), today, "a late refund must not reopen today's budget")
}

func TestChannelBudgetWindowsSurviveARestart(t *testing.T) {
	truncateTables(t)
	limited := Channel{
		Id: channelLimitTestId(951000), Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		MonthlyQuotaLimit: int64PtrForLimitTest(1 << 40),
		Name:              "durable", Models: "durable-model", Group: "default", Key: "test-key",
	}
	unlimited := Channel{
		Id: channelLimitTestId(951000), Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Name: "no-budget", Models: "durable-model", Group: "default", Key: "test-key",
	}
	require.NoError(t, limited.Insert())
	require.NoError(t, unlimited.Insert())

	UpdateChannelUsedQuota(limited.Id, 900, ChannelSpendNow(3))
	UpdateChannelUsedQuota(limited.Id, -1200, ChannelSpendNow(3)) // clamps at zero
	UpdateChannelUsedQuota(limited.Id, 600, ChannelSpendNow(3))
	UpdateChannelUsedQuota(unlimited.Id, 900, ChannelSpendNow(1))

	dayKey, monthKey := channelQuotaKeys(&limited, time.Now())
	var saved []ChannelQuotaWindow
	require.NoError(t, DB.Order("counter_key").Find(&saved).Error)
	require.Len(t, saved, 2, "only the channel with a budget keeps windows: %+v", saved)
	for _, row := range saved {
		assert.Contains(t, []string{dayKey, monthKey}, row.CounterKey)
		assert.Equal(t, int64(200), row.Quota, "the saved window clamps at zero like the live one")
		assert.Greater(t, row.ExpiresAt, time.Now().Unix())
	}
	_, unlimitedMonth := channelQuotaWindowValues(&unlimited, time.Now())
	assert.Zero(t, unlimitedMonth, "a channel without a budget keeps no window")

	// A restart starts with an empty memory store: simulate it by loading
	// stale values, then restore the saved windows over them.
	common.RestoreMemoryCounters([]common.CounterRestore{
		{Key: dayKey, Value: 0, ExpiresAt: time.Now().Add(time.Hour)},
		{Key: monthKey, Value: 0, ExpiresAt: time.Now().Add(time.Hour)},
	})
	expired := ChannelQuotaWindow{CounterKey: "chanQD:1:19990101", Quota: 5, ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	require.NoError(t, DB.Create(&expired).Error)

	RestoreChannelQuotaWindows()
	day, month := channelQuotaWindowValues(&limited, time.Now())
	assert.Equal(t, int64(200), day, "the day window comes back after a restart")
	assert.Equal(t, int64(200), month, "the month window comes back after a restart")
	var left int64
	require.NoError(t, DB.Model(&ChannelQuotaWindow{}).Where("counter_key = ?", expired.CounterKey).Count(&left).Error)
	assert.Zero(t, left, "expired windows are deleted at startup")
}

func TestChannelQuotaWindowsFollowResetTimeAndTimezone(t *testing.T) {
	losAngeles, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	ch := &Channel{
		Id:             950900,
		QuotaResetZone: common.GetPointer("America/Los_Angeles"),
		QuotaResetTime: common.GetPointer("09:30"),
	}
	cases := []struct {
		at    time.Time
		day   string
		month string
	}{
		{time.Date(2026, 3, 1, 9, 29, 0, 0, losAngeles), "20260228", "202602"},
		{time.Date(2026, 3, 1, 9, 30, 0, 0, losAngeles), "20260301", "202603"},
		// Daylight saving starts on 2026-03-08 in Los Angeles; the reset
		// stays at 09:30 local time.
		{time.Date(2026, 3, 8, 10, 0, 0, 0, losAngeles), "20260308", "202603"},
		{time.Date(2026, 3, 9, 9, 29, 0, 0, losAngeles), "20260308", "202603"},
		{time.Date(2026, 3, 9, 9, 30, 0, 0, losAngeles), "20260309", "202603"},
		// The same instant seen from another zone falls in the same window.
		{time.Date(2026, 3, 2, 1, 0, 0, 0, time.UTC), "20260301", "202603"},
	}
	for _, c := range cases {
		day, month := channelQuotaWindowLabels(ch, c.at)
		assert.Equal(t, c.day, day, c.at.String())
		assert.Equal(t, c.month, month, c.at.String())
	}

	defaults := &Channel{Id: 950901}
	localNoon := time.Date(2026, 5, 31, 12, 0, 0, 0, time.Local)
	day, month := channelQuotaWindowLabels(defaults, localNoon)
	assert.Equal(t, "20260531", day, "without settings the windows are server-local calendar days")
	assert.Equal(t, "202605", month)
}

func TestValidateQuotaReset(t *testing.T) {
	valid := []*Channel{
		{},
		{QuotaResetZone: common.GetPointer("UTC"), QuotaResetTime: common.GetPointer("00:00")},
		{QuotaResetZone: common.GetPointer("Asia/Shanghai"), QuotaResetTime: common.GetPointer("23:59")},
	}
	for _, ch := range valid {
		assert.NoError(t, ch.ValidateQuotaReset())
	}
	invalid := []*Channel{
		{QuotaResetZone: common.GetPointer("Mars/Olympus")},
		{QuotaResetTime: common.GetPointer("24:00")},
		{QuotaResetTime: common.GetPointer("9:30")},
		{QuotaResetTime: common.GetPointer("09:60")},
		{QuotaResetTime: common.GetPointer("0930")},
		// Stored values are used as is, so padding is refused rather than
		// trimmed (a padded time would not even fit the varchar(5) column).
		{QuotaResetTime: common.GetPointer(" 09:00")},
		{QuotaResetTime: common.GetPointer("+1:00")},
		{QuotaResetTime: common.GetPointer("-0:30")},
		{QuotaResetZone: common.GetPointer(" UTC")},
	}
	for _, ch := range invalid {
		assert.Error(t, ch.ValidateQuotaReset())
	}
}

func TestFilterCandidateIDsDropsOverLimitChannels(t *testing.T) {
	overRpm := &Channel{Id: 950501, RpmLimit: int64PtrForLimitTest(1)}
	overDaily := &Channel{Id: 950502, DailyQuotaLimit: int64PtrForLimitTest(10)}
	unlimited := &Channel{Id: 950503}
	seedChannelLimitCounters(t, 950501, 1, 0, 0, 0)
	seedChannelLimitCounters(t, 950502, 0, 0, 10, 0)

	channelSyncLock.Lock()
	previous := channelsIDM
	channelsIDM = map[int]*Channel{
		950501: overRpm,
		950502: overDaily,
		950503: unlimited,
	}
	t.Cleanup(func() {
		channelsIDM = previous
		channelSyncLock.Unlock()
	})

	limitsFilter := []dto.ChannelFilter{{Kind: dto.FilterChannelLimits}}

	kept, emptiedBy := filterCandidateIDs([]int{950501, 950502, 950503}, "mirror-model", limitsFilter)
	assert.Equal(t, []int{950503}, kept, "over-limit channels drop out while others stay")
	assert.Empty(t, emptiedBy)

	kept, emptiedBy = filterCandidateIDs([]int{950501, 950502}, "mirror-model", limitsFilter)
	assert.Empty(t, kept)
	assert.Equal(t, dto.FilterChannelLimits, emptiedBy, "an emptied set must attribute the cause to the limits filter")

	kept, emptiedBy = filterCandidateIDs([]int{950503, 999999}, "mirror-model", limitsFilter)
	assert.Equal(t, []int{950503}, kept, "ids missing from the cache are dropped like other non-path filters")
	assert.Empty(t, emptiedBy)
}

func TestGetRandomSatisfiedChannelReportsOverLimit(t *testing.T) {
	truncateTables(t)
	originalMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = originalMemoryCache; InitChannelCache() })

	rpm := int64(1)
	tpm := int64(10)
	limited := Channel{
		Id: 950601, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Name: "limited", Models: "limit-model", Group: "default", Key: "limited-key", RpmLimit: &rpm,
	}
	spare := Channel{
		Id: 950602, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Name: "spare", Models: "limit-model", Group: "default", Key: "spare-key", TpmLimit: &tpm,
	}
	require.NoError(t, limited.Insert())
	require.NoError(t, spare.Insert())
	InitChannelCache()

	limitsFilter := []dto.ChannelFilter{{Kind: dto.FilterChannelLimits}}
	seedChannelLimitCounters(t, limited.Id, 1, 0, 0, 0)
	seedChannelLimitCounters(t, spare.Id, 0, 0, 0, 0)

	selected, err := GetRandomSatisfiedChannel("default", "limit-model", 0, limitsFilter)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, "spare", selected.Name, "the over-limit channel drops out and the spare serves")

	seedChannelLimitCounters(t, spare.Id, 0, 10, 0, 0)
	selected, err = GetRandomSatisfiedChannel("default", "limit-model", 0, limitsFilter)
	assert.Nil(t, selected)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrChannelsOverLimit)

	// Without the filter both channels stay selectable: limiting is opt-in.
	selected, err = GetRandomSatisfiedChannel("default", "limit-model", 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
}

func TestGetChannelAttributesOverLimitInDatabaseMode(t *testing.T) {
	truncateTables(t)
	originalMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = originalMemoryCache })

	rpm := int64(1)
	channel := Channel{
		Id: 950701, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Name: "db-limited", Models: "db-model", Group: "default", Key: "db-key", RpmLimit: &rpm,
	}
	require.NoError(t, channel.Insert())
	seedChannelLimitCounters(t, channel.Id, 1, 0, 0, 0)

	limitsFilter := []dto.ChannelFilter{{Kind: dto.FilterChannelLimits}}
	selected, err := GetChannel("default", "db-model", 0, limitsFilter)
	assert.Nil(t, selected)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrChannelsOverLimit, "DB-mode selection attributes the empty result to channel limits")

	selected, err = GetChannel("default", "db-model", 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, "db-limited", selected.Name)
}

// Legacy schemas predate the channel rate/quota limit columns and the token
// rpm_limit column; migrating a representative old table must add them with
// defaults, keep existing rows, and issue no DDL on a second run.
type channelBeforeLimitColumns struct {
	ID        int    `gorm:"column:id;primaryKey"`
	Name      string `gorm:"column:name;size:64"`
	Key       string `gorm:"column:key;not null"`
	UsedQuota int64  `gorm:"column:used_quota;bigint;default:0"`
}

type tokenBeforeRpmLimit struct {
	ID   int    `gorm:"column:id;primaryKey"`
	Name string `gorm:"column:name;size:64"`
	Key  string `gorm:"column:key;type:varchar(128)"`
}

func testChannelLimitColumnsMigration(t *testing.T, db *gorm.DB, recorder *migrationSQLRecorder) {
	t.Helper()
	channelTable := "channel_limit_columns_migration_test"
	tokenTable := "token_rpm_limit_migration_test"
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(channelTable)
		_ = db.Migrator().DropTable(tokenTable)
	})

	require.NoError(t, db.Table(channelTable).AutoMigrate(&channelBeforeLimitColumns{}))
	require.NoError(t, db.Table(channelTable).Create(&channelBeforeLimitColumns{
		ID: 1, Name: "kept-channel", Key: "sk-legacy", UsedQuota: 77,
	}).Error)
	require.NoError(t, db.Table(tokenTable).AutoMigrate(&tokenBeforeRpmLimit{}))
	require.NoError(t, db.Table(tokenTable).Create(&tokenBeforeRpmLimit{
		ID: 1, Name: "kept-token", Key: "sk-legacy-token",
	}).Error)

	require.NoError(t, db.Table(channelTable).AutoMigrate(&Channel{}))
	require.NoError(t, db.Table(tokenTable).AutoMigrate(&Token{}))

	channelColumns := map[string]bool{}
	columnTypes, err := db.Table(channelTable).Migrator().ColumnTypes(&Channel{})
	require.NoError(t, err)
	for _, columnType := range columnTypes {
		channelColumns[strings.ToLower(columnType.Name())] = true
	}
	for _, column := range []string{"rpm_limit", "tpm_limit", "daily_quota_limit", "monthly_quota_limit", "quota_reset_timezone", "quota_reset_time"} {
		assert.True(t, channelColumns[column], "the upgraded channels table must gain %s", column)
	}

	tokenColumns := map[string]bool{}
	columnTypes, err = db.Table(tokenTable).Migrator().ColumnTypes(&Token{})
	require.NoError(t, err)
	for _, columnType := range columnTypes {
		tokenColumns[strings.ToLower(columnType.Name())] = true
	}
	assert.True(t, tokenColumns["rpm_limit"], "the upgraded tokens table must gain rpm_limit")

	var channelRow channelBeforeLimitColumns
	require.NoError(t, db.Table(channelTable).First(&channelRow, 1).Error)
	assert.Equal(t, "kept-channel", channelRow.Name)
	assert.Equal(t, int64(77), channelRow.UsedQuota, "existing channel rows must survive the migration")

	var tokenRow tokenBeforeRpmLimit
	require.NoError(t, db.Table(tokenTable).First(&tokenRow, 1).Error)
	assert.Equal(t, "kept-token", tokenRow.Name, "existing token rows must survive the migration")

	recorder.reset()
	require.NoError(t, db.Table(channelTable).AutoMigrate(&Channel{}))
	require.NoError(t, db.Table(tokenTable).AutoMigrate(&Token{}))
	assert.Empty(t, recorder.schemaMutations(), "a second migration must be a no-op")
}

func TestChannelAndTokenLimitColumnsMigrationSQLite(t *testing.T) {
	recorder := &migrationSQLRecorder{}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: recorder})
	require.NoError(t, err)
	testChannelLimitColumnsMigration(t, db, recorder)
}

func TestChannelAndTokenLimitColumnsMigrationConfiguredDatabases(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		dialector func(string) gorm.Dialector
	}{
		{name: "mysql", env: "TEST_MYSQL_DSN", dialector: func(dsn string) gorm.Dialector { return mysql.Open(dsn) }},
		{name: "postgres", env: "TEST_POSTGRES_DSN", dialector: func(dsn string) gorm.Dialector {
			return postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skip(test.env + " is not configured")
			}
			recorder := &migrationSQLRecorder{}
			db, err := gorm.Open(test.dialector(dsn), &gorm.Config{Logger: recorder})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			testChannelLimitColumnsMigration(t, db, recorder)
		})
	}
}

// The budget window upsert uses dialect-sensitive SQL (ON CONFLICT / ON
// DUPLICATE KEY with a table-qualified CASE), so it is checked on every
// configured database, not only on SQLite.
func TestChannelQuotaWindowUpsertConfiguredDatabases(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		dialector func(string) gorm.Dialector
	}{
		{name: "mysql", env: "TEST_MYSQL_DSN", dialector: func(dsn string) gorm.Dialector { return mysql.Open(dsn) }},
		{name: "postgres", env: "TEST_POSTGRES_DSN", dialector: func(dsn string) gorm.Dialector {
			return postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skip(test.env + " is not configured")
			}
			db, err := gorm.Open(test.dialector(dsn), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			require.NoError(t, db.Migrator().DropTable(&ChannelQuotaWindow{}))
			require.NoError(t, db.AutoMigrate(&ChannelQuotaWindow{}))
			previous := DB
			DB = db
			t.Cleanup(func() {
				DB = previous
				_ = db.Migrator().DropTable(&ChannelQuotaWindow{})
				_ = sqlDB.Close()
			})

			key := "chanQM:1:202611"
			require.NoError(t, persistChannelQuotaDeltas([]common.CounterDelta{{Key: key, Delta: 300, TTL: time.Hour}}))
			require.NoError(t, persistChannelQuotaDeltas([]common.CounterDelta{{Key: key, Delta: 200, TTL: time.Hour}}))
			require.NoError(t, persistChannelQuotaDeltas([]common.CounterDelta{{Key: key, Delta: -800, TTL: time.Hour}}))
			require.NoError(t, persistChannelQuotaDeltas([]common.CounterDelta{{Key: key, Delta: 50, TTL: time.Hour}}))
			fresh := "chanQD:1:20261126"
			require.NoError(t, persistChannelQuotaDeltas([]common.CounterDelta{{Key: fresh, Delta: -40, TTL: time.Hour}}))

			var rows []ChannelQuotaWindow
			require.NoError(t, db.Order("counter_key").Find(&rows).Error)
			require.Len(t, rows, 2)
			assert.Equal(t, fresh, rows[0].CounterKey)
			assert.Equal(t, int64(0), rows[0].Quota, "a refund on an empty window saves zero")
			assert.Equal(t, key, rows[1].CounterKey)
			assert.Equal(t, int64(50), rows[1].Quota, "500 - 800 clamps at zero, then + 50")
		})
	}
}
