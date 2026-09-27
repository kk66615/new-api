package model

import (
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// NormalEvidenceRetentionDays: kind='normal' rows (full response retention for
// every clean request — user decision 2026-09-02) are pruned after this many
// days. Anomaly rows (every other kind) are kept forever.
const NormalEvidenceRetentionDays = 30

// UpstreamEvidence stores the forensic record for upstream responses:
// kind=normal            — every clean request (response excerpt + headers)
// kind=model_mismatch    — upstream declared a different model than we sent
// kind=provider_switch   — the inferred upstream source/backend flipped
// kind=mid_stream_change — one stream declared more than one model
// kind=replay_suspect    — the response looks replayed (duplicate id/output)
// The response excerpt is capped at 64KB, frames at 16KB, headers at 8KB.
//
// Every column added after the table reached production size is nullable
// with no default, so AutoMigrate adds it without rewriting the table. Its
// secondary indexes are NOT declared here: they are built in the background
// after startup (ensureUpstreamEvidenceIndexes), never by AutoMigrate.
type UpstreamEvidence struct {
	Id        int   `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt int64 `json:"created_at" gorm:"bigint;index"`
	// Kind: normal | model_mismatch | provider_switch | mid_stream_change | replay_suspect
	Kind string `json:"kind" gorm:"type:varchar(32);index;default:model_mismatch"`
	// Note: human-readable context (e.g. "azure -> gpt_relay" for switches)
	Note string `json:"note" gorm:"type:varchar(255)"`
	// Source: inferred upstream source label at record time
	Source string `json:"source" gorm:"type:varchar(32)"`
	// RequestId: NewAPI request id (X-Oneapi-Request-Id) — searchable in logs
	RequestId         string `json:"request_id" gorm:"type:varchar(64);index"`
	ChannelId         int    `json:"channel_id" gorm:"index"`
	UserId            int    `json:"user_id"`
	TokenId           int    `json:"token_id"`
	RequestModel      string `json:"request_model" gorm:"type:varchar(255)"`
	SentModel         string `json:"sent_model" gorm:"type:varchar(255)"`
	ResponseModel     string `json:"response_model" gorm:"type:varchar(255);index"`
	ResponseId        string `json:"response_id" gorm:"type:varchar(255)"`
	SystemFingerprint string `json:"system_fingerprint" gorm:"type:varchar(128)"`
	ResponseCreated   int64  `json:"response_created"`
	IsStream          bool   `json:"is_stream"`
	UseTimeMs         int    `json:"use_time_ms"`
	ResponseExcerpt   string `json:"response_excerpt" gorm:"type:text"`
	ResponseHeaders   string `json:"response_headers" gorm:"type:text"`
	// ModelFrame: the frame that declared the model used for the verdict
	// (first model-bearing stream frame, or the tail of an oversized body)
	ModelFrame string `json:"model_frame" gorm:"type:text"`
	// ErrorFrame: an upstream error frame, kept apart from the excerpt
	ErrorFrame       string `json:"error_frame" gorm:"type:text"`
	ExcerptTruncated bool   `json:"excerpt_truncated"`
	ExcerptFullLen   int    `json:"excerpt_full_len"`
	// RetryIndex: 0 for the first upstream attempt of a request
	RetryIndex int `json:"retry_index"`
	// PromptSha256 / OutputSha256: hashes of the canonicalised sent prompt and
	// of the normalised output; no text is stored for either
	PromptSha256 string `json:"prompt_sha256" gorm:"column:prompt_sha256;type:varchar(64)"`
	OutputSha256 string `json:"output_sha256" gorm:"column:output_sha256;type:varchar(64)"`
}

// sanitize makes every column storable: varchar values are cut to their
// declared length in characters, text values lose NUL bytes and invalid UTF-8
// (PostgreSQL rejects both with SQLSTATE 22021 / 22P05).
func (e *UpstreamEvidence) sanitize() {
	e.Kind = common.TruncateAuditRunes(e.Kind, 32)
	e.Note = common.TruncateAuditRunes(e.Note, 255)
	e.Source = common.TruncateAuditRunes(e.Source, 32)
	e.RequestId = common.TruncateAuditRunes(e.RequestId, 64)
	e.RequestModel = common.TruncateAuditRunes(e.RequestModel, 255)
	e.SentModel = common.TruncateAuditRunes(e.SentModel, 255)
	e.ResponseModel = common.TruncateAuditRunes(e.ResponseModel, 255)
	e.ResponseId = common.TruncateAuditRunes(e.ResponseId, 255)
	e.SystemFingerprint = common.TruncateAuditRunes(e.SystemFingerprint, 128)
	e.PromptSha256 = common.TruncateAuditRunes(e.PromptSha256, 64)
	e.OutputSha256 = common.TruncateAuditRunes(e.OutputSha256, 64)
	e.ResponseExcerpt = common.SanitizeAuditText(e.ResponseExcerpt)
	e.ResponseHeaders = common.SanitizeAuditText(e.ResponseHeaders)
	e.ModelFrame = common.SanitizeAuditText(e.ModelFrame)
	e.ErrorFrame = common.SanitizeAuditText(e.ErrorFrame)
}

// ── asynchronous evidence writer ─────────────────────────────────────────────
// One bounded queue drained by a single batch writer. A full queue drops the
// row (counted) instead of blocking the relay or spawning goroutines.

const (
	evidenceQueueCap = 4096
	// evidenceQueueByteBudget bounds the memory held by queued rows (a row
	// can carry ~100KB of excerpt, frames and headers): beyond it rows are
	// dropped and counted even if the queue has free slots.
	evidenceQueueByteBudget = 64 << 20
	evidenceBatchMax        = 200
	evidenceFlushInterval   = time.Second
	// evidenceIndexDelay keeps the background index build well clear of
	// startup and the first burst of traffic.
	evidenceIndexDelay = 2 * time.Minute
)

// UpstreamEvidenceHealth is the admin view of the evidence writer.
type UpstreamEvidenceHealth struct {
	Enabled        bool   `json:"enabled"`
	DisabledReason string `json:"disabled_reason,omitempty"`
	InsertedTotal  int64  `json:"inserted_total"`
	DroppedTotal   int64  `json:"dropped_total"`
	FailedTotal    int64  `json:"failed_total"`
	LastError      string `json:"last_error"`
	LastErrorAt    int64  `json:"last_error_at"`
	QueueLen       int    `json:"queue_len"`
	QueueCap       int    `json:"queue_cap"`
	QueueBytes     int64  `json:"queue_bytes"`
	StartedAt      int64  `json:"started_at"`
	IndexState     string `json:"index_state"`
}

type evidenceWriter struct {
	db         func() *gorm.DB
	queue      chan *UpstreamEvidence
	batchMax   int
	interval   time.Duration
	byteBudget int64
	// maintenance starts the pruner and the background index build with
	// the writer (the process-wide writer only).
	maintenance bool

	startOnce sync.Once
	enabled   atomic.Bool
	reason    atomic.Value // string
	started   atomic.Int64
	inserted  atomic.Int64
	dropped   atomic.Int64
	failed    atomic.Int64
	queued    atomic.Int64 // bytes held by rows in the queue
	lastErr   atomic.Value // string
	lastErrAt atomic.Int64
	lastLogAt atomic.Int64
	index     atomic.Value // string

	flushReq chan chan struct{}
}

func newEvidenceWriter(db func() *gorm.DB, queueCap, batchMax int, interval time.Duration) *evidenceWriter {
	return &evidenceWriter{
		db:         db,
		queue:      make(chan *UpstreamEvidence, queueCap),
		batchMax:   batchMax,
		interval:   interval,
		byteBudget: evidenceQueueByteBudget,
		flushReq:   make(chan chan struct{}),
	}
}

var defaultEvidenceWriter = func() *evidenceWriter {
	w := newEvidenceWriter(func() *gorm.DB { return LOG_DB }, evidenceQueueCap, evidenceBatchMax, evidenceFlushInterval)
	w.maintenance = true
	return w
}()

// EnqueueUpstreamEvidence hands a row to the background writer. It never
// blocks: when persistence is disabled it returns false, and when the queue is
// full the row is dropped and counted.
func EnqueueUpstreamEvidence(e *UpstreamEvidence) bool {
	return defaultEvidenceWriter.enqueue(e)
}

// GetUpstreamEvidenceHealth reports the writer's counters.
func GetUpstreamEvidenceHealth() UpstreamEvidenceHealth {
	return defaultEvidenceWriter.health()
}

// FlushUpstreamEvidence writes whatever is queued, waiting at most timeout.
// Called on graceful shutdown.
func FlushUpstreamEvidence(timeout time.Duration) {
	defaultEvidenceWriter.flush(timeout)
}

func (w *evidenceWriter) enqueue(e *UpstreamEvidence) bool {
	w.startOnce.Do(w.start)
	if e == nil || !w.enabled.Load() {
		return false
	}
	size := e.approxBytes()
	if w.queued.Add(size) > w.byteBudget {
		w.queued.Add(-size)
		w.dropped.Add(1)
		return false
	}
	select {
	case w.queue <- e:
		return true
	default:
		w.queued.Add(-size)
		w.dropped.Add(1)
		return false
	}
}

// approxBytes estimates the memory a queued row holds. Computed before the
// writer sanitizes the row, so enqueue and dequeue agree on the value.
func (e *UpstreamEvidence) approxBytes() int64 {
	return int64(512 + len(e.ResponseExcerpt) + len(e.ResponseHeaders) + len(e.ModelFrame) + len(e.ErrorFrame) +
		len(e.Note) + len(e.RequestModel) + len(e.SentModel) + len(e.ResponseModel) + len(e.ResponseId))
}

// dequeued releases a row's share of the byte budget.
func (w *evidenceWriter) dequeued(e *UpstreamEvidence) *UpstreamEvidence {
	w.queued.Add(-e.approxBytes())
	return e
}

// start decides once whether persistence is possible (E6): a missing log
// database, an unsupported engine (ClickHouse) or a missing table disables
// it with a single warning instead of an error per request.
func (w *evidenceWriter) start() {
	w.started.Store(time.Now().Unix())
	w.index.Store("pending")
	db := w.db()
	reason := ""
	switch {
	case db == nil:
		reason = "log database not initialised"
	case common.UsingLogDatabase(common.DatabaseTypeClickHouse):
		reason = "log database engine does not support upstream_evidences"
	case !db.Migrator().HasTable(&UpstreamEvidence{}):
		reason = "table upstream_evidences does not exist"
	}
	if reason != "" {
		w.reason.Store(reason)
		w.index.Store("disabled")
		common.SysError("[upstream-audit] evidence persistence disabled: " + reason)
		return
	}
	w.enabled.Store(true)
	go w.run()
	if w.maintenance {
		go w.pruneLoop()
		go w.buildIndexes(evidenceIndexDelay)
	}
}

func (w *evidenceWriter) run() {
	batch := make([]*UpstreamEvidence, 0, w.batchMax)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case e := <-w.queue:
			batch = append(batch, w.dequeued(e))
			if len(batch) >= w.batchMax {
				batch = w.write(batch)
			}
		case <-ticker.C:
			batch = w.write(batch)
		case done := <-w.flushReq:
			for drained := false; !drained; {
				select {
				case e := <-w.queue:
					batch = append(batch, w.dequeued(e))
					if len(batch) >= w.batchMax {
						batch = w.write(batch)
					}
				default:
					drained = true
				}
			}
			batch = w.write(batch)
			close(done)
		}
	}
}

// write inserts a batch; if the batch insert fails, rows are retried one by
// one so a single bad row cannot take the rest of the batch with it.
func (w *evidenceWriter) write(batch []*UpstreamEvidence) []*UpstreamEvidence {
	if len(batch) == 0 {
		return batch
	}
	defer func() {
		if r := recover(); r != nil {
			w.failed.Add(int64(len(batch)))
			w.recordError(fmt.Sprintf("panic: %v", r))
		}
	}()
	for _, e := range batch {
		e.sanitize()
	}
	db := w.db()
	if err := db.CreateInBatches(batch, w.batchMax).Error; err == nil {
		w.inserted.Add(int64(len(batch)))
		return batch[:0]
	}
	for _, e := range batch {
		e.Id = 0
		if err := db.Create(e).Error; err != nil {
			w.failed.Add(1)
			w.recordError(err.Error())
			continue
		}
		w.inserted.Add(1)
	}
	return batch[:0]
}

// recordError keeps the last error for the health endpoint and logs at most
// once a minute, so an unreachable database cannot flood the system log.
func (w *evidenceWriter) recordError(msg string) {
	msg = redactDatabaseAddress(msg)
	now := time.Now().Unix()
	w.lastErr.Store(common.TruncateAuditRunes(msg, 500))
	w.lastErrAt.Store(now)
	if last := w.lastLogAt.Load(); now-last >= 60 && w.lastLogAt.CompareAndSwap(last, now) {
		common.SysError("[upstream-audit] evidence persist failed (failed_total=" +
			strconv.FormatInt(w.failed.Load(), 10) + "): " + msg)
	}
}

func (w *evidenceWriter) flush(timeout time.Duration) {
	if !w.enabled.Load() {
		return
	}
	done := make(chan struct{})
	select {
	case w.flushReq <- done:
	case <-time.After(timeout):
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (w *evidenceWriter) health() UpstreamEvidenceHealth {
	reason, _ := w.reason.Load().(string)
	lastErr, _ := w.lastErr.Load().(string)
	index, _ := w.index.Load().(string)
	if index == "" {
		index = "pending"
	}
	return UpstreamEvidenceHealth{
		Enabled:        w.enabled.Load(),
		DisabledReason: reason,
		InsertedTotal:  w.inserted.Load(),
		DroppedTotal:   w.dropped.Load(),
		FailedTotal:    w.failed.Load(),
		LastError:      lastErr,
		LastErrorAt:    w.lastErrAt.Load(),
		QueueLen:       len(w.queue),
		QueueCap:       cap(w.queue),
		QueueBytes:     w.queued.Load(),
		StartedAt:      w.started.Load(),
		IndexState:     index,
	}
}

// databaseAddressRe matches DSNs, host=/user=/password= parameters and
// host:port pairs that driver errors may embed.
var databaseAddressRe = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://\S+|\b(?:host|hostaddr|user|password|dbname)=\S+|@?\[?[a-z0-9.-]+\]?:\d{2,5}\b`)

// redactDatabaseAddress keeps error text useful for the health endpoint
// without exposing where the log database lives or how to log in.
func redactDatabaseAddress(msg string) string {
	return databaseAddressRe.ReplaceAllString(msg, "[redacted]")
}

// ── pruning ──────────────────────────────────────────────────────────────────

const (
	evidencePruneBatch = 5000
	evidencePrunePause = 200 * time.Millisecond
)

// pruneLoop deletes expired kind=normal rows every 6h. Each run is guarded by
// its own recover so one failure never ends the loop.
func (w *evidenceWriter) pruneLoop() {
	for {
		w.pruneOnce(time.Now().Add(-time.Duration(NormalEvidenceRetentionDays) * 24 * time.Hour).Unix())
		time.Sleep(6 * time.Hour)
	}
}

// pruneOnce deletes expired normal rows in batches of evidencePruneBatch
// (oldest first, by primary key) with a short pause between batches, so a
// large backlog never becomes one long-running DELETE.
func (w *evidenceWriter) pruneOnce(cutoff int64) (deleted int64) {
	defer func() {
		if r := recover(); r != nil {
			common.SysError(fmt.Sprintf("[upstream-audit] evidence prune panic recovered after %d rows: %v", deleted, r))
		}
	}()
	db := w.db()
	if db == nil {
		return 0
	}
	for batchNo := 1; ; batchNo++ {
		var ids []int
		if err := db.Model(&UpstreamEvidence{}).
			Where("kind = ? AND created_at < ?", "normal", cutoff).
			Order("id").Limit(evidencePruneBatch).
			Pluck("id", &ids).Error; err != nil {
			common.SysError("[upstream-audit] evidence prune select failed: " + err.Error())
			return deleted
		}
		if len(ids) == 0 {
			break
		}
		res := db.Where("id IN ?", ids).Delete(&UpstreamEvidence{})
		if res.Error != nil {
			common.SysError("[upstream-audit] evidence prune delete failed: " + res.Error.Error())
			return deleted
		}
		deleted += res.RowsAffected
		if batchNo%20 == 0 {
			common.SysLog("[upstream-audit] pruning expired normal evidence: " + strconv.FormatInt(deleted, 10) + " rows so far")
		}
		if len(ids) < evidencePruneBatch {
			break
		}
		time.Sleep(evidencePrunePause)
	}
	if deleted > 0 {
		common.SysLog("[upstream-audit] pruned " + strconv.FormatInt(deleted, 10) + " expired normal evidence rows")
	}
	return deleted
}

// ── background secondary indexes ─────────────────────────────────────────────

// upstreamEvidenceIndexes serve the replay lookups by (channel, response id)
// and (channel, output hash).
var upstreamEvidenceIndexes = []struct{ name, columns string }{
	{"idx_upstream_evidences_channel_response_id", "channel_id, response_id"},
	{"idx_upstream_evidences_channel_output_sha256", "channel_id, output_sha256"},
}

// buildIndexes creates the secondary indexes after a delay, outside startup.
// On PostgreSQL the build is CREATE INDEX CONCURRENTLY, which never blocks
// reads or writes on the (very large) table; an INVALID leftover from an
// interrupted build is dropped concurrently and rebuilt. MySQL uses an online
// (INPLACE, LOCK=NONE) build; SQLite a plain CREATE INDEX IF NOT EXISTS.
func (w *evidenceWriter) buildIndexes(delay time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			w.index.Store(fmt.Sprintf("failed: panic: %v", r))
			common.SysError(fmt.Sprintf("[upstream-audit] evidence index build panic recovered: %v", r))
		}
	}()
	time.Sleep(delay)
	db := w.db()
	if db == nil {
		return
	}
	w.index.Store("building")
	if err := ensureUpstreamEvidenceIndexes(db); err != nil {
		w.index.Store("failed: " + common.TruncateAuditRunes(err.Error(), 300))
		common.SysError("[upstream-audit] evidence index build failed: " + err.Error())
		return
	}
	w.index.Store("ready")
}

