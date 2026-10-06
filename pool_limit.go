package main

import (
	"fmt"
	"strconv"
)

func poolJobLimit(value string) (int, error) {
	if value == "" {
		return defaultPoolJobLimit, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 3 {
		return 0, fmt.Errorf("RUNNER_MAX_JOBS must be 1..3")
	}
	return n, nil
}
func (f *fleet) jobLimit() int {
	if f.maxJobs == 0 {
		return defaultPoolJobLimit
	}
	return f.maxJobs
}
