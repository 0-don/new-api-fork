package controller

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/go-fuego/fuego"
)

// ----- Response DTOs (match the OpenStatus lib's TypeScript types verbatim) -----

// ComponentDTO maps to the StatusComponent props on the lib side.
type ComponentDTO struct {
	Id             int     `json:"id"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	GroupId        *int    `json:"group_id,omitempty"`
	Status         string  `json:"status"`     // success|degraded|error|empty
	UpChannels     int     `json:"up_channels"`
	TotalChannels  int     `json:"total_channels"`
	ProbeLatencyMs int     `json:"probe_latency_ms"`
	Uptime24h      float64 `json:"uptime_24h"`
	Uptime30d      float64 `json:"uptime_30d"`
	OpenIncidentId *int    `json:"open_incident_id,omitempty"`
	SampledAt      int64   `json:"sampled_at"`
}

// EventDTO matches StatusBarData.events item: {id, name, type, from, to, isAggregated?}.
// `From`/`To` are RFC3339 (ISO 8601) strings so the JS side can `new Date(s)` /
// `dayjs(s)` directly without a millisecond multiplication step at the boundary.
type EventDTO struct {
	Id           int     `json:"id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	From         string  `json:"from"`
	To           *string `json:"to"`
	IsAggregated bool    `json:"is_aggregated,omitempty"`
}

// BarSegmentDTO is one stacked segment within a single bucket; heights sum to 100.
type BarSegmentDTO struct {
	Status string `json:"status"`
	Height int    `json:"height"`
}

// CardItemDTO is one row in the hover card breakdown.
type CardItemDTO struct {
	Status string `json:"status"`
	Value  string `json:"value"`
}

// StatusBarDataDTO matches StatusBarData[] verbatim. `Day` is an ISO timestamp
// of the bucket start; the lib accepts any ISO string (not just calendar days).
type StatusBarDataDTO struct {
	Day    string          `json:"day"`
	Bar    []BarSegmentDTO `json:"bar"`
	Card   []CardItemDTO   `json:"card"`
	Events []EventDTO      `json:"events"`
}

// PageDTO is the single-shot payload for the status page render.
type PageDTO struct {
	Components []ComponentDTO                `json:"components"`
	Bars       map[string][]StatusBarDataDTO `json:"bars"`
	Incidents  []EventDTO                    `json:"incidents"`
}

// CompactBucket is one bucket as a fixed-length tuple. Field order is locked
// by the client decoder in unorouter: [ok, deg, err, empty, reqSum, errSum, p95].
// Indices in the parent array map to bucket start = bucketStart + i*bucketSec.
type CompactBucket = [7]int

// CompactBarDTO carries one model's buckets and its incident overlay. Events
// are a sparse map keyed by bucket index (string for JSON) -> incident ids that
// overlap that bucket. Incident metadata lives once at the top level.
type CompactBarDTO struct {
	Buckets [][7]int         `json:"buckets"`
	Events  map[string][]int `json:"events,omitempty"`
}

// CompactPageDTO is the compact wire format for /page when ?compact=1. Cuts
// payload ~10x by dropping per-bucket strings, RFC3339 timestamps, and empty
// arrays. Client reconstructs StatusBarData[] from this.
type CompactPageDTO struct {
	Components  []ComponentDTO            `json:"components"`
	Incidents   []EventDTO                `json:"incidents"`
	BucketStart int64                     `json:"bucket_start"`
	BucketSec   int64                     `json:"bucket_sec"`
	BucketCount int                       `json:"bucket_count"`
	Bars        map[string]*CompactBarDTO `json:"bars"`
}

// ----- Bucket parsing -----

var bucketSeconds = map[string]int64{
	"1m":  60,
	"5m":  300,
	"15m": 900,
	"1h":  3600,
	"1d":  86400,
}

func resolveBucketSeconds(bucket string) int64 {
	if v, ok := bucketSeconds[bucket]; ok {
		return v
	}
	return bucketSeconds["15m"]
}

// ----- Handlers -----

