package model

import "time"

type Project struct {
	Version   int    `yaml:"version" json:"version"`
	ProjectID string `yaml:"project_id" json:"project_id"`
	Root      string `yaml:"-" json:"root"`
}

type Cluster struct {
	Host       string `yaml:"host" json:"host"`
	Scheduler  string `yaml:"scheduler" json:"scheduler"`
	RemoteRoot string `yaml:"remote_root" json:"remote_root"`
	Transfer   string `yaml:"transfer,omitempty" json:"transfer,omitempty"`
}

type ParamSpec struct {
	Type        string `yaml:"type" json:"type"`
	Default     any    `yaml:"default,omitempty" json:"default,omitempty"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Choices     []any  `yaml:"choices,omitempty" json:"choices,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

type FilePolicy struct {
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
	Default []string `yaml:"default,omitempty" json:"default,omitempty"`
}

type SourcePolicy struct {
	Kind     string   `yaml:"kind,omitempty" json:"kind"`
	Patterns []string `yaml:"patterns,omitempty" json:"patterns,omitempty"`
}

type Target struct {
	Cluster string               `yaml:"cluster" json:"cluster"`
	Source  SourcePolicy         `yaml:"source,omitempty" json:"source"`
	Params  map[string]ParamSpec `yaml:"params,omitempty" json:"params,omitempty"`
	Script  string               `yaml:"script" json:"script"`
	Push    FilePolicy           `yaml:"push,omitempty" json:"push,omitempty"`
	Pull    FilePolicy           `yaml:"pull,omitempty" json:"pull,omitempty"`
	Logs    []string             `yaml:"logs,omitempty" json:"logs,omitempty"`
}

type Config struct {
	Version  int                `yaml:"version" json:"version"`
	Clusters map[string]Cluster `yaml:"clusters" json:"clusters"`
	Targets  map[string]Target  `yaml:"targets" json:"targets"`
}

type Source struct {
	ProjectID    string  `json:"project_id"`
	RelativePath string  `json:"relative_path"`
	WorkDir      string  `json:"workdir"`
	Entry        *string `json:"entry,omitempty"`
	Kind         string  `json:"kind"`
}

type ManifestEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Task struct {
	ID             string            `json:"id"`
	ProjectID      string            `json:"project_id"`
	SourcePath     string            `json:"source_path"`
	SourceWorkDir  string            `json:"source_workdir"`
	SourceEntry    *string           `json:"source_entry,omitempty"`
	TargetName     string            `json:"target"`
	ClusterName    string            `json:"cluster"`
	RemoteDir      string            `json:"remote_dir"`
	SchedulerID    string            `json:"scheduler_id,omitempty"`
	ComputeState   string            `json:"compute_state"`
	PullState      string            `json:"pull_state"`
	SchedulerState string            `json:"scheduler_state,omitempty"`
	ResolvedParams map[string]any    `json:"params"`
	RenderedScript string            `json:"rendered_script"`
	TargetHash     string            `json:"target_hash"`
	InputManifest  []ManifestEntry   `json:"input_manifest"`
	PullPatterns   []string          `json:"pull_patterns,omitempty"`
	PushExcludes   []string          `json:"push_excludes,omitempty"`
	Logs           []string          `json:"logs,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	SubmittedAt    *time.Time        `json:"submitted_at,omitempty"`
	UpdatedAt      time.Time         `json:"updated_at"`
	PulledAt       *time.Time        `json:"pulled_at,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type TaskEvent struct {
	ID        int64             `json:"id"`
	TaskID    string            `json:"task_id"`
	Type      string            `json:"type"`
	Stage     string            `json:"stage"`
	Message   string            `json:"message,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

type TaskSummary struct {
	ID             string    `json:"id"`
	SourcePath     string    `json:"source_path"`
	TargetName     string    `json:"target"`
	ClusterName    string    `json:"cluster"`
	SchedulerID    string    `json:"scheduler_id,omitempty"`
	ComputeState   string    `json:"compute_state"`
	PullState      string    `json:"pull_state"`
	SchedulerState string    `json:"scheduler_state,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func SummarizeTask(task Task) TaskSummary {
	return TaskSummary{
		ID: task.ID, SourcePath: task.SourcePath, TargetName: task.TargetName,
		ClusterName: task.ClusterName, SchedulerID: task.SchedulerID,
		ComputeState: task.ComputeState, PullState: task.PullState,
		SchedulerState: task.SchedulerState, CreatedAt: task.CreatedAt,
		UpdatedAt: task.UpdatedAt,
	}
}

const (
	ComputeCreated          = "created"
	ComputeSubmissionFailed = "submission_failed"
	ComputeQueued           = "queued"
	ComputeRunning          = "running"
	ComputeCompleted        = "completed"
	ComputeFailed           = "failed"
	ComputeCancelled        = "cancelled"
	ComputeUnknown          = "unknown"
)

const (
	PullNotPulled  = "not_pulled"
	PullInProgress = "pulling"
	PullSucceeded  = "pulled"
	PullPartial    = "partial"
	PullFailed     = "failed"
)
