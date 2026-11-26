package common

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// Rolling 60-second counters for RPM/TPM limits. Amounts are kept per epoch
// second, and a counter sums the current second and the 60 before it, so two
// admissions counted against each other are always at least 60 seconds apart
// and a limit of N per minute admits at most N in any 60-second span (the
// window errs up to one second on the safe side). A fixed calendar-minute
// window would let up to 2N through around a minute boundary (N at hh:mm:59,
// N more at hh:mm+1:00), which is exactly the burst an upstream's own rolling
// limiter rejects.
//
// Redis keeps one hash per counter (field = epoch second, value = amount) and
// a Lua script checks and adds atomically, so every node shares one window.
// Without Redis each process keeps its own 60-slot ring.

const (
	RollingCounterWindowSeconds = 60
	// rollingCounterSlots is the current second plus the 60 before it.
	rollingCounterSlots        = RollingCounterWindowSeconds + 1
	rollingCounterRedisTTL     = 2 * time.Minute
	rollingCounterIdleAfter    = 2 * RollingCounterWindowSeconds
	rollingCounterJanitorEvery = 5 * time.Minute
)

// RollingCounterKey names the rolling counter of one entity.
func RollingCounterKey(prefix string, id int) string {
	return fmt.Sprintf("%s:%d", prefix, id)
}

// rollingCounterNow is the clock of every rolling counter; tests replace it.
var rollingCounterNow = func() int64 { return time.Now().Unix() }

// RollingCounterTryAdd adds delta to the counter when the last 60 seconds plus
// delta stay within limit, and reports whether it did. limit <= 0 always adds.
// A rejected attempt changes nothing, so callers never roll back.
func RollingCounterTryAdd(key string, delta int64, limit int64) (bool, error) {
	now := rollingCounterNow()
	if RedisEnabled && RDB != nil {
		return redisRollingTryAdd(key, delta, limit, now)
	}
	return memoryRollingTryAdd(key, delta, limit, now), nil
}

// RollingCounterAdd adds delta to the counter without a limit check.
func RollingCounterAdd(key string, delta int64) error {
	_, err := RollingCounterTryAdd(key, delta, 0)
	return err
}

// RollingCounterSums returns the last-60-second total of each key. Keys that
// are unset read as 0. A Redis read failure is logged and reads as 0 for every
// key (fail open), the same policy as CounterMGet.
func RollingCounterSums(keys []string) map[string]int64 {
	sums := make(map[string]int64, len(keys))
	if len(keys) == 0 {
		return sums
	}
	now := rollingCounterNow()
	if RedisEnabled && RDB != nil {
		redisSums, err := redisRollingSums(keys, now)
		if err != nil {
			SysError("failed to read rolling rate limit counters: " + err.Error())
			return sums
		}
		return redisSums
	}
	return memoryRollingSums(keys, now)
}

// --- in-memory store ---

type rollingRing struct {
	seconds  [rollingCounterSlots]int64
	amounts  [rollingCounterSlots]int64
	lastSeen int64
}

// sum totals the slots of the current second and the 60 before it. Slots
// hold the epoch second they belong to, so a slot reused after a pause is
// recognised as stale instead of being counted.
func (r *rollingRing) sum(now int64) int64 {
	var total int64
	for i := range r.seconds {
		if r.seconds[i] > now-rollingCounterSlots {
			total += r.amounts[i]
		}
	}
	return total
}

func (r *rollingRing) add(now int64, delta int64) {
	slot := now % rollingCounterSlots
	if r.seconds[slot] != now {
		r.seconds[slot] = now
		r.amounts[slot] = 0
	}
	r.amounts[slot] += delta
	r.lastSeen = now
}

type rollingStore struct {
	mutex sync.Mutex
	rings map[string]*rollingRing
}

var rollingCounters = &rollingStore{rings: map[string]*rollingRing{}}
var rollingCounterJanitorOnce sync.Once

func memoryRollingTryAdd(key string, delta int64, limit int64, now int64) bool {
	startRollingCounterJanitor()
	rollingCounters.mutex.Lock()
	defer rollingCounters.mutex.Unlock()
	ring := rollingCounters.rings[key]
	if ring == nil {
		ring = &rollingRing{}
		rollingCounters.rings[key] = ring
	}
	if limit > 0 && ring.sum(now)+delta > limit {
		return false
	}
	ring.add(now, delta)
	return true
}

func memoryRollingSums(keys []string, now int64) map[string]int64 {
	startRollingCounterJanitor()
	rollingCounters.mutex.Lock()
	defer rollingCounters.mutex.Unlock()
	sums := make(map[string]int64, len(keys))
	for _, key := range keys {
		if ring := rollingCounters.rings[key]; ring != nil {
			sums[key] = ring.sum(now)
		}
	}
	return sums
}

// startRollingCounterJanitor drops rings that saw no write for two windows so
// a long-running node does not keep one ring per channel or token forever.
func startRollingCounterJanitor() {
	rollingCounterJanitorOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(rollingCounterJanitorEvery)
			for range ticker.C {
				now := rollingCounterNow()
				rollingCounters.mutex.Lock()
				for key, ring := range rollingCounters.rings {
					if ring.lastSeen <= now-rollingCounterIdleAfter {
						delete(rollingCounters.rings, key)
					}
				}
				rollingCounters.mutex.Unlock()
			}
		}()
	})
}

// --- Redis store ---

// rollingTryAddScript checks and adds in one step. Fields older than the
// window are deleted on the way; fields from a node whose clock runs slightly
// ahead still count, so clock skew never hides traffic.
var rollingTryAddScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local delta = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local window = tonumber(ARGV[4])
local fields = redis.call('HGETALL', KEYS[1])
local total = 0
for i = 1, #fields, 2 do
	if tonumber(fields[i]) <= now - window then
		redis.call('HDEL', KEYS[1], fields[i])
	else
		total = total + tonumber(fields[i + 1])
	end
end
if limit > 0 and total + delta > limit then
	return 0
end
redis.call('HINCRBY', KEYS[1], ARGV[1], delta)
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[5]))
return 1
`)

func redisRollingTryAdd(key string, delta int64, limit int64, now int64) (bool, error) {
	result, err := rollingTryAddScript.Run(context.Background(), RDB, []string{key},
		now, delta, limit, rollingCounterSlots, int64(rollingCounterRedisTTL/time.Second)).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func redisRollingSums(keys []string, now int64) (map[string]int64, error) {
	ctx := context.Background()
	pipe := RDB.Pipeline()
	cmds := make([]*redis.StringStringMapCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.HGetAll(ctx, key)
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	sums := make(map[string]int64, len(keys))
	for i, cmd := range cmds {
		fields, err := cmd.Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}
		var total int64
		for second, amount := range fields {
			sec, secErr := strconv.ParseInt(second, 10, 64)
			value, valueErr := strconv.ParseInt(amount, 10, 64)
			if secErr != nil || valueErr != nil || sec <= now-rollingCounterSlots {
				continue
			}
			total += value
		}
		if total != 0 {
			sums[keys[i]] = total
		}
	}
	return sums, nil
}
