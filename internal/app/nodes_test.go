package app

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
)

type targetNodesRunner struct {
	command string
}

func (r *targetNodesRunner) Exec(
	_ context.Context,
	_, command string,
	_ io.Reader,
) (string, string, error) {
	r.command = command
	return "community|node01|idle|32|256000|(null)\n", "", nil
}

func TestTargetNodesResolvesAllowedTargetPartition(t *testing.T) {
	runner := &targetNodesRunner{}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{
				"mindu": {Host: "mindu", Scheduler: "slurm"},
			},
			Targets: map[string]model.Target{
				"mindu/orca": {
					Cluster: "mindu",
					Params: map[string]model.ParamSpec{
						"partition": {
							Type: "string", Default: "small",
							Choices: []any{"small", "community"},
						},
					},
					Status: model.TargetStatus{Partition: "{{ .Params.partition }}"},
				},
			},
		},
		Runner: runner,
	}
	result, err := application.TargetNodes(
		context.Background(), "mindu/orca", []string{"partition=community"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Partition != "community" || result.Summary.Idle != 1 ||
		result.ParamSources["partition"] != "cli" {
		t.Fatalf("unexpected target nodes result: %#v", result)
	}
	if !strings.Contains(runner.command, "-p 'community'") {
		t.Fatalf("resolved partition was not queried: %q", runner.command)
	}
	if _, err := application.TargetNodes(
		context.Background(), "mindu/orca", []string{"partition=gpu"},
	); fault.As(err).Code != "INVALID_PARAMETER" {
		t.Fatalf("expected target choices to reject partition, got %v", err)
	}
}

func TestTargetNodesRequiresExplicitPartitionStatus(t *testing.T) {
	application := &App{Config: model.Config{
		Targets: map[string]model.Target{"c/run": {Cluster: "c"}},
	}}
	_, err := application.TargetNodes(context.Background(), "c/run", nil)
	if fault.As(err).Code != "TARGET_STATUS_NOT_CONFIGURED" {
		t.Fatalf("expected missing status configuration error, got %v", err)
	}
}
