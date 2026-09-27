package model

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// openEvidenceTestDB returns an isolated in-memory SQLite database with the
// evidence table migrated (twice, to prove the migration is idempotent).
func openEvidenceTestDB(t *testing.T, migrate bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if migrate {
		require.NoError(t, db.AutoMigrate(&UpstreamEvidence{}))
		require.NoError(t, db.AutoMigrate(&UpstreamEvidence{}))
	}
	return db
}

func TestEvidenceWriterBatchesAndSanitizes(t *testing.T) {
	db := openEvidenceTestDB(t, true)
	for _, col := range []string{"model_frame", "error_frame", "excerpt_truncated", "excerpt_full_len", "retry_index", "prompt_sha256", "output_sha256"} {
		assert.True(t, db.Migrator().HasColumn(&UpstreamEvidence{}, col), col)
	}
	w := newEvidenceWriter(func() *gorm.DB { return db }, 16, 4, 20*time.Millisecond)

	longModel := strings.Repeat("模", 300)
	for i := range 6 {
		ok := w.enqueue(&UpstreamEvidence{
			Kind:            "normal",
			ChannelId:       987650,
			ResponseModel:   longModel,
			ResponseExcerpt: "bad\x00utf8\xff" + strings.Repeat("x", i),
			ResponseHeaders: `{"a":"b"}`,
		})
		require.True(t, ok)
	}
	w.flush(2 * time.Second)

	var rows []UpstreamEvidence
	require.NoError(t, db.Where("channel_id = ?", 987650).Find(&rows).Error)
	require.Len(t, rows, 6)
	for _, row := range rows {
		assert.Equal(t, 255, len([]rune(row.ResponseModel)), "varchar cut by characters")
		assert.NotContains(t, row.ResponseExcerpt, "\x00")
		assert.True(t, strings.HasPrefix(row.ResponseExcerpt, "badutf8�"))
	}
	health := w.health()
	assert.True(t, health.Enabled)
	assert.Equal(t, int64(6), health.InsertedTotal)
	assert.Zero(t, health.DroppedTotal)
	assert.Zero(t, health.FailedTotal)
	assert.Equal(t, 16, health.QueueCap)
	assert.NotZero(t, health.StartedAt)
}

func TestEvidenceWriterDropsWhenQueueIsFull(t *testing.T) {
	db := openEvidenceTestDB(t, true)
	w := newEvidenceWriter(func() *gorm.DB { return db }, 1, 4, time.Hour)
	// enabled but with no consumer running, so the queue stays full
	w.startOnce.Do(func() {})
	w.enabled.Store(true)

	assert.True(t, w.enqueue(&UpstreamEvidence{Kind: "normal"}))
	assert.False(t, w.enqueue(&UpstreamEvidence{Kind: "normal"}))
	assert.False(t, w.enqueue(&UpstreamEvidence{Kind: "normal"}))
	health := w.health()
	assert.Equal(t, int64(2), health.DroppedTotal)
	assert.Equal(t, 1, health.QueueLen)
}