// GET /api/model_status/components
func GetModelStatusComponents(c fuego.ContextNoBody) (*dto.Response[[]ComponentDTO], error) {
	comps, err := model.GetAllPublicModelStatusComponents()
	if err != nil {
		return dto.Fail[[]ComponentDTO](err.Error())
	}
	latest, err := model.LatestPingByModel()
	if err != nil {
		return dto.Fail[[]ComponentDTO](err.Error())
	}
	now := time.Now().Unix()
	since24h := now - 24*60*60
	since30d := now - 30*24*60*60

	modelNames := make([]string, 0, len(comps))
	componentIds := make([]int, 0, len(comps))
	for _, comp := range comps {
		modelNames = append(modelNames, comp.ModelName)
		componentIds = append(componentIds, comp.Id)
	}

	uptime24h, err := model.UptimeByModelSince(modelNames, since24h)
	if err != nil {
		return dto.Fail[[]ComponentDTO](err.Error())
	}
	uptime30d, err := model.UptimeByModelSince(modelNames, since30d)
	if err != nil {
		return dto.Fail[[]ComponentDTO](err.Error())
	}
	openIncidents, err := model.OpenIncidentsByComponent(componentIds)
	if err != nil {
		return dto.Fail[[]ComponentDTO](err.Error())
	}

	items := make([]ComponentDTO, 0, len(comps))
	for _, comp := range comps {
		dto := ComponentDTO{
			Id:          comp.Id,
			Name:        comp.ModelName,
			Description: comp.Description,
			GroupId:     comp.GroupId,
			Status:      model.ModelStatusEmpty,
		}
		if p, ok := latest[comp.ModelName]; ok {
			dto.Status = p.Status
			dto.UpChannels = p.UpChannels
			dto.TotalChannels = p.TotalChannels
			dto.ProbeLatencyMs = p.LatencyMs
			dto.SampledAt = p.Timestamp
		}
		dto.Uptime24h = uptime24h[comp.ModelName]
		dto.Uptime30d = uptime30d[comp.ModelName]
		if id, ok := openIncidents[comp.Id]; ok {
			idCopy := id
			dto.OpenIncidentId = &idCopy
		}
		items = append(items, dto)
	}
	return dtoOk(items)
}

// GET /api/model_status/buckets?model=X&bucket=15m&hours=24
func GetModelStatusBuckets(c fuego.ContextWithParams[dto.GetModelStatusBucketsParams]) (*dto.Response[[]StatusBarDataDTO], error) {
	p, _ := dto.ParseParams[dto.GetModelStatusBucketsParams](c)
	if p.Model == "" {
		return dto.Fail[[]StatusBarDataDTO]("model is required")
	}
	hours := p.Hours
	if hours <= 0 {
		hours = 24
	}
	if hours > 720 {
		hours = 720
	}
	bucketSec := resolveBucketSeconds(p.Bucket)
	since := time.Now().Unix() - int64(hours)*60*60

	rows, err := model.AggregateBuckets(p.Model, bucketSec, since)
	if err != nil {
		return dto.Fail[[]StatusBarDataDTO](err.Error())
	}

	// Look up the component once for incident overlay. Missing component =>
	// no events (model not yet probed).
	var componentId int
	if comp, _ := model.GetComponentByModel(p.Model); comp != nil {
		componentId = comp.Id
	}

	items := make([]StatusBarDataDTO, 0, len(rows))
	for _, r := range rows {
		bucketEnd := r.BucketStart + bucketSec
		item := StatusBarDataDTO{
			Day:    time.Unix(r.BucketStart, 0).UTC().Format(time.RFC3339),
			Bar:    buildBarSegments(r),
			Card:   buildCardItems(r),
			Events: []EventDTO{},
		}
		if componentId != 0 {
			incidents, _ := model.ListIncidentsByComponentBetween(componentId, r.BucketStart, bucketEnd)
			for _, inc := range incidents {
				item.Events = append(item.Events, incidentToEvent(inc))
			}
		}
		items = append(items, item)
	}
	return dtoOk(items)
}

