package service

import "testing"

func TestFormatNoticeQuota(t *testing.T) {
	got := formatNoticeQuota(859831)
	t.Log(got)
	if len(got) == 0 || got[len(got)-3] != '.' {
		t.Fatalf("want two decimals, got %q", got)
	}
}
