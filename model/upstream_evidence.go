package model

// UpstreamEvidence stores the full forensic record for an upstream response
// whose declared model does not match the model we sent (model substitution /
// channel adulteration evidence). Rows are only written on mismatch, so the
// table stays small; the response excerpt is capped at 64KB upstream.
type UpstreamEvidence struct {
	Id        int   `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt int64 `json:"created_at" gorm:"bigint;index"`
	// Kind: model_mismatch | provider_switch
	Kind string `json:"kind" gorm:"type:varchar(32);index;default:model_mismatch"`
	// Note: human-readable context (e.g. "azure -> gpt_relay" for switches)
	Note string `json:"note" gorm:"type:varchar(255)"`
	// Source: inferred upstream source label at record time
	Source    string `json:"source" gorm:"type:varchar(32)"`
	ChannelId int    `json:"channel_id" gorm:"index"`
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

func InsertUpstreamEvidence(e *UpstreamEvidence) error {
	return LOG_DB.Create(e).Error
}
