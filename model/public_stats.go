package model

import (
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// PublicGroupStat is the anonymous, aggregate health of one group over the
// last 24 hours, shown on the public status page.
type PublicGroupStat struct {
	Group      string  `json:"group"`
	Requests   int64   `json:"requests"`
	Errors     int64   `json:"errors"`
	AvgUseTime float64 `json:"avg_use_time"`
	// AvgFrt is the mean first-response time in milliseconds of successful
	// requests that recorded one (0 when none did).
	AvgFrt float64 `json:"avg_frt"`
}

// PublicModelStat is the same aggregate for one model within one group, so
// the model list can show which route serves a model well right now.
type PublicModelStat struct {
	Group      string  `json:"group"`
	Model      string  `json:"model"`
	Requests   int64   `json:"requests"`
	Errors     int64   `json:"errors"`
	AvgUseTime float64 `json:"avg_use_time"`
	AvgFrt     float64 `json:"avg_frt"`
}

// PublicStats holds only aggregate numbers; it never exposes users, tokens,
// models or request content.
type PublicStats struct {
	Groups        []PublicGroupStat `json:"groups"`
	Models        []PublicModelStat `json:"models"`
	Requests30d   int64             `json:"requests_30d"`
	ActiveUsers30 int64             `json:"active_users_30d"`
	UpdatedAt     int64             `json:"updated_at"`
}

const publicStatsTTL = 5 * time.Minute

var (
	publicStatsMu    sync.Mutex
	publicStatsCache *PublicStats
)

// GetPublicStats returns cached aggregate stats, recomputing at most every
// publicStatsTTL so the public endpoint cannot be used to load the log DB.
func GetPublicStats() (*PublicStats, error) {
	publicStatsMu.Lock()
	defer publicStatsMu.Unlock()
	now := time.Now().Unix()
	if publicStatsCache != nil && now-publicStatsCache.UpdatedAt < int64(publicStatsTTL.Seconds()) {
		return publicStatsCache, nil
	}

	since24h := now - 24*3600
	groups, models, err := publicStats24h(since24h)
	if err != nil {
		return nil, err
	}
	stats := &PublicStats{UpdatedAt: now, Groups: groups, Models: models}

	since30d := now - 30*24*3600
	if err := LOG_DB.Model(&Log{}).Where("created_at >= ? AND type = ?", since30d, LogTypeConsume).Count(&stats.Requests30d).Error; err != nil {
		return nil, err
	}
	if err := LOG_DB.Model(&Log{}).Where("created_at >= ? AND type = ?", since30d, LogTypeConsume).Distinct("user_id").Count(&stats.ActiveUsers30).Error; err != nil {
		return nil, err
	}
	publicStatsCache = stats
	common.SysLog(common.LogText("public stats refreshed"))
	return stats, nil
}

// publicFrtScanLimit caps how many recent successful logs are read to average
// first-response time. frt lives in the JSON "other" column, which cannot be
// aggregated portably in SQL, so it is averaged in Go over at most this many rows.
const publicFrtScanLimit = 50000

type frtSum struct {
	sum   float64
	count int64
}

func (f frtSum) avg() float64 {
	if f.count == 0 {
		return 0
	}
	return f.sum / float64(f.count)
}

// publicStats24h aggregates consume/error logs since the given time by group
// and by group+model. Only counts and averages leave this function.
func publicStats24h(since int64) ([]PublicGroupStat, []PublicModelStat, error) {
	var rows []struct {
		Group     string
		ModelName string
		Type      int
		Count     int64
		UseTime   float64
	}
	err := LOG_DB.Model(&Log{}).
		Select(logGroupCol+" AS "+logGroupCol+", model_name, type, COUNT(*) AS count, AVG(use_time) AS use_time").
		Where("created_at >= ? AND type IN ?", since, []int{LogTypeConsume, LogTypeError}).
		Group(logGroupCol + ", model_name, type").
		Scan(&rows).Error
	if err != nil {
		return nil, nil, err
	}

	var frtRows []struct {
		Group     string
		ModelName string
		Other     string
	}
	err = LOG_DB.Model(&Log{}).
		Select(logGroupCol+" AS "+logGroupCol+", model_name, other").
		Where("created_at >= ? AND type = ?", since, LogTypeConsume).
		Order("id DESC").Limit(publicFrtScanLimit).
		Scan(&frtRows).Error
	if err != nil {
		return nil, nil, err
	}
	groupFrt := map[string]frtSum{}
	modelFrt := map[[2]string]frtSum{}
	for _, r := range frtRows {
		frt, ok := logFrt(r.Other)
		if !ok || r.Group == "" {
			continue
		}
		g := groupFrt[r.Group]
		g.sum += frt
		g.count++
		groupFrt[r.Group] = g
		k := [2]string{r.Group, r.ModelName}
		m := modelFrt[k]
		m.sum += frt
		m.count++
		modelFrt[k] = m
	}

	type acc struct {
		requests, errors, okCount int64
		useTimeSum                float64
	}
	byGroup := map[string]*acc{}
	byModel := map[[2]string]*acc{}
	var groupOrder []string
	var modelOrder [][2]string
	add := func(a *acc, r struct {
		Group     string
		ModelName string
		Type      int
		Count     int64
		UseTime   float64
	}) {
		a.requests += r.Count
		if r.Type == LogTypeError {
			a.errors += r.Count
			return
		}
		a.okCount += r.Count
		a.useTimeSum += r.UseTime * float64(r.Count)
	}
	for _, r := range rows {
		if r.Group == "" {
			continue
		}
		g, ok := byGroup[r.Group]
		if !ok {
			g = &acc{}
			byGroup[r.Group] = g
			groupOrder = append(groupOrder, r.Group)
		}
		add(g, r)
		if r.ModelName == "" {
			continue
		}
		k := [2]string{r.Group, r.ModelName}
		m, ok := byModel[k]
		if !ok {
			m = &acc{}
			byModel[k] = m
			modelOrder = append(modelOrder, k)
		}
		add(m, r)
	}
	avgUse := func(a *acc) float64 {
		if a.okCount == 0 {
			return 0
		}
		return a.useTimeSum / float64(a.okCount)
	}
	groups := make([]PublicGroupStat, 0, len(groupOrder))
	for _, name := range groupOrder {
		a := byGroup[name]
		groups = append(groups, PublicGroupStat{Group: name, Requests: a.requests, Errors: a.errors, AvgUseTime: avgUse(a), AvgFrt: groupFrt[name].avg()})
	}
	models := make([]PublicModelStat, 0, len(modelOrder))
	for _, k := range modelOrder {
		a := byModel[k]
		models = append(models, PublicModelStat{Group: k[0], Model: k[1], Requests: a.requests, Errors: a.errors, AvgUseTime: avgUse(a), AvgFrt: modelFrt[k].avg()})
	}
	return groups, models, nil
}

// logFrt reads the first-response time (ms) a consume log recorded in its
// "other" JSON; ok is false when absent or not positive.
func logFrt(other string) (float64, bool) {
	if other == "" || !strings.Contains(other, `"frt"`) {
		return 0, false
	}
	var v struct {
		Frt float64 `json:"frt"`
	}
	if err := common.UnmarshalJsonStr(other, &v); err != nil || v.Frt <= 0 {
		return 0, false
	}
	return v.Frt, true
}

// UserPerf is one user's own latency summary over a time range.
type UserPerf struct {
	Requests   int64   `json:"requests"`
	AvgUseTime float64 `json:"avg_use_time"`
	AvgFrt     float64 `json:"avg_frt"`
	// FrtSamples is how many requests the first-response average is based on
	// (capped by userFrtScanLimit; streams record it, some others do not).
	FrtSamples int64 `json:"frt_samples"`
}

const userFrtScanLimit = 20000

// GetUserPerf averages a user's successful requests in [start, end].
func GetUserPerf(userId int, start, end int64) (*UserPerf, error) {
	tx := LOG_DB.Model(&Log{}).Where("user_id = ? AND type = ?", userId, LogTypeConsume)
	if start > 0 {
		tx = tx.Where("created_at >= ?", start)
	}
	if end > 0 {
		tx = tx.Where("created_at <= ?", end)
	}
	var agg struct {
		Count   int64
		UseTime float64
	}
	if err := tx.Session(&gorm.Session{}).Select("COUNT(*) AS count, COALESCE(AVG(use_time), 0) AS use_time").Scan(&agg).Error; err != nil {
		return nil, err
	}
	perf := &UserPerf{Requests: agg.Count, AvgUseTime: agg.UseTime}
	if agg.Count == 0 {
		return perf, nil
	}
	var others []string
	if err := tx.Session(&gorm.Session{}).Order("id DESC").Limit(userFrtScanLimit).Pluck("other", &others).Error; err != nil {
		return nil, err
	}
	var f frtSum
	for _, o := range others {
		if v, ok := logFrt(o); ok {
			f.sum += v
			f.count++
		}
	}
	perf.AvgFrt = f.avg()
	perf.FrtSamples = f.count
	return perf, nil
}
