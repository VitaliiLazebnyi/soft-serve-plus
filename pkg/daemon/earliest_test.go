package daemon

import (
	"testing"
	"time"
)

func TestEarliest(t *testing.T) {
	t.Parallel()
	base := time.Now()
	before, after := base.Add(-time.Second), base.Add(time.Second)
	for _, tc := range []struct {
		ts   []time.Time
		want time.Time
	}{
		{nil, base},
		{[]time.Time{{}, {}}, base},
		{[]time.Time{after}, base},
		{[]time.Time{{}, before}, before},
		{[]time.Time{after, before, {}}, before},
	} {
		if got := earliest(base, tc.ts...); !got.Equal(tc.want) {
			t.Errorf("earliest(base, %v) = %v, want %v", tc.ts, got, tc.want)
		}
	}
}
