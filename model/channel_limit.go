package model

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// Channel-level rate and quota limiting (issue #7070). Limits are opt-in per
// channel: zero or nil means unlimited, so channels without any configured
// limit take a pure in-memory fast path and never touch the counter store.
//
//   - RPM counts every upstream dispatch (one per retry attempt, mirroring the
//     pressure actually put on the upstream) over the last 60 seconds.
//   - TPM sums the settled prompt+completion tokens of consume-log records
//     over the last 60 seconds; it is checked at channel selection because
//     the request's token count is unknown up front.
//   - Daily/monthly quota limits are budgets at list price: every used_quota
//     delta is divided by the group ratio it was charged at, so a group's
//     markup or discount does not move how much of the upstream a channel
//     may use. The windows start at the channel's reset time in its reset
//     timezone (default midnight, server local time). Refunds and later
//     settlements go back to the window of the original charge; a window
//     never drops below zero, while the durable channels.used_quota column
//     keeps the exact net total.

// ErrChannelsOverLimit reports that a group/model had enabled candidate
// channels but every one of them is currently over its RPM, TPM, or calendar
// quota limit. Callers surface it as HTTP 429 instead of the generic
// "no available channel" 503.
var ErrChannelsOverLimit = errors.New("all candidate channels are over their rate or quota limits")

// ChannelLimitExceededCode is the client-facing error code shared by the
// selection-side 429 and the dispatch-side RPM admission rejection. It
// deliberately avoids the "channel:" prefix of types.IsChannelError so
// ProcessChannelError never treats an over-limit attempt as a channel fault
// and auto-bans a temporarily limited channel.
const ChannelLimitExceededCode types.ErrorCode = "channel_limit_exceeded"

const (
	channelRpmCounterPrefix = "chanRPM"
	channelTpmCounterPrefix = "chanTPM"
	channelDayQuotaPrefix   = "chanQD"
	channelMonthQuotaPrefix = "chanQM"
	dailyQuotaWindowTTL     = 48 * time.Hour
	monthlyQuotaWindowTTL   = 45 * 24 * time.Hour
	dayWindowBucketLayout   = "20060102"
	monthWindowBucketLayout = "200601"
)

// ChannelSpend describes how a used_quota delta was charged. GroupRatio is the
// ratio applied to the request; the budget windows count quota / GroupRatio.
// A ratio <= 0 (a free group, or a refund whose charge ratio was not kept)
// leaves the budget unchanged: nothing can be derived from a zero charge, and
// skipping an unknown refund only keeps the budget tighter. ChargedAt places
// the delta in the window of the original charge; zero means now.
type ChannelSpend struct {
	GroupRatio float64
	ChargedAt  time.Time
}

// ChannelSpendNow is the spend of a charge made right now at groupRatio.
func ChannelSpendNow(groupRatio float64) ChannelSpend {
	return ChannelSpend{GroupRatio: groupRatio, ChargedAt: time.Now()}
}

func channelRpmKey(channelId int) string {
	return common.RollingCounterKey(channelRpmCounterPrefix, channelId)
}

func channelTpmKey(channelId int) string {
	return common.RollingCounterKey(channelTpmCounterPrefix, channelId)
}

var quotaResetLocations sync.Map // IANA name -> *time.Location

