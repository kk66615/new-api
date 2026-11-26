package common

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useRollingCounterClock pins the rolling counter clock to *now for the test.
func useRollingCounterClock(t *testing.T, now *int64) {
	t.Helper()
	previous := rollingCounterNow
	rollingCounterNow = func() int64 { return *now }
	t.Cleanup(func() { rollingCounterNow = previous })
}

// rollingCounterStores runs fn once against the in-memory store and once
// against Redis, so both backends are held to the same behaviour.
func rollingCounterStores(t *testing.T, fn func(t *testing.T)) {
	t.Run("memory", func(t *testing.T) {
		useWindowCounterMemoryStore(t)
		fn(t)
	})
	t.Run("redis", func(t *testing.T) {
		useWindowCounterMiniRedis(t)
		fn(t)
	})
}

func TestRollingCounterRejectsTheMinuteBoundaryBurst(t *testing.T) {
	rollingCounterStores(t, func(t *testing.T) {
		now := int64(1_800_000_059) // the last second of a calendar minute
		useRollingCounterClock(t, &now)
		key := RollingCounterKey("testRollingBurst", int(windowCounterTestBucketSeq.Add(1)))

		for i := 0; i < 3; i++ {
			admitted, err := RollingCounterTryAdd(key, 1, 3)
			require.NoError(t, err)
			assert.True(t, admitted)
		}
		// One second later a new calendar minute begins; a fixed window
		// would admit three more here.
		now++
		admitted, err := RollingCounterTryAdd(key, 1, 3)
		require.NoError(t, err)
		assert.False(t, admitted, "the rolling minute still holds the three requests of a second ago")
		assert.Equal(t, int64(3), RollingCounterSums([]string{key})[key], "a rejected attempt is not counted")

		// 59 seconds after the burst second it still counts; a full 60
		// seconds after its last possible instant it has left the window.
		now += 58
		admitted, err = RollingCounterTryAdd(key, 1, 3)
		require.NoError(t, err)
		assert.False(t, admitted, "a request less than 60 seconds after the burst is still refused")
		now += 2
		admitted, err = RollingCounterTryAdd(key, 1, 3)
		require.NoError(t, err)
		assert.True(t, admitted)
		assert.Equal(t, int64(1), RollingCounterSums([]string{key})[key])
	})
}

func TestRollingCounterSlidesSecondBySecond(t *testing.T) {
	rollingCounterStores(t, func(t *testing.T) {
		now := int64(1_800_000_000)
		useRollingCounterClock(t, &now)
		key := RollingCounterKey("testRollingSlide", int(windowCounterTestBucketSeq.Add(1)))

		require.NoError(t, RollingCounterAdd(key, 100)) // t = 0
		now += 30
		require.NoError(t, RollingCounterAdd(key, 50)) // t = 30
		now += 29
		assert.Equal(t, int64(150), RollingCounterSums([]string{key})[key], "t = 59: both are inside the last 60 seconds")
		now++
		assert.Equal(t, int64(150), RollingCounterSums([]string{key})[key],
			"t = 60: the first add may be less than 60 seconds old (it landed somewhere in second 0), so it still counts")
		now++
		assert.Equal(t, int64(50), RollingCounterSums([]string{key})[key], "t = 61: the first add has left the window")

		admitted, err := RollingCounterTryAdd(key, 60, 100)
		require.NoError(t, err)
		assert.False(t, admitted, "50 + 60 would pass the limit of 100")
		admitted, err = RollingCounterTryAdd(key, 50, 100)
		require.NoError(t, err)
		assert.True(t, admitted, "50 + 50 reaches the limit exactly and is allowed")

		now += 120
		assert.Equal(t, int64(0), RollingCounterSums([]string{key})[key], "an idle counter reads as zero")
	})
}

func TestRollingCounterSumsBatchesKeys(t *testing.T) {
	rollingCounterStores(t, func(t *testing.T) {
		now := int64(1_800_000_000)
		useRollingCounterClock(t, &now)
		seq := int(windowCounterTestBucketSeq.Add(1))
		first := RollingCounterKey("testRollingBatchA", seq)
		second := RollingCounterKey("testRollingBatchB", seq)
		missing := RollingCounterKey("testRollingBatchC", seq)
		require.NoError(t, RollingCounterAdd(first, 2))
		require.NoError(t, RollingCounterAdd(second, 9))

		sums := RollingCounterSums([]string{first, second, missing})
		assert.Equal(t, int64(2), sums[first])
		assert.Equal(t, int64(9), sums[second])
		assert.Equal(t, int64(0), sums[missing])
	})
}

func TestRollingCounterConcurrentAdmissionNeverOvershoots(t *testing.T) {
	rollingCounterStores(t, func(t *testing.T) {
		now := int64(1_800_000_000)
		useRollingCounterClock(t, &now)
		key := RollingCounterKey("testRollingRace", int(windowCounterTestBucketSeq.Add(1)))

		var wg sync.WaitGroup
		results := make(chan bool, 50)
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := RollingCounterTryAdd(key, 1, 20)
				assert.NoError(t, err)
				results <- ok
			}()
		}
		wg.Wait()
		close(results)
		count := 0
		for ok := range results {
			if ok {
				count++
			}
		}
		assert.Equal(t, 20, count, "exactly the limit is admitted under contention")
		assert.Equal(t, int64(20), RollingCounterSums([]string{key})[key])
	})
}

func TestRollingCounterRedisCountsAheadClocksAndExpires(t *testing.T) {
	redisServer := useWindowCounterMiniRedis(t)
	now := int64(1_800_000_000)
	useRollingCounterClock(t, &now)
	key := RollingCounterKey("testRollingSkew", int(windowCounterTestBucketSeq.Add(1)))

	// Another node whose clock runs two seconds ahead writes first.
	now += 2
	require.NoError(t, RollingCounterAdd(key, 5))
	now -= 2
	assert.Equal(t, int64(5), RollingCounterSums([]string{key})[key], "traffic from a node slightly ahead still counts")
	admitted, err := RollingCounterTryAdd(key, 1, 5)
	require.NoError(t, err)
	assert.False(t, admitted, "the ahead node's traffic counts toward the limit too")
	assert.True(t, redisServer.TTL(key) > 0, "the Redis hash carries a TTL so idle counters disappear")
}
