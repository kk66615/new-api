package model

import (
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// NormalEvidenceRetentionDays: kind='normal' rows (full response retention for
// every clean request — user decision 2026-09-02) are pruned after this many
// days. Anomaly rows (model_mismatch / provider_switch) are kept forever.
const NormalEvidenceRetentionDays = 30

// UpstreamEvidence stores the forensic record for upstream responses:
// kind=normal      — every clean request (full response body + headers)
// kind=model_mismatch  — upstream declared a different model than we sent
// kind=provider_switch — the inferred upstream source flipped for a channel
// The response excerpt is capped at 64KB upstream.
type UpstreamEvidence struct {
	Id        int   `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt int64 `json:"created_at" gorm:"bigint;index"`
	// Kind: normal | model_mismatch | provider_switch
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
}

var evidencePruneOnce sync.Once

var errUpstreamEvidenceNoDB = errors.New("upstream evidence: log database not initialised")

func InsertUpstreamEvidence(e *UpstreamEvidence) error {
	if LOG_DB == nil {
		// Never dereference a nil *gorm.DB: unit tests that exercise the relay
		// handlers without a database would otherwise fault inside gorm, and a
		// recovered nil-pointer fault in that path has been observed to wedge
		// the test binary rather than unwind.
		return errUpstreamEvidenceNoDB
	}
	evidencePruneOnce.Do(startEvidencePruner)
	return LOG_DB.Create(e).Error
}

// startEvidencePruner runs every 6h and deletes expired kind=normal rows.
// Started lazily on first insert so LOG_DB is guaranteed to be initialized.
func startEvidencePruner() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				common.SysError("evidence pruner panic recovered")
			}
		}()
		prune := func() {
			if LOG_DB == nil {
				return
			}
			cutoff := time.Now().Add(-time.Duration(NormalEvidenceRetentionDays) * 24 * time.Hour).Unix()
			res := LOG_DB.Where("kind = ? AND created_at < ?", "normal", cutoff).Delete(&UpstreamEvidence{})
			if res.Error != nil {
				common.SysError("evidence prune failed: " + res.Error.Error())
			} else if res.RowsAffected > 0 {
				common.SysLog("[upstream-audit] pruned " + strconv.FormatInt(res.RowsAffected, 10) + " expired normal evidence rows")
			}
		}
		prune()
		for range time.Tick(6 * time.Hour) {
			prune()
		}
	}()
}
