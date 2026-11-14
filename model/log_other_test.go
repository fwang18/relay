package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogOtherScopesAndMerges(t *testing.T) {
	var other LogOther

	assert.True(t, other.SetPublic("request_path", "/v1/chat/completions"))
	other.MergePublic(map[string]any{
		"zero": 0,
	})
	assert.True(t, other.SetAdmin("use_channel", []string{"channel-a"}))
	other.MergeAdmin(map[string]any{
		"rejected": false,
	})
	assert.True(t, other.SetRoot("upstream_request_id", "upstream-private"))
	other.MergeRoot(map[string]any{
		"generation": 0,
	})
	assert.True(t, other.SetAudit("method", "POST"))
	other.MergeAudit(map[string]any{
		"success": false,
	})

	require.JSONEq(t, `{
		"request_path": "/v1/chat/completions",
		"zero": 0,
		"admin_info": {
			"use_channel": ["channel-a"],
			"rejected": false
		},
		"root_info": {
			"upstream_request_id": "upstream-private",
			"generation": 0
		},
		"audit_info": {
			"method": "POST",
			"success": false
		}
	}`, other.JSONString())
}

func TestLogOtherRejectsSensitivePublicFields(t *testing.T) {
	other := NewLogOther()

	for _, key := range []string{
		"admin_info",
		"root_info",
		"audit_info",
		"channel_id",
		"channel_name",
		"channel_type",
		"reject_reason",
	} {
		assert.False(t, other.SetPublic(key, "must-not-leak"), key)
	}
	other.MergePublic(map[string]any{
		"request_path": "/v1/responses",
		"channel_name": "still-must-not-leak",
		"admin_info":   map[string]any{"secret": true},
	})

	require.JSONEq(t, `{"request_path":"/v1/responses"}`, other.JSONString())
	require.JSONEq(t, `{}`, NewLogOther().JSONString())
}

func TestLogOtherJSONStringDoesNotMutateReceiver(t *testing.T) {
	other := NewLogOther()
	require.True(t, other.SetPublic("request_path", "/v1/chat/completions"))
	require.True(t, other.SetAdmin("rejected", false))

	before := other.Snapshot()
	first := other.JSONString()
	after := other.Snapshot()
	second := other.JSONString()

	require.Equal(t, before, after)
	require.Equal(t, first, second)
}

func TestGetPublicStatsAggregatesByGroupWithoutUserData(t *testing.T) {
	truncateTables(t)
	publicStatsCache = nil
	t.Cleanup(func() { publicStatsCache = nil })

	now := time.Now().Unix()
	logs := []*Log{
		{UserId: 1, Type: LogTypeConsume, Group: "claude", UseTime: 2, CreatedAt: now - 60},
		{UserId: 2, Type: LogTypeConsume, Group: "claude", UseTime: 4, CreatedAt: now - 60},
		{UserId: 2, Type: LogTypeError, Group: "claude", CreatedAt: now - 60},
		{UserId: 3, Type: LogTypeConsume, Group: "codex", UseTime: 1, CreatedAt: now - 2*24*3600},
		{UserId: 4, Type: LogTypeTopup, Group: "claude", CreatedAt: now - 60},
	}
	require.NoError(t, LOG_DB.Create(logs).Error)

	stats, err := GetPublicStats()
	require.NoError(t, err)
	require.Len(t, stats.Groups, 1)
	assert.Equal(t, PublicGroupStat{Group: "claude", Requests: 3, Errors: 1, AvgUseTime: 3}, stats.Groups[0])
	require.Len(t, stats.Models, 0) // logs above carry no model name
	assert.Equal(t, int64(3), stats.Requests30d)
	assert.Equal(t, int64(3), stats.ActiveUsers30)
}

func TestGetPublicStatsAggregatesByModelWithFirstResponseTime(t *testing.T) {
	truncateTables(t)
	publicStatsCache = nil
	t.Cleanup(func() { publicStatsCache = nil })

	now := time.Now().Unix()
	logs := []*Log{
		{UserId: 1, Type: LogTypeConsume, Group: "cc", ModelName: "claude-a", UseTime: 2, Other: `{"frt":1000}`, CreatedAt: now - 60},
		{UserId: 2, Type: LogTypeConsume, Group: "cc", ModelName: "claude-a", UseTime: 6, Other: `{"frt":3000,"x":1}`, CreatedAt: now - 60},
		{UserId: 2, Type: LogTypeError, Group: "cc", ModelName: "claude-a", CreatedAt: now - 60},
		{UserId: 3, Type: LogTypeConsume, Group: "cc", ModelName: "claude-b", UseTime: 1, CreatedAt: now - 60},
		{UserId: 3, Type: LogTypeConsume, Group: "codex", ModelName: "claude-a", UseTime: 5, Other: `{"frt":500}`, CreatedAt: now - 60},
	}
	require.NoError(t, LOG_DB.Create(logs).Error)

	stats, err := GetPublicStats()
	require.NoError(t, err)
	byKey := map[string]PublicModelStat{}
	for _, m := range stats.Models {
		byKey[m.Group+"/"+m.Model] = m
	}
	require.Len(t, byKey, 3)
	assert.Equal(t, PublicModelStat{Group: "cc", Model: "claude-a", Requests: 3, Errors: 1, AvgUseTime: 4, AvgFrt: 2000}, byKey["cc/claude-a"])
	assert.Equal(t, PublicModelStat{Group: "cc", Model: "claude-b", Requests: 1, AvgUseTime: 1}, byKey["cc/claude-b"])
	assert.Equal(t, 500.0, byKey["codex/claude-a"].AvgFrt)
	for _, g := range stats.Groups {
		if g.Group == "cc" {
			assert.Equal(t, PublicGroupStat{Group: "cc", Requests: 4, Errors: 1, AvgUseTime: 3, AvgFrt: 2000}, g)
		}
	}
}

func TestGetUserPerfAveragesOnlyOwnSuccessfulRequests(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	logs := []*Log{
		{UserId: 7, Type: LogTypeConsume, UseTime: 2, Other: `{"frt":800}`, CreatedAt: now - 60},
		{UserId: 7, Type: LogTypeConsume, UseTime: 4, CreatedAt: now - 60},
		{UserId: 7, Type: LogTypeError, UseTime: 30, CreatedAt: now - 60},
		{UserId: 7, Type: LogTypeConsume, UseTime: 100, CreatedAt: now - 10*24*3600},
		{UserId: 8, Type: LogTypeConsume, UseTime: 50, Other: `{"frt":9000}`, CreatedAt: now - 60},
	}
	require.NoError(t, LOG_DB.Create(logs).Error)

	perf, err := GetUserPerf(7, now-24*3600, now)
	require.NoError(t, err)
	assert.Equal(t, UserPerf{Requests: 2, AvgUseTime: 3, AvgFrt: 800, FrtSamples: 1}, *perf)

	empty, err := GetUserPerf(9, now-24*3600, now)
	require.NoError(t, err)
	assert.Equal(t, UserPerf{}, *empty)
}
