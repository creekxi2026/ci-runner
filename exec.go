package main

import (
	"context"
	"fmt"
	"time"
)

// execCode never infers completion from a lost start response.
func execCode(ctx context.Context, n, user string, cmd []string) (int, error) {
	return execCodeEnv(ctx, n, user, cmd, nil)
}
func execCodeEnv(ctx context.Context, n, user string, cmd, env []string) (int, error) {
	var ex struct{ ID string }
	if err := dockerContext(ctx, "POST", "/containers/"+n+"/exec", obj{"User": user, "Cmd": cmd, "Env": env}, &ex); err != nil {
		return -1, err
	}
	if ex.ID == "" {
		return -1, fmt.Errorf("missing exec ID")
	}
	if err := dockerContext(ctx, "POST", "/exec/"+ex.ID+"/start", obj{"Detach": false, "Tty": false}, nil); err != nil {
		return -1, err
	}
	for {
		var v struct {
			Running  bool
			ExitCode *int
		}
		if err := dockerContext(ctx, "GET", "/exec/"+ex.ID+"/json", nil, &v); err != nil {
			return -1, err
		}
		if !v.Running && v.ExitCode != nil {
			return *v.ExitCode, nil
		}
		if !pause(ctx, 50*time.Millisecond) {
			return -1, ctx.Err()
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func appendUnique(values []string, more ...string) []string {
	for _, v := range more {
		found := false
		for _, old := range values {
			if old == v {
				found = true
			}
		}
		if !found {
			values = append(values, v)
		}
	}
	return values
}
