package scheduler

import (
	"context"
	"io"
	"strings"
	"testing"
)

type nodeQueryRunner struct {
	command string
	output  string
	stderr  string
	err     error
}

func (r *nodeQueryRunner) Exec(
	_ context.Context,
	_, command string,
	_ io.Reader,
) (string, string, error) {
	r.command = command
	return r.output, r.stderr, r.err
}

func TestNodesQueriesOneQuotedPartitionAndSummarizesStates(t *testing.T) {
	runner := &nodeQueryRunner{output: strings.Join([]string{
		"community*|node01|idle|32|256000|(null)",
		"community*|node02|mixed|32|256000|gpu:a100:1",
		"community*|node03|allocated|32|256000|(null)",
		"community*|node04|drained|32|256000|(null)",
	}, "\n")}
	result, err := (Slurm{Runner: runner}).Nodes(
		context.Background(), "mindu", "community",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runner.command, "-p 'community'") ||
		!strings.Contains(runner.command, "%P|%N|%T|%c|%m|%G") {
		t.Fatalf("unexpected sinfo command: %q", runner.command)
	}
	if result.Summary.Total != 4 || result.Summary.Idle != 1 ||
		result.Summary.Mixed != 1 || result.Summary.Allocated != 1 ||
		result.Summary.Unavailable != 1 {
		t.Fatalf("unexpected summary: %#v", result.Summary)
	}
	if result.Nodes[0].Partition != "community" ||
		result.Nodes[1].GRES != "gpu:a100:1" {
		t.Fatalf("unexpected nodes: %#v", result.Nodes)
	}
}

func TestParseNodesRejectsUnexpectedSinfoOutput(t *testing.T) {
	if _, err := parseNodes("broken row"); err == nil {
		t.Fatal("expected malformed sinfo row rejection")
	}
}
