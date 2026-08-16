package retention

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseDays parses a non-negative whole number of days. Zero means retain
// indefinitely. An empty value uses defaultDays.
func ParseDays(name, value string, defaultDays int64) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = strconv.FormatInt(defaultDays, 10)
	}
	days, err := strconv.ParseInt(value, 10, 64)
	if err != nil || days < 0 {
		return 0, fmt.Errorf("%s must be a non-negative whole number of days", name)
	}
	const day = 24 * time.Hour
	if days > math.MaxInt64/int64(day) {
		return 0, fmt.Errorf("%s is too large", name)
	}
	return time.Duration(days) * day, nil
}

// Cutoff returns the oldest time to retain. A zero duration disables pruning.
func Cutoff(now time.Time, keep time.Duration) time.Time {
	if keep == 0 {
		return time.Time{}
	}
	return now.UTC().Add(-keep)
}
