package model

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ChannelQuotaWindow is the durable copy of one channel budget window (one
// day or one month of one channel). Without Redis the windows live in
// process memory, so a restart or deploy would hand every channel its whole
// daily and monthly budget again; each charge on a channel with a budget is
// therefore also added here, and startup loads the unexpired windows back.
// With Redis the windows are kept in Redis and this table is not used.
type ChannelQuotaWindow struct {
	CounterKey string `json:"counter_key" gorm:"primaryKey;type:varchar(64)"`
	Quota      int64  `json:"quota" gorm:"not null;default:0"`
	ExpiresAt  int64  `json:"expires_at" gorm:"index;not null"`
}

// persistChannelQuotaDeltas adds the deltas to the durable windows with the
// same rule as the counter store: a window never drops below zero.
func persistChannelQuotaDeltas(deltas []common.CounterDelta) error {
	now := time.Now()
	return DB.Transaction(func(tx *gorm.DB) error {
		for _, delta := range deltas {
			row := ChannelQuotaWindow{
				CounterKey: delta.Key,
				Quota:      max(delta.Delta, 0),
				ExpiresAt:  now.Add(delta.TTL).Unix(),
			}
			err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "counter_key"}},
				DoUpdates: clause.Assignments(map[string]any{
					"quota": gorm.Expr("CASE WHEN channel_quota_windows.quota + ? < 0 THEN 0 ELSE channel_quota_windows.quota + ? END",
						delta.Delta, delta.Delta),
					"expires_at": row.ExpiresAt,
				}),
			}).Create(&row).Error
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// RestoreChannelQuotaWindows loads the unexpired budget windows into the
// in-memory counter store and deletes expired rows. Call it once at startup,
// after the database and Redis are initialised; it does nothing with Redis.
func RestoreChannelQuotaWindows() {
	if common.RedisEnabled {
		return
	}
	now := time.Now().Unix()
	if err := DB.Where("expires_at <= ?", now).Delete(&ChannelQuotaWindow{}).Error; err != nil {
		common.SysError(fmt.Sprintf("failed to delete expired channel quota windows: %v", err))
	}
	var rows []ChannelQuotaWindow
	if err := DB.Where("expires_at > ?", now).Find(&rows).Error; err != nil {
		common.SysError(fmt.Sprintf("failed to load channel quota windows: %v", err))
		return
	}
	values := make([]common.CounterRestore, 0, len(rows))
	for _, row := range rows {
		values = append(values, common.CounterRestore{Key: row.CounterKey, Value: row.Quota, ExpiresAt: time.Unix(row.ExpiresAt, 0)})
	}
	common.RestoreMemoryCounters(values)
	common.SysLog(fmt.Sprintf("restored %d channel quota windows", len(values)))
}
