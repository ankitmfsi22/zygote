package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/mindfire/zygote/harness"
	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

const (
	verdictMatch    = "match"
	verdictDiverged = "diverged"
	verdictInvalid  = "invalid"
	verdictError    = "error"

	agentExitGrace = 5 * time.Second
)

type replayStage int

const (
	stageNone replayStage = iota
	stageImport
	stageInit
	stagePipe
	stageStart
	stageHarness
	stageAgent
	stageTimeout
)

type replayOutcome struct {
	Verdict    string
	Stage      replayStage
	Err        error
	Divergence *journal.DivergenceError
	StepName   string
}

func recordedStepName(steps []trace.Step, n int) string {
	if n <= 0 {
		return ""
	}
	for _, s := range steps {
		if s.N == n {
			return s.Name
		}
	}
	return ""
}

func (o replayOutcome) errMessage() string {
	switch o.Stage {
	case stageStart:
		return "starting agent: " + o.Err.Error()
	case stageHarness:
		return "harness protocol error: " + o.Err.Error()
	case stageAgent:
		return "agent exited with error: " + o.Err.Error()
	}
	return o.Err.Error()
}

func failedReplay(verdict string, stage replayStage, err error) replayOutcome {
	return replayOutcome{Verdict: verdict, Stage: stage, Err: err}
}

func agentEndedNormally(waitErr error, killed bool) bool {
	if waitErr == nil {
		return true
	}
	if !killed {
		return false
	}
	var exitErr *exec.ExitError
	return errors.Is(waitErr, context.Canceled) || errors.As(waitErr, &exitErr)
}

func replayBundle(ctx context.Context, path, agentCmd string, timeout time.Duration) replayOutcome {
	store := vfs.NewMemStore()

	bundle, err := loadBundle(path, store)
	if err != nil {
		return failedReplay(verdictInvalid, stageImport, err)
	}

	r, err := trace.Replay(store, &bundle)
	if err != nil {
		return failedReplay(verdictInvalid, stageInit, err)
	}

	base := ctx
	if timeout > 0 {
		var stop context.CancelFunc
		base, stop = context.WithTimeout(ctx, timeout)
		defer stop()
	}
	runCtx, cancel := context.WithCancel(base)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", agentCmd) //nolint:gosec
	cmd.Env = agentEnv()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return failedReplay(verdictError, stagePipe, fmt.Errorf("creating stdout pipe: %w", err))
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return failedReplay(verdictError, stagePipe, fmt.Errorf("creating stdin pipe: %w", err))
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return failedReplay(verdictError, stageStart, err)
	}

	srv := harness.NewServer(stdoutPipe, stdinPipe, harness.NewReplayHandler(r))
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve() }()

	var srvErr error
	select {
	case srvErr = <-serveDone:
	case <-runCtx.Done():
		_ = stdoutPipe.Close()
	}

	var waitErr error
	killed := false
	if srvErr == nil && runCtx.Err() == nil {
		_ = stdinPipe.Close()
		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()
		select {
		case waitErr = <-waitCh:
		case <-time.After(agentExitGrace):
			killed = true
			cancel()
			waitErr = <-waitCh
		}
	} else {
		killed = true
		cancel()
		waitErr = cmd.Wait()
	}

	if err := base.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("timed out")
		}
		return failedReplay(verdictError, stageTimeout, err)
	}

	ended := agentEndedNormally(waitErr, killed)
	if srvErr == nil && ended {
		if srvErr = r.Finish(); srvErr == nil {
			srvErr = r.CheckComplete()
		}
	}

	if srvErr != nil {
		var divErr *journal.DivergenceError
		if errors.As(srvErr, &divErr) {
			return replayOutcome{Verdict: verdictDiverged, Divergence: divErr, StepName: recordedStepName(bundle.Recording.Steps, divErr.Step)}
		}
		return failedReplay(verdictError, stageHarness, srvErr)
	}
	if !ended {
		return failedReplay(verdictError, stageAgent, waitErr)
	}
	return replayOutcome{Verdict: verdictMatch}
}
