package model

import (
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// PublicGroupStat is the anonymous, aggregate health of one group over the
// last 24 hours, shown on the public status page.
type PublicGroupStat struct {
	Group      string  `json:"group"`
	Requests   int64   `json:"requests"`
	Errors     int64   `json:"errors"`
	AvgUseTime float64 `json:"avg_use_time"`
}

// PublicStats holds only aggregate numbers; it never exposes users, tokens,
// models or request content.
type PublicStats struct {
	Groups        []PublicGroupStat `json:"groups"`
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
	var rows []struct {
		Group   string
		Type    int
		Count   int64
		UseTime float64
	}
	err := LOG_DB.Model(&Log{}).
		Select(logGroupCol+" AS "+logGroupCol+", type, COUNT(*) AS count, AVG(use_time) AS use_time").
		Where("created_at >= ? AND type IN ?", since24h, []int{LogTypeConsume, LogTypeError}).
		Group(logGroupCol + ", type").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	byGroup := map[string]*PublicGroupStat{}
	var order []string
	for _, r := range rows {
		if r.Group == "" {
			continue
		}
		g, ok := byGroup[r.Group]
		if !ok {
			g = &PublicGroupStat{Group: r.Group}
			byGroup[r.Group] = g
			order = append(order, r.Group)
		}
		g.Requests += r.Count
		if r.Type == LogTypeError {
			g.Errors += r.Count
			continue
		}
		g.AvgUseTime = r.UseTime
	}

	stats := &PublicStats{UpdatedAt: now, Groups: make([]PublicGroupStat, 0, len(order))}
	for _, name := range order {
		stats.Groups = append(stats.Groups, *byGroup[name])
	}

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
