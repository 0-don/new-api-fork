package model

const (
	ModelStatusEventIncident    = "incident"
	ModelStatusEventReport      = "report"
	ModelStatusEventMaintenance = "maintenance"
)

// ModelStatusIncident is an auto-detected outage window for a component.
// Opened by the worker when a ping is "error" and no open incident exists;
// closed when the next non-error ping arrives.
type ModelStatusIncident struct {
	Id           int    `json:"id" gorm:"primaryKey;autoIncrement"`
	ComponentId  int    `json:"component_id" gorm:"index;not null"`
	Title        string `json:"title" gorm:"type:varchar(255)"`
	StartedAt    int64  `json:"started_at" gorm:"bigint;index"`
	ResolvedAt   *int64 `json:"resolved_at" gorm:"bigint;index"`
	AutoResolved bool   `json:"auto_resolved" gorm:"default:true"`
	EventType    string `json:"event_type" gorm:"type:varchar(16);default:'incident'"`
}

func (ModelStatusIncident) TableName() string {
	return "model_status_incidents"
}

// GetOpenIncidentByComponent returns the currently-open incident for a
// component, or (nil, nil) if there is none.
func GetOpenIncidentByComponent(componentId int) (*ModelStatusIncident, error) {
	var row ModelStatusIncident
	err := DB.Where("component_id = ? AND resolved_at IS NULL", componentId).
		Order("started_at DESC").
		First(&row).Error
	if err != nil {
		// Treat "not found" as nil; let caller distinguish via the returned ptr.
		if err.Error() == "record not found" {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

func OpenIncident(componentId int, title string, startedAt int64) error {
	row := &ModelStatusIncident{
		ComponentId:  componentId,
		Title:        title,
		StartedAt:    startedAt,
		AutoResolved: true,
		EventType:    ModelStatusEventIncident,
	}
	return DB.Create(row).Error
}

func ResolveIncident(id int, resolvedAt int64) error {
	return DB.Model(&ModelStatusIncident{}).
		Where("id = ?", id).
		Update("resolved_at", resolvedAt).
		Error
}

// ListIncidentsBetween returns incidents that overlap the half-open window
// [from, to). Includes ongoing incidents (resolved_at IS NULL).
func ListIncidentsBetween(from, to int64, modelName string) ([]*ModelStatusIncident, error) {
	q := DB.Model(&ModelStatusIncident{}).
		Where("started_at < ?", to).
		Where("(resolved_at IS NULL OR resolved_at >= ?)", from)

	if modelName != "" {
		// Resolve component_id by model name first to keep this cross-DB simple.
		var compId int
		if err := DB.Model(&ModelStatusComponent{}).
			Select("id").
			Where("model_name = ?", modelName).
			Scan(&compId).Error; err != nil {
			return nil, err
		}
		if compId == 0 {
			return []*ModelStatusIncident{}, nil
		}
		q = q.Where("component_id = ?", compId)
	}

	var rows []*ModelStatusIncident
	err := q.Order("started_at ASC").Find(&rows).Error
	return rows, err
}

// ListIncidentsByComponentBetween is the typed sibling used by the bucket
// renderer (one component, one window).
func ListIncidentsByComponentBetween(componentId int, from, to int64) ([]*ModelStatusIncident, error) {
	var rows []*ModelStatusIncident
	err := DB.Where("component_id = ?", componentId).
		Where("started_at < ?", to).
		Where("(resolved_at IS NULL OR resolved_at >= ?)", from).
		Order("started_at ASC").
		Find(&rows).Error
	return rows, err
}