// ParseQuotaResetTimezone resolves a reset timezone; empty means server local
// time. The value is used exactly as stored, so surrounding spaces are an
// error rather than silently trimmed.
func ParseQuotaResetTimezone(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	if cached, ok := quotaResetLocations.Load(name); ok {
		return cached.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	quotaResetLocations.Store(name, loc)
	return loc, nil
}

// ParseQuotaResetTime parses an "HH:MM" reset time into the offset from
// midnight; empty means midnight.
func ParseQuotaResetTime(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	hours, minutes, ok := strings.Cut(value, ":")
	if !ok || len(hours) != 2 || len(minutes) != 2 || !allDigits(hours) || !allDigits(minutes) {
		return 0, fmt.Errorf("reset time %q must look like HH:MM", value)
	}
	h, hErr := strconv.Atoi(hours)
	m, mErr := strconv.Atoi(minutes)
	if hErr != nil || mErr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("reset time %q must look like HH:MM", value)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

func allDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ValidateQuotaReset rejects a reset timezone or time the windows cannot use.
func (channel *Channel) ValidateQuotaReset() error {
	if _, err := ParseQuotaResetTimezone(channel.GetQuotaResetTimezone()); err != nil {
		return fmt.Errorf("unknown quota reset timezone %q", channel.GetQuotaResetTimezone())
	}
	if _, err := ParseQuotaResetTime(channel.GetQuotaResetTime()); err != nil {
		return err
	}
	return nil
}

// channelQuotaWindowLabels returns the day and month labels of the budget
// windows containing at. A window labelled D runs from D at the reset time to
// the next day at the reset time, in the reset timezone; months likewise
// start on the 1st at the reset time. A setting that no longer parses falls
// back to midnight server time rather than dropping the charge.
func channelQuotaWindowLabels(ch *Channel, at time.Time) (string, string) {
	loc, err := ParseQuotaResetTimezone(ch.GetQuotaResetTimezone())
	if err != nil {
		loc = time.Local
	}
	offset, err := ParseQuotaResetTime(ch.GetQuotaResetTime())
	if err != nil {
		offset = 0
	}
	// Compare wall clocks, not elapsed time, so a daylight saving change
	// never moves the reset away from its local HH:MM.
	local := at.In(loc)
	windowDay := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	resetToday := time.Date(local.Year(), local.Month(), local.Day(),
		int(offset/time.Hour), int(offset%time.Hour/time.Minute), 0, 0, loc)
	if local.Before(resetToday) {
		windowDay = windowDay.AddDate(0, 0, -1)
	}
	return windowDay.Format(dayWindowBucketLayout), windowDay.Format(monthWindowBucketLayout)
}

func channelQuotaKeys(ch *Channel, at time.Time) (string, string) {
	day, month := channelQuotaWindowLabels(ch, at)
	return common.CounterKey(channelDayQuotaPrefix, ch.Id, day),
		common.CounterKey(channelMonthQuotaPrefix, ch.Id, month)
}

// channelHasAnyLimit reports whether the channel has at least one limit
// configured and therefore needs counter lookups before being selected.
func channelHasAnyLimit(ch *Channel) bool {
	return ch.GetRpmLimit() > 0 || ch.GetTpmLimit() > 0 ||
		ch.GetDailyQuotaLimit() > 0 || ch.GetMonthlyQuotaLimit() > 0
}

// channelWithinLimitValues reports whether the channel stays within every
// configured limit given the current window counters. The comparison is
// "counter >= limit": a window at or past its budget no longer admits.
func channelWithinLimitValues(ch *Channel, rpm int64, tpm int64, dayQuota int64, monthQuota int64) bool {
	if ch.GetRpmLimit() > 0 && rpm >= ch.GetRpmLimit() {
		return false
	}
	if ch.GetTpmLimit() > 0 && tpm >= ch.GetTpmLimit() {
		return false
	}
	if ch.GetDailyQuotaLimit() > 0 && dayQuota >= ch.GetDailyQuotaLimit() {
		return false
	}
	if ch.GetMonthlyQuotaLimit() > 0 && monthQuota >= ch.GetMonthlyQuotaLimit() {
		return false
	}
	return true
}

// ChannelWithinLimits reports whether ch may still serve a new request under
// its RPM, TPM, and calendar quota limits. Counter read failures fail open
// (the channel stays selectable): RPM is still enforced per dispatch by
// ChannelRpmTryConsume, and a broken counter store must not silently shrink
// the channel pool.
func ChannelWithinLimits(ch *Channel) bool {
	if ch == nil {
		return true
	}
	return ChannelWithinLimitsBatch([]*Channel{ch})[ch.Id]
}

// ChannelWithinLimitsBatch reports, keyed by channel id, whether each channel
// stays within its limits. Channels without configured limits are always
// allowed, and the counters are fetched in one round trip per store so
// candidate filtering stays cheap while the channel cache lock is held.
func ChannelWithinLimitsBatch(channels []*Channel) map[int]bool {
	allowed := make(map[int]bool, len(channels))
	limited := make([]*Channel, 0, len(channels))
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		allowed[ch.Id] = true
		if channelHasAnyLimit(ch) {
			limited = append(limited, ch)
		}
	}
	if len(limited) == 0 {
		return allowed
	}
	now := time.Now()
	rollingKeys := make([]string, 0, len(limited)*2)
	quotaKeys := make([]string, 0, len(limited)*2)
	dayKeys := make(map[int]string, len(limited))
	monthKeys := make(map[int]string, len(limited))
	for _, ch := range limited {
		rollingKeys = append(rollingKeys, channelRpmKey(ch.Id), channelTpmKey(ch.Id))
		dayKey, monthKey := channelQuotaKeys(ch, now)
		dayKeys[ch.Id], monthKeys[ch.Id] = dayKey, monthKey
		quotaKeys = append(quotaKeys, dayKey, monthKey)
	}
	rates := common.RollingCounterSums(rollingKeys)
	quotas := common.CounterMGet(quotaKeys)
	for _, ch := range limited {
		allowed[ch.Id] = channelWithinLimitValues(ch,
			rates[channelRpmKey(ch.Id)],
			rates[channelTpmKey(ch.Id)],
			quotas[dayKeys[ch.Id]],
			quotas[monthKeys[ch.Id]],
		)
	}
	return allowed
}

// ChannelRpmTryConsume atomically admits one upstream dispatch against the
// channel's RPM limit: it counts the dispatch only when the last 60 seconds
// stay within the limit, so concurrent attempts can never overshoot.
// Channels without an RPM limit and counter store failures both pass:
// limiting degrades to the selection-side filter instead of blocking relaying.
func ChannelRpmTryConsume(ch *Channel) bool {
	if ch == nil || ch.GetRpmLimit() <= 0 {
		return true
	}
	admitted, err := common.RollingCounterTryAdd(channelRpmKey(ch.Id), 1, ch.GetRpmLimit())
	if err != nil {
		common.SysError(fmt.Sprintf("failed to consume channel rpm counter: channel_id=%d, error=%v", ch.Id, err))
		return true
	}
	return admitted
}

// RecordChannelTokenUsage adds settled token usage to the channel's TPM
// window. It is invoked from consume-log recording so every billing path
// (text, audio, realtime websocket, image, task submit, channel test) counts
// exactly once per request.
func RecordChannelTokenUsage(channelId int, tokens int64) {
	if channelId <= 0 || tokens <= 0 {
		return
	}
	if err := common.RollingCounterAdd(channelTpmKey(channelId), tokens); err != nil {
		common.SysError(fmt.Sprintf("failed to record channel tpm counter: channel_id=%d, tokens=%d, error=%v", channelId, tokens, err))
	}
}

// channelListPriceQuota converts a charged quota delta back to list price.
func channelListPriceQuota(quota int64, groupRatio float64) int64 {
	if groupRatio <= 0 || math.IsNaN(groupRatio) || math.IsInf(groupRatio, 0) {
		return 0
	}
	return int64(math.Round(float64(quota) / groupRatio))
}

// RecordChannelSpend mirrors a signed used_quota delta, at list price, onto
// the day and month budget windows that contain spend.ChargedAt, in one atomic
// step so the two windows cannot drift apart after a partial write failure.
// Negative deltas clamp at zero inside each window. Only channels with a daily
// or monthly limit keep windows, so a budget counts usage from the moment it
// is set; without Redis the windows are also saved to the database so a
// restart does not reset them (see ChannelQuotaWindow).
func RecordChannelSpend(channelId int, quota int64, spend ChannelSpend) {
	if channelId <= 0 || quota == 0 {
		return
	}
	listQuota := channelListPriceQuota(quota, spend.GroupRatio)
	if listQuota == 0 {
		return
	}
	ch, err := CacheGetChannel(channelId)
	if err != nil || ch == nil {
		// The channel is gone, so no budget can be enforced for it any more.
		return
	}
	if ch.GetDailyQuotaLimit() <= 0 && ch.GetMonthlyQuotaLimit() <= 0 {
		return
	}
	chargedAt := spend.ChargedAt
	if chargedAt.IsZero() {
		chargedAt = time.Now()
	}
	dayKey, monthKey := channelQuotaKeys(ch, chargedAt)
	deltas := []common.CounterDelta{
		{Key: dayKey, Delta: listQuota, TTL: dailyQuotaWindowTTL},
		{Key: monthKey, Delta: listQuota, TTL: monthlyQuotaWindowTTL},
	}
	if err := common.CounterQuotaMIncrBy(deltas); err != nil {
		common.SysError(fmt.Sprintf("failed to record channel quota counters: channel_id=%d, quota=%d, error=%v", channelId, listQuota, err))
	}
	if !common.RedisEnabled {
		if err := persistChannelQuotaDeltas(deltas); err != nil {
			common.SysError(fmt.Sprintf("failed to save channel quota windows: channel_id=%d, quota=%d, error=%v", channelId, listQuota, err))
		}
	}
}
