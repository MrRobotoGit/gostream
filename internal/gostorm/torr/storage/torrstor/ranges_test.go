package torrstor

import (
	"testing"
)

func TestInRanges(t *testing.T) {
	// Reference linear implementation for validation
	inRangesLinear := func(ranges []Range, ind int) bool {
		for _, r := range ranges {
			if ind >= r.Start && ind <= r.End {
				return true
			}
		}
		return false
	}

	tests := []struct {
		name   string
		ranges []Range
		ind    int
	}{
		{
			name:   "Inside single range",
			ranges: []Range{{Start: 10, End: 20}},
			ind:    15,
		},
		{
			name:   "On start boundary",
			ranges: []Range{{Start: 10, End: 20}},
			ind:    10,
		},
		{
			name:   "On end boundary",
			ranges: []Range{{Start: 10, End: 20}},
			ind:    20,
		},
		{
			name:   "Outside before",
			ranges: []Range{{Start: 10, End: 20}},
			ind:    5,
		},
		{
			name:   "Outside after",
			ranges: []Range{{Start: 10, End: 20}},
			ind:    25,
		},
		{
			name:   "Between two ranges",
			ranges: []Range{{Start: 10, End: 20}, {Start: 30, End: 40}},
			ind:    25,
		},
		{
			name:   "Inside second range",
			ranges: []Range{{Start: 10, End: 20}, {Start: 30, End: 40}},
			ind:    35,
		},
		{
			name:   "Large set of ranges",
			ranges: []Range{
				{Start: 0, End: 5},
				{Start: 10, End: 15},
				{Start: 20, End: 25},
				{Start: 30, End: 35},
				{Start: 100, End: 1000},
			},
			ind: 150,
		},
		{
			name:   "Empty ranges",
			ranges: []Range{},
			ind:    10,
		},
		{
			name:   "Index before all ranges",
			ranges: []Range{{Start: 100, End: 200}, {Start: 300, End: 400}},
			ind:    50,
		},
		{
			name:   "Index after all ranges",
			ranges: []Range{{Start: 100, End: 200}, {Start: 300, End: 400}},
			ind:    500,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inRanges(tt.ranges, tt.ind)
			expected := inRangesLinear(tt.ranges, tt.ind)
			if got != expected {
				t.Errorf("inRanges() = %v, expected %v (ind: %d, ranges: %+v)", got, expected, tt.ind, tt.ranges)
			}
		})
	}
}
