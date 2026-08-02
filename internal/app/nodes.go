package app

import (
	"context"
	"fmt"
	"time"

	"github.com/wxia529/joyrun/internal/config"
	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/scheduler"
)

type TargetNodesResult struct {
	Target          string                  `json:"target"`
	Cluster         string                  `json:"cluster"`
	Software        model.Software          `json:"software"`
	Partition       model.ResolvedPartition `json:"partition"`
	PartitionSource string                  `json:"partition_source"`
	ObservedAt      time.Time               `json:"observed_at"`
	Summary         scheduler.NodeSummary   `json:"summary"`
	Nodes           []scheduler.NodeInfo    `json:"nodes"`
}

func (a *App) TargetNodes(
	ctx context.Context,
	targetName string,
	partitionOverride string,
) (TargetNodesResult, error) {
	target, ok := a.Config.Targets[targetName]
	if !ok {
		return TargetNodesResult{}, fault.New(
			"TARGET_NOT_FOUND", fmt.Sprintf("target %q not found", targetName), false,
		)
	}
	if target.Placement.DefaultPartition == "" {
		return TargetNodesResult{}, fault.New(
			"TARGET_PLACEMENT_NOT_CONFIGURED",
			fmt.Sprintf("target %q does not declare placement", targetName), false,
		).WithAction("add placement.default_partition and placement.allowed_partitions")
	}
	cluster := a.Config.Clusters[target.Cluster]
	partition, partitionSource, err := config.ResolvePartition(cluster, target, partitionOverride)
	if err != nil {
		return TargetNodesResult{}, err
	}
	nodes, err := a.scheduler().Nodes(
		ctx, cluster.Host, partition.Name,
	)
	if err != nil {
		return TargetNodesResult{}, err
	}
	return TargetNodesResult{
		Target: targetName, Cluster: target.Cluster, Software: target.Software,
		Partition: partition, PartitionSource: partitionSource,
		ObservedAt: time.Now().UTC(), Summary: nodes.Summary, Nodes: nodes.Nodes,
	}, nil
}
