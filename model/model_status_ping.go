package model

import (
	"github.com/samber/lo"
	"gorm.io/gorm/clause"
)

// Per-ping status enum, matching the OpenStatus presentation taxonomy 1:1
// (success | degraded | error | empty). Stored as the lib's enum string at
// write time so reads need no derivation.
const (
	ModelStatusSuccess  = "success"
	ModelStatusDegraded = "degraded"
	ModelStatusError    = "error"
	ModelStatusEmpty    = "empty"
)

// ModelStatusPing is one snapshot per (model, minute). The single fact table
// for status. Status is computed at write time. Aggregations into 1m/5m/15m/
// 1h/1d buckets happen at read time via AggregateBuckets.
type ModelStatusPing struct {
	Model         string `json:"model" gorm:"type:varchar(255);primaryKey"`
	Timestamp     int64  `json:"timestamp" gorm:"bigint;primaryKey;index:idx_status_ts_model,priority:1"`
	Status        string `json:"status" gorm:"type:varchar(16);index"`
	UpChannels    int    `json:"up_channels"`
	TotalChannels int    `json:"total_channels"`
	LatencyMs     int    `json:"latency_ms"`
	RequestCount  int    `json:"request_count"`
	ErrorCount    int    `json:"error_count"`
	P50LatencyMs  int    `json:"p50_latency_ms"`
	P95LatencyMs  int    `json:"p95_latency_ms"`
}

func (ModelStatusPing) TableName() string {
	return "model_status_pings"
}

// ComputeModelStatus derives the per-minute status enum from channel counts.
// "Degraded" is reserved for genuinely diminished capacity (less than half the
// channels up). A 5/6 model still has plenty of headroom and reads as healthy;
// a 1/3 model is degraded; a 0/n model is down.
func ComputeModelStatus(upChannels, totalChannels int) string {
	switch {
	case totalChannels == 0:
		return ModelStatusEmpty
	case upChannels == 0:
		return ModelStatusError
	case upChannels*2 < totalChannels:
		return ModelStatusDegraded
	default:
		return ModelStatusSuccess
	}
}

func InsertModelStatusPings(rows []*ModelStatusPing) error {
	if len(rows) == 0 {
		return nil
	}
	for _, chunk := range lo.Chunk(rows, 100) {
		err := DB.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "model"}, {Name: "timestamp"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"status", "up_channels", "total_channels", "latency_ms",
				"request_count", "error_count", "p50_latency_ms", "p95_latency_ms",
			}),
		}).Create(&chunk).Error
		if err != nil {
			return err
		}
	}
	return nil
}

func PruneModelStatusPingsBefore(beforeTs int64) error {
	return DB.Where("timestamp < ?", beforeTs).Delete(&ModelStatusPing{}).Error
}

// LatestPingByModel returns the most recent ping per model (used by the
// /components endpoint to show "current status").
func LatestPingByModel() (map[string]*ModelStatusPing, error) {
	var latestTs int64
	if err := DB.Model(&ModelStatusPing{}).
		Select("MAX(timestamp)").
		Scan(&latestTs).Error; err != nil {
		return nil, err
	}
	if latestTs == 0 {
		return map[string]*ModelStatusPing{}, nil
	}
	var rows []*ModelStatusPing
	if err := DB.Where("timestamp = ?", latestTs).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]*ModelStatusPing, len(rows))
	for _, r := range rows {
		out[r.Model] = r
	}
	return out, nil
}

// BucketRow is one aggregated time window for one model. Lives only in
// memory (computed by AggregateBuckets).
//
// AVG() returns NUMERIC/DECIMAL on Postgres and FLOAT on SQLite/MySQL, so the
// latency averages MUST be float64 to scan portably. Callers round to int when
// rendering.
type BucketRow struct {
	BucketStart  int64   `gorm:"column:bucket_start"`
	Count        int     `gorm:"column:cnt"`
	Ok           int     `gorm:"column:ok"`
	Degraded     int     `gorm:"column:degraded"`
	ErrorCnt     int     `gorm:"column:err"`
	Empty        int     `gorm:"column:empty_cnt"`
	AvgLatencyMs float64 `gorm:"column:avg_latency"`
	P50LatencyMs float64 `gorm:"column:avg_p50"`
	P95LatencyMs float64 `gorm:"column:avg_p95"`
	RequestSum   int     `gorm:"column:req_sum"`
	ErrorSum     int     `gorm:"column:err_sum"`
}

// AggregateBuckets groups pings into windows of bucketSeconds for one model
// since `since` (unix seconds, inclusive). Cross-DB compatible: uses
// SUM(CASE ...) instead of FILTER (Postgres-only) and integer truncation
// instead of date_trunc.
func AggregateBuckets(modelName string, bucketSeconds int64, since int64) ([]*BucketRow, error) {
	var rows []*BucketRow
	err := DB.Table("model_status_pings").
		Select(`
			(timestamp / ?) * ? AS bucket_start,
			COUNT(*) AS cnt,
			SUM(CASE WHEN status = 'success'  THEN 1 ELSE 0 END) AS ok,
			SUM(CASE WHEN status = 'degraded' THEN 1 ELSE 0 END) AS degraded,
			SUM(CASE WHEN status = 'error'    THEN 1 ELSE 0 END) AS err,
			SUM(CASE WHEN status = 'empty'    THEN 1 ELSE 0 END) AS empty_cnt,
			AVG(latency_ms)      AS avg_latency,
			AVG(p50_latency_ms)  AS avg_p50,
			AVG(p95_latency_ms)  AS avg_p95,
			SUM(request_count)   AS req_sum,
			SUM(error_count)     AS err_sum
		`, bucketSeconds, bucketSeconds).
		Where("model = ? AND timestamp >= ?", modelName, since).
		Group("bucket_start").
		Order("bucket_start ASC").
		Scan(&rows).Error
	return rows, err
}