// upstreamEvidenceIndexLockKey is the PostgreSQL advisory lock that keeps
// several gateway instances from building (or dropping) the indexes at once.
const upstreamEvidenceIndexLockKey = 7423190417

func ensureUpstreamEvidenceIndexes(db *gorm.DB) error {
	if common.UsingLogDatabase(common.DatabaseTypePostgreSQL) {
		// one pinned session: the advisory lock belongs to the connection
		return db.Connection(ensureUpstreamEvidenceIndexesPostgres)
	}
	table := "upstream_evidences"
	for _, idx := range upstreamEvidenceIndexes {
		if common.UsingLogDatabase(common.DatabaseTypeMySQL) {
			if db.Migrator().HasIndex(&UpstreamEvidence{}, idx.name) {
				continue
			}
			common.SysLog("[upstream-audit] building index " + idx.name + " online")
			if err := db.Exec("CREATE INDEX " + idx.name + " ON " + table + " (" + idx.columns + ") ALGORITHM=INPLACE LOCK=NONE").Error; err != nil {
				return err
			}
			continue
		}
		if err := db.Exec("CREATE INDEX IF NOT EXISTS " + idx.name + " ON " + table + " (" + idx.columns + ")").Error; err != nil {
			return err
		}
	}
	return nil
}

// ensureUpstreamEvidenceIndexesPostgres builds the indexes with CREATE INDEX
// CONCURRENTLY under an advisory lock. An INVALID index is only dropped and
// rebuilt when no build of it is in progress anywhere (an instance without
// the lock, e.g. an older release, may still be building it).
func ensureUpstreamEvidenceIndexesPostgres(conn *gorm.DB) error {
	var locked bool
	if err := conn.Raw("SELECT pg_try_advisory_lock(?)", upstreamEvidenceIndexLockKey).Scan(&locked).Error; err != nil {
		return err
	}
	if !locked {
		common.SysLog("[upstream-audit] another instance holds the evidence index lock; skipping index build")
		return nil
	}
	defer conn.Exec("SELECT pg_advisory_unlock(?)", upstreamEvidenceIndexLockKey)
	for _, idx := range upstreamEvidenceIndexes {
		var valid []bool
		if err := conn.Raw("SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = ?", idx.name).
			Scan(&valid).Error; err != nil {
			return err
		}
		if len(valid) > 0 && valid[0] {
			continue
		}
		if len(valid) > 0 {
			var building []int64
			if err := conn.Raw("SELECT p.pid FROM pg_stat_progress_create_index p JOIN pg_class c ON c.oid = p.index_relid WHERE c.relname = ?", idx.name).
				Scan(&building).Error; err != nil {
				return err // cannot tell whether a build is running (PostgreSQL < 12): leave it alone
			}
			if len(building) > 0 {
				common.SysLog("[upstream-audit] index " + idx.name + " is being built by another session; not touching it")
				continue
			}
			common.SysLog("[upstream-audit] dropping invalid index " + idx.name + " left by an interrupted build")
			if err := conn.Exec("DROP INDEX CONCURRENTLY IF EXISTS " + idx.name).Error; err != nil {
				return err
			}
		}
		common.SysLog("[upstream-audit] building index " + idx.name + " concurrently")
		if err := conn.Exec("CREATE INDEX CONCURRENTLY IF NOT EXISTS " + idx.name + " ON upstream_evidences (" + idx.columns + ")").Error; err != nil {
			return err
		}
	}
	return nil
}
