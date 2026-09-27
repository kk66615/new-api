package audit_setting

import "github.com/QuantumNous/new-api/setting/config"

// Excerpt retention modes for kind=normal upstream evidence rows.
const (
	// NormalExcerptModeFull keeps the full (64KB-capped) excerpt on every
	// normal row. Default: the owner decides later whether to switch.
	NormalExcerptModeFull = "full"
	// NormalExcerptModeDigest keeps only hashes plus the first and last 2KB
	// of a normal row's excerpt and model frame; anomaly rows always keep the
	// full excerpt.
	NormalExcerptModeDigest = "digest"
)

// AuditSetting is stored as options "audit_setting.<field>"; the excerpt mode
// is the option key audit_setting.normal_excerpt_mode.
type AuditSetting struct {
	NormalExcerptMode string `json:"normal_excerpt_mode"`
}

var auditSetting = AuditSetting{
	NormalExcerptMode: NormalExcerptModeFull,
}

func init() {
	config.GlobalConfig.Register("audit_setting", &auditSetting)
}

// NormalExcerptMode returns the configured mode; anything but "digest" is
// treated as "full", so a typo can never silently drop evidence.
func NormalExcerptMode() string {
	if auditSetting.NormalExcerptMode == NormalExcerptModeDigest {
		return NormalExcerptModeDigest
	}
	return NormalExcerptModeFull
}