// A missing table pauses persistence and is re-checked with backoff: once
// the table appears (a late migration), the writer switches on by itself.
func TestEvidenceWriterWaitsForAMissingTable(t *testing.T) {
	db := openEvidenceTestDB(t, false)
	w := newEvidenceWriter(func() *gorm.DB { return db }, 4, 4, 10*time.Millisecond)
	w.tableRetry = time.Hour // the background retry stays asleep; the test drives the retries
	assert.False(t, w.enqueue(&UpstreamEvidence{Kind: "normal"}))
	health := w.health()
	assert.False(t, health.Enabled)
	assert.Contains(t, health.DisabledReason, "retrying")
	assert.Zero(t, health.DroppedTotal, "a paused writer is not a dropping writer")
	assert.False(t, w.activateIfTableExists(), "still no table")

	require.NoError(t, db.AutoMigrate(&UpstreamEvidence{}))
	require.True(t, w.activateIfTableExists())
	assert.True(t, w.health().Enabled)
	assert.Empty(t, w.health().DisabledReason)
	require.True(t, w.enqueue(&UpstreamEvidence{Kind: "normal", ChannelId: 987653}))
	w.flush(2 * time.Second)
	var count int64
	require.NoError(t, db.Model(&UpstreamEvidence{}).Where("channel_id = ?", 987653).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestEvidenceWriterDisablesOnceWithoutDatabase(t *testing.T) {

	nilDB := newEvidenceWriter(func() *gorm.DB { return nil }, 4, 4, time.Hour)
	assert.False(t, nilDB.enqueue(&UpstreamEvidence{Kind: "normal"}))
	assert.False(t, nilDB.health().Enabled)
	nilDB.flush(10 * time.Millisecond) // must not block on a writer that never ran
}

// A failing batch is retried row by row so one bad row cannot take the others
// with it; the bad row is counted and its error kept for the health endpoint.
func TestEvidenceWriterIsolatesAFailingRow(t *testing.T) {
	db := openEvidenceTestDB(t, true)
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("evidence_test_fail", func(tx *gorm.DB) {
		fail := false
		switch dest := tx.Statement.Dest.(type) {
		case *UpstreamEvidence:
			fail = dest.Note == "poison"
		case []*UpstreamEvidence:
			for _, e := range dest {
				fail = fail || e.Note == "poison"
			}
		}
		if fail {
			_ = tx.AddError(errors.New("simulated constraint violation"))
		}
	}))
	w := newEvidenceWriter(func() *gorm.DB { return db }, 16, 10, 20*time.Millisecond)
	for _, note := range []string{"a", "poison", "b"} {
		require.True(t, w.enqueue(&UpstreamEvidence{Kind: "normal", ChannelId: 987651, Note: note}))
	}
	w.flush(2 * time.Second)

	var notes []string
	require.NoError(t, db.Model(&UpstreamEvidence{}).Where("channel_id = ?", 987651).Order("note").Pluck("note", &notes).Error)
	assert.Equal(t, []string{"a", "b"}, notes)
	health := w.health()
	assert.Equal(t, int64(2), health.InsertedTotal)
	assert.Equal(t, int64(1), health.FailedTotal)
	assert.Contains(t, health.LastError, "simulated constraint violation")
	assert.NotZero(t, health.LastErrorAt)
}

func TestEvidencePruneDeletesOnlyExpiredNormalRows(t *testing.T) {
	db := openEvidenceTestDB(t, true)
	cutoff := time.Now().Unix()
	old := cutoff - 3600
	rows := []*UpstreamEvidence{
		{Kind: "normal", CreatedAt: old, ChannelId: 987652},
		{Kind: "normal", CreatedAt: old, ChannelId: 987652},
		{Kind: "model_mismatch", CreatedAt: old, ChannelId: 987652},
		{Kind: "replay_suspect", CreatedAt: old, ChannelId: 987652},
		{Kind: "normal", CreatedAt: cutoff + 60, ChannelId: 987652},
	}
	require.NoError(t, db.Create(rows).Error)
	w := newEvidenceWriter(func() *gorm.DB { return db }, 4, 4, time.Hour)

	assert.Equal(t, int64(2), w.pruneOnce(cutoff))
	var kinds []string
	require.NoError(t, db.Model(&UpstreamEvidence{}).Where("channel_id = ?", 987652).Order("kind").Pluck("kind", &kinds).Error)
	assert.Equal(t, []string{"model_mismatch", "normal", "replay_suspect"}, kinds)
	assert.Zero(t, w.pruneOnce(cutoff), "a second run finds nothing")

	nilDB := newEvidenceWriter(func() *gorm.DB { return nil }, 4, 4, time.Hour)
	assert.Zero(t, nilDB.pruneOnce(cutoff))
}

// The replay-lookup indexes are built outside AutoMigrate and the build is
// idempotent (it runs on every startup).
func TestEnsureUpstreamEvidenceIndexesIsIdempotent(t *testing.T) {
	db := openEvidenceTestDB(t, true)
	for _, idx := range upstreamEvidenceIndexes {
		assert.False(t, db.Migrator().HasIndex(&UpstreamEvidence{}, idx.name), "AutoMigrate must not build %s", idx.name)
	}
	require.NoError(t, ensureUpstreamEvidenceIndexes(db))
	require.NoError(t, ensureUpstreamEvidenceIndexes(db))
	for _, idx := range upstreamEvidenceIndexes {
		assert.True(t, db.Migrator().HasIndex(&UpstreamEvidence{}, idx.name), idx.name)
	}
}

// The queue is bounded by memory as well as by rows: large rows beyond the
// byte budget are dropped and counted even with free slots.
func TestEvidenceWriterDropsBeyondByteBudget(t *testing.T) {
	db := openEvidenceTestDB(t, true)
	w := newEvidenceWriter(func() *gorm.DB { return db }, 16, 4, time.Hour)
	w.byteBudget = 10_000
	w.startOnce.Do(func() {})
	w.enabled.Store(true)

	big := strings.Repeat("x", 6_000)
	assert.True(t, w.enqueue(&UpstreamEvidence{Kind: "normal", ResponseExcerpt: big}))
	assert.False(t, w.enqueue(&UpstreamEvidence{Kind: "normal", ResponseExcerpt: big}))
	health := w.health()
	assert.Equal(t, int64(1), health.DroppedTotal)
	assert.Equal(t, 1, health.QueueLen)
	assert.Equal(t, int64(6_512), health.QueueBytes)

	w.dequeued(<-w.queue)
	assert.Zero(t, w.health().QueueBytes, "a dequeued row releases its budget")
}

func TestRedactDatabaseAddress(t *testing.T) {
	msg := `failed to connect to host=10.0.0.5 user=newapi database=logs: dial tcp 10.0.0.5:5432: connect: connection refused; dsn postgres://newapi:secret@db.internal:5432/logs`
	redacted := redactDatabaseAddress(msg)
	for _, secret := range []string{"10.0.0.5", "5432", "secret", "db.internal", "user=newapi"} {
		assert.NotContains(t, redacted, secret)
	}
	assert.Contains(t, redacted, "connection refused")
}

// The background index build is opt-in: without AUDIT_EVIDENCE_AUTO_INDEX the
// production writer reports index_state=disabled and builds nothing.
func TestEvidenceIndexBuildIsOptIn(t *testing.T) {
	t.Setenv("AUDIT_EVIDENCE_AUTO_INDEX", "")
	db := openEvidenceTestDB(t, true)
	w := newEvidenceWriter(func() *gorm.DB { return db }, 4, 4, time.Hour)
	w.maintenance = true
	w.startOnce.Do(w.start)
	assert.True(t, w.health().Enabled)
	assert.Equal(t, "disabled", w.health().IndexState)

	t.Setenv("AUDIT_EVIDENCE_AUTO_INDEX", "true")
	enabled := newEvidenceWriter(func() *gorm.DB { return db }, 4, 4, time.Hour)
	enabled.maintenance = true
	enabled.startOnce.Do(enabled.start)
	assert.Equal(t, "pending", enabled.health().IndexState)
}

// Every text column fits MySQL TEXT (65535 bytes) as valid UTF-8, even when
// invalid bytes grow on replacement after an upstream cap was applied.
func TestEvidenceSanitizeCapsTextColumnsAfterReplacement(t *testing.T) {
	invalid := strings.Repeat("\xff", 30_000)
	e := &UpstreamEvidence{ResponseExcerpt: invalid, ResponseHeaders: invalid, ModelFrame: invalid, ErrorFrame: "海" + invalid}
	e.sanitize()
	for _, v := range []string{e.ResponseExcerpt, e.ResponseHeaders, e.ModelFrame, e.ErrorFrame} {
		assert.LessOrEqual(t, len(v), 65535)
		assert.True(t, utf8.ValidString(v))
	}
}
