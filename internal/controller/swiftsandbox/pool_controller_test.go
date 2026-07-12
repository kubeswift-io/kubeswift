package swiftsandbox

import "testing"

func TestSlotsToCreate(t *testing.T) {
	cases := []struct {
		name                       string
		minWarm, maxWarm, warmLive int
		want                       int
	}{
		{"cold empty pool", 3, 6, 0, 3},
		{"partially warm", 3, 6, 1, 2},
		{"at target", 3, 6, 3, 0},
		{"over target (no negative)", 3, 6, 5, 0},
		{"maxWarm below minWarm — minWarm wins", 4, 2, 0, 4},
		{"warmLive above minWarm — no create", 3, 4, 4, 0},
		{"unset maxWarm caps at minWarm", 2, 0, 0, 2},
		{"minWarm zero warms nothing", 0, 6, 0, 0},
		{"explicit large minWarm is honored (operator owns it)", 100, 0, 0, 100},
	}
	for _, c := range cases {
		if got := slotsToCreate(c.minWarm, c.maxWarm, c.warmLive); got != c.want {
			t.Errorf("%s: slotsToCreate(min=%d,max=%d,live=%d)=%d, want %d",
				c.name, c.minWarm, c.maxWarm, c.warmLive, got, c.want)
		}
	}
}