// buildBarSegments converts a BucketRow into bar segments summing to 100.
// Zero-height segments are omitted to keep the JSON small.
func buildBarSegments(r *model.BucketRow) []BarSegmentDTO {
	if r.Count == 0 {
		return []BarSegmentDTO{{Status: model.ModelStatusEmpty, Height: 100}}
	}
	pct := func(n int) int { return (n * 100) / r.Count }
	segs := []BarSegmentDTO{}
	if h := pct(r.Ok); h > 0 {
		segs = append(segs, BarSegmentDTO{Status: model.ModelStatusSuccess, Height: h})
	}
	if h := pct(r.Degraded); h > 0 {
		segs = append(segs, BarSegmentDTO{Status: model.ModelStatusDegraded, Height: h})
	}
	if h := pct(r.ErrorCnt); h > 0 {
		segs = append(segs, BarSegmentDTO{Status: model.ModelStatusError, Height: h})
	}
	if h := pct(r.Empty); h > 0 {
		segs = append(segs, BarSegmentDTO{Status: model.ModelStatusEmpty, Height: h})
	}
	// Pad the last segment so heights sum to exactly 100 (rounding fix).
	if len(segs) > 0 {
		var sum int
		for _, s := range segs {
			sum += s.Height
		}
		if diff := 100 - sum; diff != 0 {
			segs[len(segs)-1].Height += diff
		}
	}
	return segs
}

// buildCardItems formats per-bucket metrics for the hover card.
func buildCardItems(r *model.BucketRow) []CardItemDTO {
	items := []CardItemDTO{}
	if r.Ok > 0 {
		items = append(items, CardItemDTO{
			Status: model.ModelStatusSuccess,
			Value:  fmt.Sprintf("%d min", r.Ok),
		})
	}
	if r.Degraded > 0 {
		items = append(items, CardItemDTO{
			Status: model.ModelStatusDegraded,
			Value:  fmt.Sprintf("%d min", r.Degraded),
		})
	}
	if r.ErrorCnt > 0 {
		items = append(items, CardItemDTO{
			Status: model.ModelStatusError,
			Value:  fmt.Sprintf("%d min", r.ErrorCnt),
		})
	}
	if r.RequestSum > 0 || r.ErrorSum > 0 {
		latency := ""
		if r.P95LatencyMs > 0 {
			latency = fmt.Sprintf(" / p95 %s", formatMs(int(r.P95LatencyMs)))
		}
		items = append(items, CardItemDTO{
			Status: model.ModelStatusSuccess,
			Value:  fmt.Sprintf("%d req / %d err%s", r.RequestSum, r.ErrorSum, latency),
		})
	}
	return items
}

func formatMs(ms int) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

func incidentToEvent(inc *model.ModelStatusIncident) EventDTO {
	var to *string
	if inc.ResolvedAt != nil {
		s := time.Unix(*inc.ResolvedAt, 0).UTC().Format(time.RFC3339)
		to = &s
	}
	return EventDTO{
		Id:   inc.Id,
		Name: inc.Title,
		Type: inc.EventType,
		From: time.Unix(inc.StartedAt, 0).UTC().Format(time.RFC3339),
		To:   to,
	}
}

// GET /api/model_status/incidents?since=...&until=...&model=...
func GetModelStatusIncidents(c fuego.ContextWithParams[dto.GetModelStatusIncidentsParams]) (*dto.Response[[]EventDTO], error) {
	p, _ := dto.ParseParams[dto.GetModelStatusIncidentsParams](c)
	now := time.Now().Unix()
	since := p.Since
	if since == 0 {
		since = now - 24*60*60
	}
	until := p.Until
	if until == 0 {
		until = now
	}
	rows, err := model.ListIncidentsBetween(since, until, p.Model)
	if err != nil {
		return dto.Fail[[]EventDTO](err.Error())
	}
	out := make([]EventDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, incidentToEvent(r))
	}
	return dtoOk(out)
}

