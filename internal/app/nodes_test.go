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
				"mindu": {
					Host: "mindu", Scheduler: "slurm",
					Partitions: map[string]model.Partition{
						"small":     {CoresPerNode: 32, MemoryPerNode: "128GiB"},
						"community": {CoresPerNode: 32, MemoryPerNode: "256GiB"},
					},
				},
			},
			Targets: map[string]model.Target{
				"mindu/orca": {
					Cluster:  "mindu",
					Software: model.Software{Name: "orca", Version: "6.1.1"},
					Placement: model.Placement{
						DefaultPartition:  "small",
						AllowedPartitions: []string{"small", "community"},
					},
				},
			},
		},
		Runner: runner,
	}
	result, err := application.TargetNodes(
		context.Background(), "mindu/orca", "community",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Partition.Name != "community" || result.Summary.Idle != 1 ||
		result.PartitionSource != "cli" ||
		result.Partition.MemoryPerNode != "256GiB" {
		t.Fatalf("unexpected target nodes result: %#v", result)
	}
	if !strings.Contains(runner.command, "-p 'community'") {
		t.Fatalf("resolved partition was not queried: %q", runner.command)
	}
	if _, err := application.TargetNodes(
		context.Background(), "mindu/orca", "gpu",
	); fault.As(err).Code != "INVALID_PARTITION" {
		t.Fatalf("expected target placement to reject partition, got %v", err)
	}
}

func TestTargetNodesRequiresExplicitPlacement(t *testing.T) {
	application := &App{Config: model.Config{
		Targets: map[string]model.Target{"c/run": {Cluster: "c"}},
	}}
	_, err := application.TargetNodes(context.Background(), "c/run", "")
	if fault.As(err).Code != "TARGET_PLACEMENT_NOT_CONFIGURED" {
		t.Fatalf("expected missing placement configuration error, got %v", err)
	}
}
