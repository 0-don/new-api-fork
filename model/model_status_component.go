package model

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm/clause"
)

// ModelStatusComponent is the page-level row shown on the public status page,
// one per model. Auto-created by the snapshot worker on first probe; admin
// can later edit Description / GroupId / SortOrder / Public.
type ModelStatusComponent struct {
	Id          int    `json:"id" gorm:"primaryKey;autoIncrement"`
	ModelName   string `json:"name" gorm:"type:varchar(255);uniqueIndex"`
	Description string `json:"description" gorm:"type:text"`
	GroupId     *int   `json:"group_id,omitempty" gorm:"index"`
	SortOrder   int    `json:"sort_order" gorm:"default:0"`
	Public      bool   `json:"public" gorm:"default:true"`
	CreatedAt   int64  `json:"created_at" gorm:"bigint"`
}

func (ModelStatusComponent) TableName() string {
	return "model_status_components"
}

// UpsertModelStatusComponents inserts a component row for any model not yet
// known. Existing rows are left untouched (admin edits to description etc.
// are preserved).
func UpsertModelStatusComponents(modelNames []string) error {
	if len(modelNames) == 0 {
		return nil
	}
	now := common.GetTimestamp()
	rows := make([]*ModelStatusComponent, 0, len(modelNames))
	for _, name := range modelNames {
		rows = append(rows, &ModelStatusComponent{
			ModelName: name,
			Public:    true,
			CreatedAt: now,
		})
	}
	return DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
}

func GetAllPublicModelStatusComponents() ([]*ModelStatusComponent, error) {
	var rows []*ModelStatusComponent
	err := DB.Where("public = ?", true).
		Order("sort_order ASC, model_name ASC").
		Find(&rows).Error
	return rows, err
}

// GetComponentByModel resolves a component row for a model name. Used by the
// incident state machine in the worker.
func GetComponentByModel(modelName string) (*ModelStatusComponent, error) {
	var row ModelStatusComponent
	err := DB.Where("model_name = ?", modelName).First(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// DeleteModelStatusComponentsNotIn removes component rows whose model_name is
// not in the given active set. Caller MUST guard against an empty slice; an
// empty active set is treated as a no-op to avoid wiping the table when the
// snapshot worker temporarily fails to enumerate channels.
func DeleteModelStatusComponentsNotIn(activeModels []string) error {
	if len(activeModels) == 0 {
		return nil
	}
	return DB.Where("model_name NOT IN ?", activeModels).
		Delete(&ModelStatusComponent{}).Error
}
