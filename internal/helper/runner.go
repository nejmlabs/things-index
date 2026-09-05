package helper

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

const defaultCommandTimeout = 30 * time.Second

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, args []string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type boundedRunner struct {
	runner  CommandRunner
	timeout time.Duration
}

func (r boundedRunner) Run(ctx context.Context, executable string, args []string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	stdout, stderr, err := r.runner.Run(ctx, executable, args)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return stdout, stderr, err
}

func (c *Client) commandRunner() CommandRunner {
	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	return boundedRunner{runner: runner, timeout: timeout}
}

func waitForPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
