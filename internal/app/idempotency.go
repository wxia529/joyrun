package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/wxia529/joyrun/internal/model"
)

// submissionFingerprint contains only the immutable intent of a submission.
// It deliberately excludes TaskID, RemoteDir, and timestamps so a client
// retrying after a lost SSH response addresses the same submission.
type submissionFingerprint struct {
	ProjectID      string
	SourcePath     string
	SourceWorkDir  string
	SourceEntry    *string
	TargetName     string
	ClusterName    string
	RenderedScript string
	Partition      string
	ResolvedParams map[string]any
	InputManifest  []model.ManifestEntry
}

func submissionKey(task model.Task) (string, error) {
	manifest := append([]model.ManifestEntry(nil), task.InputManifest...)
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
	payload, err := json.Marshal(submissionFingerprint{
		ProjectID: task.ProjectID, SourcePath: task.SourcePath,
		SourceWorkDir: task.SourceWorkDir, SourceEntry: task.SourceEntry,
		TargetName: task.TargetName, ClusterName: task.ClusterName,
		RenderedScript: task.RenderedScript, Partition: task.Metadata["partition"],
		ResolvedParams: task.ResolvedParams, InputManifest: manifest,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "v1:" + hex.EncodeToString(sum[:]), nil
}

// SubmissionKey exposes the immutable fingerprint used by both direct and
// daemon admission.  Keeping the calculation in one place prevents a fast
// local reservation from diverging from the idempotent remote executor.
func SubmissionKey(task model.Task) (string, error) { return submissionKey(task) }
