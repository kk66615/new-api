package common

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// Calendar-window counters for the channel daily/monthly quota limits. Every
// key embeds its window label (for example the day or month it covers), so a
// window resets by rolling to the next label and stale keys expire on their
// own. Redis is the authoritative store for multi-node deployments; the
// in-memory fallback keeps single-node deployments working. Quota batches
// clamp refunds at zero so a window never reads below its real usage, while
// callers compare with >= limit. Per-minute limits use the rolling counters
// in rolling_counter.go instead.

const windowCounterJanitorInterval = 10 * time.Minute

type windowCounterEntry struct {
	value     int64
	expiresAt time.Time
}

type windowCounterStore struct {
	mutex   sync.RWMutex
	entries map[string]windowCounterEntry
}

var windowCounters = &windowCounterStore{entries: map[string]windowCounterEntry{}}
var windowCounterJanitorOnce sync.Once

// CounterKey builds a calendar-window counter key from a prefix, an entity id,
// and the window label.
func CounterKey(prefix string, id int, bucket string) string {
	return fmt.Sprintf("%s:%d:%s", prefix, id, bucket)
}

// CounterDelta pairs a counter key with the signed delta to apply and the
// key's TTL.
type CounterDelta struct {
	Key   string
	Delta int64
	TTL   time.Duration
}

// CounterQuotaMIncrBy applies every signed quota delta in one atomic step: a
// single Redis Lua script, or one in-memory critical section, so paired
// windows (for example the daily and monthly channel quota) cannot drift
// apart after a partial failure. Positive deltas accumulate. Negative deltas
// (refunds and task rollbacks) clamp at zero: a refund whose charge window
// already rolled over must not loosen the new window's budget.
func CounterQuotaMIncrBy(deltas []CounterDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	if RedisEnabled && RDB != nil {
		return redisCounterQuotaMIncrBy(deltas)
	}
	memoryCounterQuotaMIncrBy(deltas)
	return nil
}

// counterQuotaMIncrByScript arms each key's TTL and clamps negative deltas at
// zero in the same atomic execution, so Redis and the in-memory store cannot
// disagree after a partial failure.
var counterQuotaMIncrByScript = redis.NewScript(`local n = #KEYS
for i = 1, n do
	local delta = tonumber(ARGV[i])
	if delta < 0 then
		local raw = redis.call('GET', KEYS[i])
		if raw then
			local value = tonumber(raw) + delta
			if value < 0 then
				value = 0
			end
			redis.call('SET', KEYS[i], value)
		end
	else
		redis.call('INCRBY', KEYS[i], delta)
	end
	local ttl = tonumber(ARGV[n + i])
	if ttl > 0 then
		redis.call('EXPIRE', KEYS[i], ttl)
	end
end
return 1`)

func redisCounterQuotaMIncrBy(deltas []CounterDelta) error {
	ctx := context.Background()
	keys := make([]string, 0, len(deltas))
	args := make([]any, 0, len(deltas)*2)
	for _, delta := range deltas {
		keys = append(keys, delta.Key)
		args = append(args, delta.Delta)
	}
	for _, delta := range deltas {
		args = append(args, int64(delta.TTL/time.Second))
	}
	return counterQuotaMIncrByScript.Run(ctx, RDB, keys, args...).Err()
}

func memoryCounterQuotaMIncrBy(deltas []CounterDelta) {
	startWindowCounterJanitor()
	now := time.Now()
	windowCounters.mutex.Lock()
	defer windowCounters.mutex.Unlock()
	for _, delta := range deltas {
		entry, ok := windowCounters.entries[delta.Key]
		if !ok || now.After(entry.expiresAt) {
			entry = windowCounterEntry{expiresAt: now.Add(delta.TTL)}
		}
		entry.value += delta.Delta
		if entry.value < 0 {
			entry.value = 0
		}
		windowCounters.entries[delta.Key] = entry
	}
}

// CounterRestore is one saved calendar-window value to load back into the
// in-memory store.
type CounterRestore struct {
	Key       string
	Value     int64
	ExpiresAt time.Time
}

// RestoreMemoryCounters loads saved window values into the in-memory store,
// replacing what it holds for those keys. Expired values are skipped. It is
// only meaningful without Redis, which keeps the windows itself.
func RestoreMemoryCounters(values []CounterRestore) {
	startWindowCounterJanitor()
	now := time.Now()
	windowCounters.mutex.Lock()
	defer windowCounters.mutex.Unlock()
	for _, value := range values {
		if !value.ExpiresAt.After(now) {
			continue
		}
		windowCounters.entries[value.Key] = windowCounterEntry{value: value.Value, expiresAt: value.ExpiresAt}
	}
}

// CounterGet returns the current value of one counter, or 0 when unset or
// expired. Read failures fail open (value 0) after logging.
func CounterGet(key string) int64 {
	return CounterMGet([]string{key})[key]
}

// CounterMGet reads several counter values in one round trip. Keys missing
// from the result map must be treated as 0 by callers.
func CounterMGet(keys []string) map[string]int64 {
	values := make(map[string]int64, len(keys))
	if len(keys) == 0 {
		return values
	}
	if RedisEnabled && RDB != nil {
		redisValues, err := redisCounterMGet(keys)
		if err != nil {
			SysError("failed to read rate limit counters: " + err.Error())
			return values
		}
		return redisValues
	}
	return memoryCounterMGet(keys)
}

func redisCounterMGet(keys []string) (map[string]int64, error) {
	ctx := context.Background()
	raw, err := RDB.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	values := make(map[string]int64, len(keys))
	for i, item := range raw {
		if item == nil {
			continue
		}
		if str, ok := item.(string); ok {
			if value, parseErr := strconv.ParseInt(str, 10, 64); parseErr == nil {
				values[keys[i]] = value
			}
		}
	}
	return values, nil
}

func memoryCounterMGet(keys []string) map[string]int64 {
	startWindowCounterJanitor()
	now := time.Now()
	windowCounters.mutex.RLock()
	defer windowCounters.mutex.RUnlock()
	values := make(map[string]int64, len(keys))
	for _, key := range keys {
		entry, ok := windowCounters.entries[key]
		if !ok || now.After(entry.expiresAt) {
			continue
		}
		values[key] = entry.value
	}
	return values
}

// startWindowCounterJanitor periodically drops expired in-memory buckets so a
// long-running node does not accumulate one entry per idle window.
func startWindowCounterJanitor() {
	windowCounterJanitorOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(windowCounterJanitorInterval)
			for now := range ticker.C {
				windowCounters.mutex.Lock()
				for key, entry := range windowCounters.entries {
					if now.After(entry.expiresAt) {
						delete(windowCounters.entries, key)
					}
				}
				windowCounters.mutex.Unlock()
			}
		}()
	})
}
