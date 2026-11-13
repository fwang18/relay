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
	assert.Equal(t, int64(3), stats.Requests30d)
	assert.Equal(t, int64(3), stats.ActiveUsers30)
}