// GET /api/model_status/page?bucket=15m&hours=24
// Single round-trip: components + buckets per public component + recent incidents.
// Bucket size and history window are client-controlled.
func GetModelStatusPage(c fuego.ContextWithParams[dto.GetModelStatusPageParams]) (*dto.Response[PageDTO], error) {
	p, _ := dto.ParseParams[dto.GetModelStatusPageParams](c)

	hours := p.Hours
	if hours <= 0 {
		hours = 24
	}
	if hours > 720 {
		hours = 720
	}
	bucketSec := resolveBucketSeconds(p.Bucket)

	comps, err := model.GetAllPublicModelStatusComponents()
	if err != nil {
		return dto.Fail[PageDTO](err.Error())
	}
	latest, err := model.LatestPingByModel()
	if err != nil {
		return dto.Fail[PageDTO](err.Error())
	}

	now := time.Now().Unix()
	since := now - int64(hours)*60*60
	since24h := now - 24*60*60
	since30d := now - 30*24*60*60

	modelNames := make([]string, 0, len(comps))
	for _, comp := range comps {
		modelNames = append(modelNames, comp.ModelName)
	}

	uptime24h, err := model.UptimeByModelSince(modelNames, since24h)
	if err != nil {
		return dto.Fail[PageDTO](err.Error())
	}
	uptime30d, err := model.UptimeByModelSince(modelNames, since30d)
	if err != nil {
		return dto.Fail[PageDTO](err.Error())
	}
	bucketsByModel, err := model.AggregateBucketsAll(modelNames, bucketSec, since)
	if err != nil {
		return dto.Fail[PageDTO](err.Error())
	}

	page := PageDTO{
		Components: make([]ComponentDTO, 0, len(comps)),
		Bars:       map[string][]StatusBarDataDTO{},
	}

	allIncidents, _ := model.ListIncidentsBetween(since, now, "")
	incByComp := map[int][]*model.ModelStatusIncident{}
	for _, inc := range allIncidents {
		incByComp[inc.ComponentId] = append(incByComp[inc.ComponentId], inc)
	}

	for _, comp := range comps {
		c := ComponentDTO{
			Id:          comp.Id,
			Name:        comp.ModelName,
			Description: comp.Description,
			GroupId:     comp.GroupId,
			Status:      model.ModelStatusEmpty,
		}
		if ping, ok := latest[comp.ModelName]; ok {
			c.Status = ping.Status
			c.UpChannels = ping.UpChannels
			c.TotalChannels = ping.TotalChannels
			c.ProbeLatencyMs = ping.LatencyMs
			c.SampledAt = ping.Timestamp
		}
		c.Uptime24h = uptime24h[comp.ModelName]
		c.Uptime30d = uptime30d[comp.ModelName]

		for _, inc := range incByComp[comp.Id] {
			if inc.ResolvedAt == nil {
				id := inc.Id
				c.OpenIncidentId = &id
				break
			}
		}

		page.Components = append(page.Components, c)

		rows := bucketsByModel[comp.ModelName]
		bars := make([]StatusBarDataDTO, 0, len(rows))
		for _, r := range rows {
			bucketEnd := r.BucketStart + bucketSec
			item := StatusBarDataDTO{
				Day:    time.Unix(r.BucketStart, 0).UTC().Format(time.RFC3339),
				Bar:    buildBarSegments(r),
				Card:   buildCardItems(r),
				Events: []EventDTO{},
			}
			for _, inc := range incByComp[comp.Id] {
				if inc.StartedAt < bucketEnd && (inc.ResolvedAt == nil || *inc.ResolvedAt >= r.BucketStart) {
					item.Events = append(item.Events, incidentToEvent(inc))
				}
			}
			bars = append(bars, item)
		}
		page.Bars[comp.ModelName] = bars
	}

	for _, inc := range allIncidents {
		page.Incidents = append(page.Incidents, incidentToEvent(inc))
	}

	return dtoOk(page)
}

