package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wxia529/joyrun/internal/app"
	"github.com/wxia529/joyrun/internal/config"
	"github.com/wxia529/joyrun/internal/daemon"
	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/identity"
	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/paths"
	"github.com/wxia529/joyrun/internal/project"
	"github.com/wxia529/joyrun/internal/remote"
	"github.com/wxia529/joyrun/internal/scheduler"
	"github.com/wxia529/joyrun/internal/source"
	"github.com/wxia529/joyrun/internal/store"
	"github.com/wxia529/joyrun/internal/transfer"
)

var runningOperationCancels sync.Map
var daemonStatusMu sync.Mutex

func (c *command) setAutoPull(db *store.Store, task *model.Task, policy string) error {
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	task.Metadata["auto_pull"] = policy
	task.Metadata["auto_pull_done"] = ""
	task.UpdatedAt = time.Now().UTC()
	return db.UpdateTask(c.ctx, task)
}

func (c *command) admitDetached(kind string, args []string) error {
	return c.admitDetachedAt(kind, args, c.cwd)
}

func (c *command) admitDetachedAt(kind string, args []string, workingDir string) error {
	p, err := project.Discover(c.cwd)
	if workingDir != "" {
		p, err = project.Discover(workingDir)
	}
	if err != nil {
		return err
	}
	db, err := store.Open(paths.DatabaseFile())
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.BindProject(c.ctx, p); err != nil {
		return err
	}
	id, err := identity.New("jo_")
	if err != nil {
		return fault.Wrap("OPERATION_CREATE_FAILED", "cannot allocate operation id", false, err)
	}
	clean := append([]string(nil), args...)
	operationRoot := filepath.Join(paths.OperationDataDir(), id)
	if err := os.MkdirAll(operationRoot, 0o700); err != nil {
		return fault.Wrap("OPERATION_CREATE_FAILED", "cannot create detached operation state", false, err)
	}
	keepOperationState := false
	defer func() {
		if !keepOperationState {
			_ = os.RemoveAll(operationRoot)
		}
	}()
	configSnapshot := filepath.Join(operationRoot, "config.yaml")
	configPath, err := filepath.Abs(c.config)
	if err != nil {
		_ = os.RemoveAll(operationRoot)
		return fault.Wrap("OPERATION_CREATE_FAILED", "cannot resolve configuration path", false, err)
	}
	if err := copyFile(configPath, configSnapshot); err != nil {
		_ = os.RemoveAll(operationRoot)
		return fault.Wrap("OPERATION_CREATE_FAILED", "cannot snapshot configuration", false, err)
	}
	payload := detachedPayload{Args: clean, ProjectID: p.ProjectID, CWD: workingDir, Config: configSnapshot, DataRoot: operationRoot}
	var submitPreview submitPreviewOutput
	if kind == "submit" {
		var normalizeErr error
		clean, normalizeErr = normalizeDetachedSubmitArgs(clean, workingDir)
		if normalizeErr != nil {
			return normalizeErr
		}
		previewArgs := append([]string{kind}, clean...)
		previewArgs = append(previewArgs, "--dry-run", "--json", "--config", c.config)
		var previewOut, previewErr bytes.Buffer
		if code := run(c.ctx, previewArgs, c.version, &previewOut, &previewErr, workingDir, true); code != 0 {
			if previewErr.Len() > 0 {
				return fault.New("DETACH_PREVIEW_FAILED", strings.TrimSpace(previewErr.String()), false)
			}
			return fault.New("DETACH_PREVIEW_FAILED", "detached submit preview failed", false)
		}
		var previewEnvelope struct {
			Result submitPreviewOutput `json:"result"`
		}
		if err := json.Unmarshal(previewOut.Bytes(), &previewEnvelope); err != nil {
			return fault.Wrap("DETACH_PREVIEW_FAILED", "cannot decode detached submit preview", false, err)
		}
		submitPreview = previewEnvelope.Result
		if len(submitPreview.Previews) == 0 {
			return fault.New("DETACH_PREVIEW_FAILED", "detached submit preview selected no sources", false)
		}
		root, rewritten, err := prepareDetachedSnapshot(p, workingDir, id, submitPreview.Previews, clean)
		if err != nil {
			_ = os.RemoveAll(operationRoot)
			return err
		}
		payload.SnapshotRoot, payload.Args = root, rewritten
		payload.IntentHash = previewManifestHash(submitPreview.Previews)
		// Reserve the immutable Task at admission for the common single-source
		// case.  The worker later calls the normal Submit path; its existing
		// submission fingerprint reuses this reservation instead of creating a
		// second Task.  Batch execution keeps the older all-or-nothing submit
		// path until batch reservations are added, but is still durable and
		// asynchronous.
		if len(submitPreview.Tasks) == 1 && !containsFlag(clean, "--force-new") &&
			!hasFlagPrefix(clean, "--force-new=") {
			task := submitPreview.Tasks[0]
			key, keyErr := app.SubmissionKey(task)
			if keyErr != nil {
				return fault.Wrap("OPERATION_CREATE_FAILED", "cannot calculate task admission key", false, keyErr)
			}
			if task.Metadata == nil {
				task.Metadata = map[string]string{}
			}
			task.Metadata["submission_key"] = key
			existing, deduplicated, err := db.CreateTaskIdempotent(c.ctx, &task, false)
			if err != nil {
				return err
			}
			if deduplicated {
				task = existing
			}
			payload.ReservedTaskIDs = []string{task.ID}
			if policy := flagValue(clean, "--auto-pull"); policy != "" && policy != "off" {
				_ = c.setAutoPull(db, &task, policy)
			}
			// The detached snapshot is project-shaped so the ordinary batch
			// submit path can resolve every source. A reserved single-source
			// execution bypasses that preparation and uploads SnapshotRoot
			// directly as remote work/. Point it at the source work directory so
			// .Input and its dependencies are present at the paths used by the
			// rendered script, rather than under project-relative prefixes.
			payload.SnapshotRoot = filepath.Join(root,
				filepath.FromSlash(submitPreview.Previews[0].Source.WorkDir))
		}
	}
	payloadBytes, _ := json.Marshal(payload)
	intentBytes, _ := json.Marshal(struct {
		Kind      string   `json:"kind"`
		ProjectID string   `json:"project_id"`
		Args      []string `json:"args"`
		Hash      string   `json:"input_hash,omitempty"`
	}{Kind: kind, ProjectID: p.ProjectID, Args: payload.Args, Hash: detachedInputHash(payload)})
	dedupSum := sha256.Sum256(intentBytes)
	dedupKey := "v1:" + hex.EncodeToString(dedupSum[:])
	// --force-new is an explicit request for a distinct execution.  It must
	// not be collapsed with another queued force-new operation even when the
	// source and rendered script are identical.
	if containsFlag(clean, "--force-new") || hasFlagPrefix(clean, "--force-new=") {
		dedupKey = "force:" + id
	}
	if existing, found, findErr := db.FindOperationByDedup(c.ctx, kind, dedupKey); findErr != nil {
		return findErr
	} else if found {
		keepOperationState = false
		result := map[string]any{"operation_id": existing.ID, "state": existing.State, "kind": existing.Kind, "deduplicated": true}
		var existingPayload detachedPayload
		if json.Unmarshal([]byte(existing.Payload), &existingPayload) == nil && len(existingPayload.ReservedTaskIDs) > 0 {
			result["task_ids"] = existingPayload.ReservedTaskIDs
		}
		if c.json {
			c.write(result, "")
		} else {
			fmt.Fprintf(c.stdout, "Reused operation %s (%s).\n", existing.ID, existing.State)
		}
		return nil
	}
	now := time.Now().UTC()
	clusterKey := ""
	if kind == "submit" && len(submitPreview.Previews) > 0 {
		clusterKey = submitPreview.Previews[0].Cluster
	} else if kind == "pull" {
		clusterKey = detachedPullCluster(c.ctx, db, p, payload.Args, workingDir)
	}
	op := &model.Operation{ID: id, Kind: kind, ProjectID: p.ProjectID, ClusterKey: clusterKey, State: model.OperationQueued,
		Stage: "queued", Payload: string(payloadBytes), Result: "{}", DedupKey: dedupKey, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateOperation(c.ctx, op); err != nil {
		return err
	}
	if len(payload.ReservedTaskIDs) > 0 {
		relations := make([]model.OperationTask, 0, len(payload.ReservedTaskIDs))
		for index, taskID := range payload.ReservedTaskIDs {
			relations = append(relations, model.OperationTask{
				OperationID: op.ID, TaskID: taskID, Ordinal: index, State: "admitted",
			})
		}
		if err := db.ReplaceOperationTasks(c.ctx, op.ID, relations); err != nil {
			return err
		}
	}
	keepOperationState = true
	result := map[string]any{"operation_id": op.ID, "state": op.State, "kind": op.Kind}
	if len(payload.ReservedTaskIDs) > 0 {
		result["task_ids"] = payload.ReservedTaskIDs
	}
	if c.json {
		c.write(result, "")
	} else {
		fmt.Fprintf(c.stdout, "Admitted operation %s (queued).\n", op.ID)
	}
	return nil
}

func detachedInputHash(payload detachedPayload) string {
	if payload.IntentHash != "" {
		return payload.IntentHash
	}
	data, _ := json.Marshal(payload.Args)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// detachedPullCluster resolves the cluster before a durable pull is queued so
// the dispatcher can serialize remote work for one cluster. A mixed-source
// pull is deliberately assigned a shared key; it may span several clusters
// and must not be allowed to bypass any per-cluster session limit.
func detachedPullCluster(ctx context.Context, db *store.Store, p model.Project, args []string, workingDir string) string {
	values := make([]string, 0, len(args))
	valueFlags := map[string]bool{"--include": true, "--glob": true, "--from": true, "--batch": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name := arg
		if before, _, ok := strings.Cut(arg, "="); ok {
			name = before
		}
		if valueFlags[name] {
			if name == "--batch" {
				if strings.Contains(arg, "=") {
					values = append(values, strings.TrimPrefix(arg, "--batch="))
				} else if i+1 < len(args) {
					i++
					values = append(values, args[i])
				}
			} else if !strings.Contains(arg, "=") {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		values = append(values, arg)
	}
	clusters := map[string]bool{}
	for _, value := range values {
		if strings.HasPrefix(value, "jr_") {
			if task, err := db.GetTask(ctx, value); err == nil && task.ClusterName != "" {
				clusters[task.ClusterName] = true
			}
			continue
		}
		if strings.HasPrefix(value, "jb_") {
			tasks, err := db.ListTasks(ctx, p.ProjectID)
			if err == nil {
				for _, task := range tasks {
					if task.Metadata["batch_id"] == value && task.ClusterName != "" {
						clusters[task.ClusterName] = true
					}
				}
			}
			continue
		}
		absolute := value
		if !filepath.IsAbs(absolute) {
			absolute = filepath.Join(workingDir, value)
		}
		src, _, err := source.Resolve(p, absolute)
		if err != nil {
			continue
		}
		if task, err := db.LatestTask(ctx, p.ProjectID, src.RelativePath); err == nil && task.ClusterName != "" {
			clusters[task.ClusterName] = true
		}
	}
	if len(clusters) == 1 {
		for cluster := range clusters {
			return cluster
		}
	}
	if len(clusters) > 1 {
		return "__mixed__"
	}
	return ""
}

func previewManifestHash(previews []app.Preview) string {
	type input struct {
		Source   model.Source          `json:"source"`
		Manifest []model.ManifestEntry `json:"manifest"`
	}
	inputs := make([]input, 0, len(previews))
	for _, preview := range previews {
		manifest := append([]model.ManifestEntry(nil), preview.InputManifest...)
		sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
		inputs = append(inputs, input{Source: preview.Source, Manifest: manifest})
	}
	data, _ := json.Marshal(inputs)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hasFlagPrefix(args []string, prefix string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

func flagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
	}
	return ""
}

type detachedPayload struct {
	Args            []string `json:"args"`
	ProjectID       string   `json:"project_id"`
	CWD             string   `json:"cwd"`
	Config          string   `json:"config"`
	DataRoot        string   `json:"data_root,omitempty"`
	SnapshotRoot    string   `json:"snapshot_root,omitempty"`
	IntentHash      string   `json:"intent_hash,omitempty"`
	ReservedTaskIDs []string `json:"reserved_task_ids,omitempty"`
}

func prepareDetachedSnapshot(p model.Project, cwd, operationID string, previews []app.Preview, args []string) (string, []string, error) {
	root := filepath.Join(paths.OperationDataDir(), operationID, "project")
	if err := os.MkdirAll(filepath.Join(root, ".joyrun"), 0o700); err != nil {
		return "", nil, fault.Wrap("SOURCE_SNAPSHOT_FAILED", "cannot create detached operation snapshot", false, err)
	}
	if err := copyFile(filepath.Join(p.Root, ".joyrun", "project.yaml"), filepath.Join(root, ".joyrun", "project.yaml")); err != nil {
		_ = os.RemoveAll(filepath.Dir(root))
		return "", nil, fault.Wrap("SOURCE_SNAPSHOT_FAILED", "cannot copy Project identity into detached snapshot", false, err)
	}
	if data, err := os.ReadFile(filepath.Join(p.Root, ".joyrunignore")); err == nil {
		if !bytes.Contains(data, []byte(".joyrun")) {
			data = append(data, []byte("\n.joyrun/\n")...)
		}
		_ = os.WriteFile(filepath.Join(root, ".joyrunignore"), data, 0o600)
	} else {
		_ = os.WriteFile(filepath.Join(root, ".joyrunignore"), []byte(".joyrun/\n"), 0o600)
	}
	seen := map[string]bool{}
	for _, preview := range previews {
		for _, entry := range preview.InputManifest {
			src := filepath.Join(p.Root, filepath.FromSlash(filepath.Join(preview.Source.WorkDir, entry.Path)))
			dst := filepath.Join(root, filepath.FromSlash(filepath.Join(preview.Source.WorkDir, entry.Path)))
			if seen[dst] {
				continue
			}
			var copyErr error
			for attempt := 0; attempt < 2; attempt++ {
				copyErr = copyVerified(src, dst, entry.SHA256)
				if copyErr == nil || !strings.Contains(copyErr.Error(), "source changed") {
					break
				}
			}
			if copyErr != nil {
				_ = os.RemoveAll(filepath.Dir(root))
				code := "SOURCE_SNAPSHOT_FAILED"
				if strings.Contains(copyErr.Error(), "source changed") {
					code = "SOURCE_CHANGED_DURING_SNAPSHOT"
				}
				return "", nil, fault.Wrap(code, fmt.Sprintf("cannot snapshot %s", entry.Path), false, copyErr)
			}
			seen[dst] = true
		}
	}
	rewritten, err := rewriteExplicitSources(args, previews)
	if err != nil {
		_ = os.RemoveAll(filepath.Dir(root))
		return "", nil, err
	}
	_ = cwd
	return root, rewritten, nil
}

func rewriteExplicitSources(args []string, previews []app.Preview) ([]string, error) {
	valueFlags := map[string]bool{"-t": true, "--target": true, "--set": true, "--include": true, "--partition": true, "--auto-pull": true}
	inlineValueFlags := map[string]bool{"--target": true, "-t": true, "--set": true, "--include": true, "--partition": true, "--auto-pull": true}
	result := make([]string, 0, len(args))
	sourceIndex := 0
	skipValue := false
	for _, arg := range args {
		if skipValue {
			result = append(result, arg)
			skipValue = false
			continue
		}
		if valueFlags[arg] {
			result = append(result, arg)
			skipValue = true
			continue
		}
		if strings.Contains(arg, "=") {
			name, _, _ := strings.Cut(arg, "=")
			if inlineValueFlags[name] {
				result = append(result, arg)
				continue
			}
		}
		if strings.HasPrefix(arg, "-") {
			result = append(result, arg)
			continue
		}
		if sourceIndex >= len(previews) {
			return nil, fault.New("SOURCE_SNAPSHOT_FAILED", "detached submit source count changed during preview", false)
		}
		result = append(result, previews[sourceIndex].Source.RelativePath)
		sourceIndex++
	}
	if sourceIndex != len(previews) {
		return nil, fault.New("SOURCE_SNAPSHOT_FAILED", "detached submit could not map all source paths", false)
	}
	return result, nil
}

func copyVerified(src, dst, expectedHash string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hash), in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != expectedHash {
		return fmt.Errorf("source changed while detached snapshot was copied")
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

func (c *command) operation(db *store.Store, args []string) error {
	if len(args) == 0 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun operation <list|show|tasks|wait|cancel|retry> [ID]", false)
	}
	switch args[0] {
	case "list":
		ops, err := db.ListOperations(c.ctx, "")
		if err != nil {
			return err
		}
		if c.json {
			c.write(map[string]any{"operations": ops}, "")
			return nil
		}
		for _, op := range ops {
			fmt.Fprintf(c.stdout, "%s\t%s\t%s\t%s\n", op.ID, op.Kind, op.State, op.Stage)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return fault.New("INVALID_ARGUMENT", "usage: joyrun operation show ID", false)
		}
		op, err := db.GetOperation(c.ctx, args[1])
		if err != nil {
			return err
		}
		c.write(op, fmt.Sprintf("Operation %s: %s (%s)\n", op.ID, op.State, op.Stage))
		return nil
	case "tasks":
		if len(args) != 2 {
			return fault.New("INVALID_ARGUMENT", "usage: joyrun operation tasks ID", false)
		}
		tasks, err := db.OperationTasks(c.ctx, args[1])
		if err != nil {
			return err
		}
		if c.json {
			c.write(map[string]any{"operation_id": args[1], "tasks": tasks}, "")
			return nil
		}
		for _, task := range tasks {
			fmt.Fprintf(c.stdout, "%d\t%s\t%s\n", task.Ordinal, task.TaskID, task.State)
		}
		return nil
	case "wait":
		if len(args) < 2 || len(args) > 6 {
			return fault.New("INVALID_ARGUMENT", "usage: joyrun operation wait ID [--until accepted|terminal] [--timeout duration]", false)
		}
		until := "terminal"
		var timeout time.Duration
		for i := 2; i < len(args); i++ {
			if args[i] == "--until" && i+1 < len(args) {
				until = args[i+1]
				if until != "accepted" && until != "terminal" {
					return fault.New("INVALID_ARGUMENT", "--until must be accepted or terminal", false)
				}
				i++
				continue
			}
			if args[i] == "--timeout" && i+1 < len(args) {
				parsed, parseErr := time.ParseDuration(args[i+1])
				if parseErr != nil || parsed <= 0 {
					return fault.New("INVALID_ARGUMENT", "--timeout must be a positive duration", false)
				}
				timeout = parsed
				i++
				continue
			}
			return fault.New("INVALID_ARGUMENT", "usage: joyrun operation wait ID [--until accepted|terminal] [--timeout duration]", false)
		}
		waitCtx := c.ctx
		cancel := func() {}
		if timeout > 0 {
			waitCtx, cancel = context.WithTimeout(waitCtx, timeout)
		}
		defer cancel()
		for {
			current, getErr := db.GetOperation(waitCtx, args[1])
			if getErr != nil {
				return getErr
			}
			accepted := operationTerminal(current.State) || current.Stage == "scheduler_accepted" || current.Stage == "accepted"
			if (until == "accepted" && accepted) || (until == "terminal" && operationTerminal(current.State)) {
				c.write(current, fmt.Sprintf("Operation %s is %s.\n", current.ID, current.State))
				if current.State == model.OperationFailed || current.State == model.OperationCancelled {
					c.exitCode = 1
				}
				return nil
			}
			select {
			case <-waitCtx.Done():
				return fault.Wrap("OPERATION_WAIT_TIMEOUT", "operation did not reach a terminal state before timeout", true, waitCtx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
	case "cancel", "retry":
		if len(args) != 2 {
			return fault.New("INVALID_ARGUMENT", "usage: joyrun operation cancel|retry ID", false)
		}
		op, err := db.GetOperation(c.ctx, args[1])
		if err != nil {
			return err
		}
		if args[0] == "cancel" {
			if op.State == model.OperationSucceeded || op.State == model.OperationFailed || op.State == model.OperationCancelled {
				return fault.New("OPERATION_TERMINAL", "operation is already terminal", false)
			}
			if op.State == model.OperationRunning && op.Kind == "submit" {
				return fault.New("OPERATION_CANCEL_UNSAFE", "a running submit operation cannot be cancelled after scheduler submission may have started", false)
			}
			op.State, op.Stage, op.ErrorCode, op.ErrorMessage, op.Retryable = model.OperationCancelled, "cancelled", "OPERATION_CANCELLED", "cancelled by user", false
		} else {
			if op.State != model.OperationFailed && op.State != model.OperationCancelled {
				return fault.New("OPERATION_NOT_RETRYABLE", "only failed or cancelled operations can be retried", false)
			}
			op.State, op.Stage, op.ErrorCode, op.ErrorMessage, op.Retryable, op.NextAttemptAt, op.FinishedAt = model.OperationQueued, "queued", "", "", false, nil, nil
		}
		op.LeaseOwner, op.LeaseExpiresAt, op.UpdatedAt = "", nil, time.Now().UTC()
		if err := db.UpdateOperation(c.ctx, &op); err != nil {
			return err
		}
		if args[0] == "cancel" {
			c.cancelRunningOperation(op.ID)
		}
		_ = db.AppendOperationEvent(c.ctx, op.ID, op.State, op.Stage, "operation state changed by user", nil)
		c.write(op, fmt.Sprintf("Operation %s is %s.\n", op.ID, op.State))
		return nil
	default:
		return fault.New("INVALID_ARGUMENT", "usage: joyrun operation <list|show|tasks|wait|cancel|retry> [ID]", false)
	}
}

func operationTerminal(state string) bool {
	return state == model.OperationSucceeded || state == model.OperationPartiallySucceeded ||
		state == model.OperationFailed || state == model.OperationCancelled
}

func (c *command) operationWorker(ctx context.Context, version string) {
	owner, _ := identity.New("dw_")
	db, err := store.Open(paths.DatabaseFile())
	if err != nil {
		return
	}
	defer db.Close()
	var wg sync.WaitGroup
	defer wg.Wait()
	var mu sync.Mutex
	active := map[string]int{}
	const maxConcurrent = 8
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		default:
		}
		mu.Lock()
		busy := 0
		for _, count := range active {
			busy += count
		}
		mu.Unlock()
		if busy >= maxConcurrent {
			select {
			case <-ctx.Done():
				wg.Wait()
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		mu.Lock()
		blocked := make([]string, 0, len(active))
		mixedBusy := active["__mixed__"] > 0
		for key, count := range active {
			if count > 0 && key != "__local__" {
				blocked = append(blocked, key)
			}
		}
		mu.Unlock()
		// A mixed-source pull may touch more than one cluster. Serialize it
		// against all remote work, otherwise it could defeat the per-cluster
		// session bound while another cluster operation is active.
		if mixedBusy {
			select {
			case <-ctx.Done():
				wg.Wait()
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		op, ok, claimErr := db.ClaimNextOperationExcept(ctx, owner, 30*time.Second, blocked)
		if claimErr == nil && ok {
			key := op.ClusterKey
			if key == "" {
				key = "__local__"
			}
			mu.Lock()
			clusterBusy := active[key] > 0
			if key == "__mixed__" {
				for activeKey, count := range active {
					if count > 0 && activeKey != "__local__" {
						clusterBusy = true
						break
					}
				}
			}
			if !clusterBusy {
				active[key]++
			}
			mu.Unlock()
			if clusterBusy {
				c.requeueOperation(context.Background(), db, &op)
				continue
			}
			wg.Add(1)
			go func(op model.Operation, key string) {
				defer wg.Done()
				defer func() {
					mu.Lock()
					active[key]--
					mu.Unlock()
				}()
				c.runOperation(ctx, db, op, version)
			}(op, key)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (c *command) requeueOperation(ctx context.Context, db *store.Store, op *model.Operation) {
	if op == nil {
		return
	}
	// The operation was claimed by this worker just before the dispatcher
	// discovered a conflicting cluster slot. Release it with the ownership
	// predicate so an expired lease cannot overwrite a replacement worker's
	// state.
	_ = db.RequeueOwnedOperation(ctx, op.ID, op.LeaseOwner)
}

// pollWorker is intentionally conservative: one refresh per Project every
// thirty seconds, using App.StatusAll's existing per-cluster batching. It is
// a cache refresher only; it never submits, cancels, or pulls a task.
func (c *command) pollWorker(ctx context.Context, version string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	last := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollProjects(ctx, version, last)
		}
	}
}

func (c *command) pollProjects(ctx context.Context, version string, last map[string]time.Time) {
	db, err := store.Open(paths.DatabaseFile())
	if err != nil {
		return
	}
	projects, err := db.ListProjects(ctx)
	if err != nil {
		_ = db.Close()
		return
	}
	for _, p := range projects {
		if p.Root == "" {
			continue
		}
		tasks, taskErr := db.ListTasks(ctx, p.ProjectID)
		if taskErr != nil {
			continue
		}
		interval, active := pollInterval(tasks)
		if !active || time.Since(last[p.ProjectID]) < interval {
			continue
		}
		last[p.ProjectID] = time.Now()
		var stdout, stderr bytes.Buffer
		_ = run(ctx, []string{"status", "--all"}, version, &stdout, &stderr, p.Root, true)
		c.enqueueAutomaticPulls(ctx, p, version)
	}
	_ = db.Close()
}

func pollInterval(tasks []model.Task) (time.Duration, bool) {
	active := false
	interval := 30 * time.Second
	now := time.Now().UTC()
	for _, task := range tasks {
		switch task.ComputeState {
		case model.ComputeSubmissionUncertain:
			active = true
			age := now.Sub(task.UpdatedAt)
			uncertainInterval := 10 * time.Second
			if age >= 10*time.Minute {
				uncertainInterval = 2 * time.Minute
			} else if age >= 2*time.Minute {
				uncertainInterval = 30 * time.Second
			}
			if uncertainInterval < interval {
				interval = uncertainInterval
			}
		case model.ComputeQueued, model.ComputeRunning, model.ComputeUnknown:
			active = true
		}
	}
	return interval, active
}

func (c *command) enqueueAutomaticPulls(ctx context.Context, p model.Project, _ string) {
	db, err := store.Open(paths.DatabaseFile())
	if err != nil {
		return
	}
	defer db.Close()
	tasks, err := db.ListTasks(ctx, p.ProjectID)
	if err != nil {
		return
	}
	for _, task := range tasks {
		policy := task.Metadata["auto_pull"]
		if policy == "" || task.Metadata["auto_pull_enqueued"] == "1" || task.PullState == model.PullSucceeded {
			continue
		}
		eligible := task.ComputeState == model.ComputeCompleted
		if policy == "terminal" {
			eligible = eligible || task.ComputeState == model.ComputeFailed || task.ComputeState == model.ComputeCancelled
		}
		if !eligible {
			continue
		}
		task.Metadata["auto_pull_enqueued"] = "1"
		task.UpdatedAt = time.Now().UTC()
		if err := db.UpdateTask(ctx, &task); err != nil {
			continue
		}
		if err := c.admitDetachedAt("pull", []string{task.ID}, p.Root); err != nil {
			task.Metadata["auto_pull_enqueued"] = ""
			task.UpdatedAt = time.Now().UTC()
			_ = db.UpdateTask(ctx, &task)
		}
	}
}

func (c *command) runOperation(ctx context.Context, db *store.Store, op model.Operation, version string) {
	var payload detachedPayload
	if err := json.Unmarshal([]byte(op.Payload), &payload); err != nil {
		if payload.DataRoot != "" {
			_ = os.RemoveAll(payload.DataRoot)
		}
		c.finishOperation(ctx, db, &op, model.OperationFailed, "invalid operation payload", "OPERATION_INVALID", false, nil)
		return
	}
	workdir := payload.CWD
	if payload.SnapshotRoot != "" {
		workdir = payload.SnapshotRoot
	} else if payload.ProjectID != "" {
		if bound, err := db.GetProject(ctx, payload.ProjectID); err == nil && bound.Root != "" {
			workdir = bound.Root
		}
	}
	runArgs := append([]string{}, payload.Args...)
	opCtx, cancelOperation := context.WithCancel(ctx)
	c.registerRunningOperation(op.ID, cancelOperation)
	defer c.unregisterRunningOperation(op.ID)
	leaseCtx, stopLease := context.WithCancel(opCtx)
	defer stopLease()
	go c.renewOperationLease(leaseCtx, db, op, cancelOperation)
	if err := c.updateOperationStage(ctx, db, &op, "executing", "running durable command"); err != nil {
		cancelOperation()
		return
	}
	var stdout, stderr bytes.Buffer
	code := c.executeOperation(opCtx, db, op.Kind, runArgs, version, workdir, payload.Config,
		payload.SnapshotRoot, payload.ReservedTaskIDs, &stdout, &stderr)
	if relations := operationTasksFromResult(op.ID, op.Kind, stdout.String()); len(relations) > 0 {
		_ = db.ReplaceOperationTasks(context.Background(), op.ID, relations)
	}
	if opCtx.Err() != nil {
		stopLease()
		if current, getErr := db.GetOperation(context.Background(), op.ID); getErr == nil && current.State == model.OperationCancelled {
			return
		}
		_ = db.RequeueOwnedOperation(context.Background(), op.ID, op.LeaseOwner)
		return
	}
	stopLease()
	op.Result = string(mustJSON(map[string]string{"stdout": stdout.String(), "stderr": stderr.String()}))
	if code == 0 {
		if payload.DataRoot != "" {
			_ = os.RemoveAll(payload.DataRoot)
		}
		c.markAutomaticPull(ctx, db, payload.Args, true)
		c.finishOperation(ctx, db, &op, model.OperationSucceeded, "operation completed", "", false, nil)
		return
	}
	c.markAutomaticPull(ctx, db, payload.Args, false)
	c.finishOperation(ctx, db, &op, model.OperationFailed, strings.TrimSpace(stderr.String()), "OPERATION_COMMAND_FAILED", false, nil)
}

// executeOperation is the daemon's typed execution boundary.  It constructs
// the same App services used by the daemon worker and invokes the command-specific
// handlers without re-entering the top-level CLI router (which would make a
// durable operation accidentally look like a new client request).
func (c *command) executeOperation(ctx context.Context, db *store.Store, kind string, args []string,
	version, cwd, configPath, snapshotRoot string, reservedTaskIDs []string, stdout, stderr *bytes.Buffer) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		c.writeErrorTo(stderr, err)
		return 1
	}
	controlPath := ""
	if runtime.GOOS != "windows" {
		if runtimePaths, pathErr := daemon.DefaultPaths(); pathErr == nil {
			controlPath = filepath.Join(filepathDir(runtimePaths.Endpoint), "control-%C")
		}
	}
	runner := remote.SSH{Stderr: stderr, ControlPath: controlPath}
	application := &app.App{Config: cfg, Store: db, Runner: runner,
		Scheduler:         scheduler.Slurm{Runner: runner},
		Transfer:          transfer.Manager{Stderr: stderr, Runner: runner, ControlPath: controlPath},
		TransferInspector: transfer.Manager{Stderr: stderr, Runner: runner, ControlPath: controlPath}, Progress: stderr}
	worker := *c
	worker.ctx, worker.cwd, worker.config, worker.stdout, worker.stderr = ctx, cwd, configPath, stdout, stderr
	worker.inDaemon = true
	// Operation results are an internal machine-readable protocol. Human
	// formatting remains the responsibility of the client-side command.
	worker.json = true
	worker.exitCode = 0
	var handlerErr error
	switch kind {
	case "submit":
		if len(reservedTaskIDs) == 1 && snapshotRoot != "" {
			result, submitErr := application.ExecuteReservedSubmit(ctx, reservedTaskIDs[0], snapshotRoot)
			if submitErr != nil {
				handlerErr = submitErr
			} else {
				data, _ := json.Marshal(result)
				_, _ = stdout.Write(append(data, '\n'))
			}
		} else {
			handlerErr = worker.submit(application, args)
		}
	case "pull":
		handlerErr = worker.pull(application, args)
	default:
		// Future operation kinds can be added here without changing the
		// durable worker. Unknown kinds retain the old parser as a safe,
		// explicit compatibility path.
		return run(ctx, append([]string{"--config", configPath}, args...), version, stdout, stderr, cwd, true)
	}
	if handlerErr != nil {
		worker.writeErrorTo(stderr, handlerErr)
		return 1
	}
	return worker.exitCode
}

func (c *command) writeErrorTo(w io.Writer, err error) {
	if err == nil {
		return
	}
	if c.json {
		data, _ := json.Marshal(map[string]any{"ok": false, "error": fault.As(err)})
		_, _ = fmt.Fprintln(w, string(data))
		return
	}
	_, _ = fmt.Fprintln(w, "Error: "+err.Error())
}

func (c *command) registerRunningOperation(id string, cancel context.CancelFunc) {
	runningOperationCancels.Store(id, cancel)
}

func (c *command) unregisterRunningOperation(id string) {
	runningOperationCancels.Delete(id)
}

func (c *command) cancelRunningOperation(id string) {
	if value, ok := runningOperationCancels.Load(id); ok {
		value.(context.CancelFunc)()
	}
}

func (c *command) updateOperationStage(ctx context.Context, db *store.Store, op *model.Operation, stage, message string) error {
	if op == nil {
		return nil
	}
	op.Stage = stage
	op.UpdatedAt = time.Now().UTC()
	if err := db.UpdateOperationOwned(ctx, op, op.LeaseOwner); err != nil {
		return err
	}
	return db.AppendOperationEvent(ctx, op.ID, op.State, stage, message, nil)
}

func (c *command) renewOperationLease(ctx context.Context, db *store.Store, op model.Operation, cancelOperation context.CancelFunc) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			expires := now.UTC().Add(30 * time.Second)
			if err := db.RenewOperationLease(context.Background(), op.ID, op.LeaseOwner, expires); err != nil {
				// A worker that loses its durable lease must stop before it can
				// perform more external work. The replacement worker will
				// reconcile the task fence before retrying submission.
				cancelOperation()
				return
			}
		}
	}
}

func (c *command) markAutomaticPull(ctx context.Context, db *store.Store, args []string, success bool) {
	selector := 0
	if len(args) > 0 && args[0] == "pull" {
		selector = 1 // tolerate operation payloads written by older daemons
	}
	if len(args) <= selector || !strings.HasPrefix(args[selector], "jr_") {
		return
	}
	task, err := db.GetTask(ctx, args[selector])
	if err != nil {
		return
	}
	if task.Metadata["auto_pull"] == "" || task.Metadata["auto_pull_enqueued"] != "1" {
		return
	}
	if success {
		task.Metadata["auto_pull_done"] = "1"
	}
	task.Metadata["auto_pull_enqueued"] = ""
	task.UpdatedAt = time.Now().UTC()
	_ = db.UpdateTask(ctx, &task)
}

func operationTasksFromResult(operationID, kind, output string) []model.OperationTask {
	if strings.TrimSpace(output) == "" {
		return nil
	}
	output = unwrapCommandResult(output)
	if kind == "submit" {
		var single struct {
			Task model.Task `json:"task"`
		}
		if json.Unmarshal([]byte(output), &single) == nil && single.Task.ID != "" {
			state := single.Task.ComputeState
			if state == "" {
				state = "submitted"
			}
			return []model.OperationTask{{OperationID: operationID, TaskID: single.Task.ID, Ordinal: 0, State: state}}
		}
		var batch submitOutput
		if json.Unmarshal([]byte(output), &batch) != nil {
			return nil
		}
		result := make([]model.OperationTask, 0, len(batch.Tasks)+len(batch.Failures))
		for index, task := range batch.Tasks {
			state := task.ComputeState
			if state == "" {
				state = "submitted"
			}
			result = append(result, model.OperationTask{
				OperationID: operationID, TaskID: task.ID, Ordinal: index, State: state,
			})
		}
		for index, failure := range batch.Failures {
			if failure.TaskID == "" {
				continue
			}
			result = append(result, model.OperationTask{
				OperationID: operationID, TaskID: failure.TaskID,
				Ordinal: len(batch.Tasks) + index, State: "failed",
				Result: batchFailureMessage(failure),
			})
		}
		return result
	}
	if kind == "pull" {
		var result pullOutput
		if json.Unmarshal([]byte(output), &result) != nil {
			return nil
		}
		tasks := make([]model.OperationTask, 0, len(result.Tasks))
		for index, item := range result.Tasks {
			state := item.PullState
			if state == "" {
				state = "pulled"
			}
			tasks = append(tasks, model.OperationTask{
				OperationID: operationID, TaskID: item.TaskID, Ordinal: index, State: state,
			})
		}
		for index, failure := range result.Failures {
			if failure.TaskID == "" {
				continue
			}
			tasks = append(tasks, model.OperationTask{
				OperationID: operationID, TaskID: failure.TaskID,
				Ordinal: len(result.Tasks) + index, State: "failed",
				Result: batchFailureMessage(failure),
			})
		}
		return tasks
	}
	return nil
}

// Detached workers invoke the normal command handlers with --json. The
// command writer wraps every successful response in {"ok":true,"result":...},
// while operation progress storage needs the command-specific result object.
// Accept both the envelope and the direct form so this protocol remains
// compatible with older workers and focused unit tests.
func unwrapCommandResult(output string) string {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err == nil && len(envelope.Result) > 0 && string(envelope.Result) != "null" {
		return string(envelope.Result)
	}
	return output
}

func batchFailureMessage(failure app.BatchFailure) string {
	if failure.Error == nil {
		return "task failed"
	}
	return failure.Error.Error()
}

func (c *command) finishOperation(ctx context.Context, db *store.Store, op *model.Operation, state, message, code string, retryable bool, _ error) {
	if current, err := db.GetOperation(ctx, op.ID); err == nil && current.State == model.OperationCancelled {
		return
	}
	now := time.Now().UTC()
	owner := op.LeaseOwner
	op.State, op.Stage, op.ErrorCode, op.ErrorMessage, op.Retryable = state, state, code, message, retryable
	op.LeaseOwner, op.LeaseExpiresAt, op.FinishedAt, op.UpdatedAt = "", nil, &now, now
	var updateErr error
	if owner != "" {
		updateErr = db.UpdateOperationOwned(ctx, op, owner)
	} else {
		updateErr = db.UpdateOperation(ctx, op)
	}
	if updateErr != nil {
		return
	}
	_ = db.AppendOperationEvent(ctx, op.ID, state, op.Stage, message, nil)
}

func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }
