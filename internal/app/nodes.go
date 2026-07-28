package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/config"
	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/scheduler"
	jtemplate "github.com/wxia529/joyrun/internal/template"
)

type TargetNodesResult struct {
	Target       string                `json:"target"`
	Cluster      string                `json:"cluster"`
	Partition    string                `json:"partition"`
	ObservedAt   time.Time             `json:"observed_at"`
	Params       map[string]any        `json:"params"`
	ParamSources map[string]string     `json:"param_sources"`
	Summary      scheduler.NodeSummary `json:"summary"`
	Nodes        []scheduler.NodeInfo  `json:"nodes"`
}

func (a *App) TargetNodes(
	ctx context.Context,
	targetName string,
	sets []string,
) (TargetNodesResult, error) {
	target, ok := a.Config.Targets[targetName]
	if !ok {
		return TargetNodesResult{}, fault.New(
			"TARGET_NOT_FOUND", fmt.Sprintf("target %q not found", targetName), false,
		)
	}
	if target.Status.Partition == "" {
		return TargetNodesResult{}, fault.New(
			"TARGET_STATUS_NOT_CONFIGURED",
			fmt.Sprintf("target %q does not declare status.partition", targetName), false,
		).WithAction("add status.partition to the target configuration")
	}
	params, sources, err := config.ResolveParams(target, sets)
	if err != nil {
		return TargetNodesResult{}, err
	}
	partition, err := jtemplate.RenderString(
		target.Status.Partition,
		jtemplate.Data{Params: params},
	)
	if err != nil {
		return TargetNodesResult{}, fault.Wrap(
			"TARGET_STATUS_INVALID", "cannot render target status.partition", false, err,
		)
	}
	partition = strings.TrimSpace(partition)
	if partition == "" || strings.ContainsAny(partition, "\r\n\x00") {
		return TargetNodesResult{}, fault.New(
			"TARGET_STATUS_INVALID", "resolved status.partition is empty or invalid", false,
		)
	}
	cluster := a.Config.Clusters[target.Cluster]
	nodes, err := (scheduler.Slurm{Runner: a.Runner}).Nodes(
		ctx, cluster.Host, partition,
	)
	if err != nil {
		return TargetNodesResult{}, err
	}
	return TargetNodesResult{
		Target: targetName, Cluster: target.Cluster, Partition: partition,
		ObservedAt: time.Now().UTC(), Params: params, ParamSources: sources,
		Summary: nodes.Summary, Nodes: nodes.Nodes,
	}, nil
}