// GET /api/model_status/page_compact?bucket=1m&hours=24
// Compact wire format: bucket counters as fixed-length int tuples keyed by
// position. Client reconstructs StatusBarData[] from this. ~10x smaller than
// /page (15 MB -> ~1.5 MB) for 1m x 24h x 83 models.
func GetModelStatusPageCompact(c fuego.ContextWithParams[dto.GetModelStatusPageParams]) (*dto.Response[CompactPageDTO], error) {
	p, _ := dto.ParseParams[dto.GetModelStatusPageParams](c)

	hours := p.Hours
	if hours <= 0 {
		hours = 24
	}
	if hours > 720 {
		hours = 720
	}
	bucketSec := resolveBucketSeconds(p.Bucket)

	comps, err := model.GetAllPublicModelStatusComponents()
	if err != nil {
		return dto.Fail[CompactPageDTO](err.Error())
	}
	latest, err := model.LatestPingByModel()
	if err != nil {
		return dto.Fail[CompactPageDTO](err.Error())
	}

	now := time.Now().Unix()
	since := (now - int64(hours)*60*60) / bucketSec * bucketSec
	since24h := now - 24*60*60
	since30d := now - 30*24*60*60
	bucketCount := int((now-since)/bucketSec) + 1

	modelNames := make([]string, 0, len(comps))
	for _, comp := range comps {
		modelNames = append(modelNames, comp.ModelName)
	}

	uptime24h, err := model.UptimeByModelSince(modelNames, since24h)
	if err != nil {
		return dto.Fail[CompactPageDTO](err.Error())
	}
	uptime30d, err := model.UptimeByModelSince(modelNames, since30d)
	if err != nil {
		return dto.Fail[CompactPageDTO](err.Error())
	}
	bucketsByModel, err := model.AggregateBucketsAll(modelNames, bucketSec, since)
	if err != nil {
		return dto.Fail[CompactPageDTO](err.Error())
	}

	allIncidents, _ := model.ListIncidentsBetween(since, now, "")
	incByComp := map[int][]*model.ModelStatusIncident{}
	for _, inc := range allIncidents {
		incByComp[inc.ComponentId] = append(incByComp[inc.ComponentId], inc)
	}

	page := CompactPageDTO{
		Components:  make([]ComponentDTO, 0, len(comps)),
		BucketStart: since,
		BucketSec:   bucketSec,
		BucketCount: bucketCount,
		Bars:        map[string]*CompactBarDTO{},
	}

	for _, comp := range comps {
		cdto := ComponentDTO{
			Id:          comp.Id,
			Name:        comp.ModelName,
			Description: comp.Description,
			GroupId:     comp.GroupId,
			Status:      model.ModelStatusEmpty,
		}
		if ping, ok := latest[comp.ModelName]; ok {
			cdto.Status = ping.Status
			cdto.UpChannels = ping.UpChannels
			cdto.TotalChannels = ping.TotalChannels
			cdto.ProbeLatencyMs = ping.LatencyMs
			cdto.SampledAt = ping.Timestamp
		}
		cdto.Uptime24h = uptime24h[comp.ModelName]
		cdto.Uptime30d = uptime30d[comp.ModelName]
		for _, inc := range incByComp[comp.Id] {
			if inc.ResolvedAt == nil {
				id := inc.Id
				cdto.OpenIncidentId = &id
				break
			}
		}
		page.Components = append(page.Components, cdto)

		rows := bucketsByModel[comp.ModelName]
		rowByStart := make(map[int64]*model.BucketRow, len(rows))
		for _, r := range rows {
			rowByStart[r.BucketStart] = r
		}

		buckets := make([][7]int, bucketCount)
		for i := 0; i < bucketCount; i++ {
			ts := since + int64(i)*bucketSec
			if r, ok := rowByStart[ts]; ok {
				buckets[i] = [7]int{r.Ok, r.Degraded, r.ErrorCnt, r.Empty, r.RequestSum, r.ErrorSum, int(r.P95LatencyMs)}
			}
		}

		bar := &CompactBarDTO{Buckets: buckets}
		if incs := incByComp[comp.Id]; len(incs) > 0 {
			ev := map[string][]int{}
			for i := 0; i < bucketCount; i++ {
				ts := since + int64(i)*bucketSec
				bucketEnd := ts + bucketSec
				var ids []int
				for _, inc := range incs {
					if inc.StartedAt < bucketEnd && (inc.ResolvedAt == nil || *inc.ResolvedAt >= ts) {
						ids = append(ids, inc.Id)
					}
				}
				if len(ids) > 0 {
					ev[fmt.Sprintf("%d", i)] = ids
				}
			}
			if len(ev) > 0 {
				bar.Events = ev
			}
		}
		page.Bars[comp.ModelName] = bar
	}

	for _, inc := range allIncidents {
		page.Incidents = append(page.Incidents, incidentToEvent(inc))
	}

	return dto.Ok(page)
}

// dtoOk wraps a successful response. Local helper so handlers stay terse.
func dtoOk[T any](v T) (*dto.Response[T], error) {
	return dto.Ok(v)
}
