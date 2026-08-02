package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
)

// CreateOperation records durable intent before the daemon performs an
// external side effect. A non-empty dedup key is unique per operation kind.
func (s *Store) CreateOperation(ctx context.Context, op *model.Operation) error {
	if op == nil || op.ID == "" || op.Kind == "" || op.ProjectID == "" {
		return fault.New("OPERATION_INVALID", "operation id, kind, and project are required", false)
	}
	now := time.Now().UTC()
	if op.CreatedAt.IsZero() {
		op.CreatedAt = now
	}
	if op.UpdatedAt.IsZero() {
		op.UpdatedAt = op.CreatedAt
	}
	if op.State == "" {
		op.State = model.OperationQueued
	}
	if op.Stage == "" {
		op.Stage = "queued"
	}
	if op.Payload == "" {
		op.Payload = "{}"
	}
	if op.Result == "" {
		op.Result = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO operations(
id,kind,project_id,cluster_key,dedup_key,state,stage,payload,result,attempt,max_attempts,
retry_deadline_at,next_attempt_at,lease_owner,lease_expires_at,error_code,error_message,
retryable,created_at,started_at,updated_at,finished_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, operationValues(*op)...)
	if err != nil {
		if isUniqueViolation(err) {
			return fault.Wrap("OPERATION_DUPLICATE", "an equivalent operation is already admitted", false, err)
		}
		return fault.Wrap("DATABASE_FAILED", "cannot create operation", true, err)
	}
	return s.AppendOperationEvent(ctx, op.ID, op.State, op.Stage, "operation admitted", nil)
}

func (s *Store) GetOperation(ctx context.Context, id string) (model.Operation, error) {
	row := s.db.QueryRowContext(ctx, operationColumns+" FROM operations WHERE id=?", id)
	op, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Operation{}, fault.New("OPERATION_NOT_FOUND", fmt.Sprintf("operation %s not found", id), false)
	}
	if err != nil {
		return model.Operation{}, fault.Wrap("DATABASE_FAILED", "cannot read operation", false, err)
	}
	return op, nil
}

// ReplaceOperationTasks atomically replaces the task progress rows for an
// operation. Admission and worker retries can therefore publish a complete
// current view without leaving stale ordinal rows behind.
func (s *Store) ReplaceOperationTasks(ctx context.Context, operationID string, tasks []model.OperationTask) error {
	if operationID == "" {
		return fault.New("OPERATION_INVALID", "operation id is required", false)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot update operation tasks", true, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM operation_tasks WHERE operation_id=?", operationID); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot clear operation tasks", true, err)
	}
	for index, task := range tasks {
		if task.OperationID == "" {
			task.OperationID = operationID
		}
		if task.OperationID != operationID || task.TaskID == "" {
			return fault.New("OPERATION_INVALID", "operation task relation is invalid", false)
		}
		if task.Ordinal == 0 && index > 0 {
			task.Ordinal = index
		}
		if task.State == "" {
			task.State = "pending"
		}
		if task.Result == "" {
			task.Result = "{}"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO operation_tasks(operation_id,task_id,ordinal,state,result)
VALUES(?,?,?,?,?)`, task.OperationID, task.TaskID, task.Ordinal, task.State, task.Result); err != nil {
			return fault.Wrap("DATABASE_FAILED", "cannot insert operation task", true, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot commit operation tasks", true, err)
	}
	return nil
}

func (s *Store) OperationTasks(ctx context.Context, operationID string) ([]model.OperationTask, error) {
	if operationID == "" {
		return nil, fault.New("OPERATION_INVALID", "operation id is required", false)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id,task_id,ordinal,state,result
FROM operation_tasks WHERE operation_id=? ORDER BY ordinal`, operationID)
	if err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot list operation tasks", true, err)
	}
	defer rows.Close()
	result := []model.OperationTask{}
	for rows.Next() {
		var task model.OperationTask
		if err := rows.Scan(&task.OperationID, &task.TaskID, &task.Ordinal, &task.State, &task.Result); err != nil {
			return nil, fault.Wrap("DATABASE_FAILED", "cannot decode operation task", false, err)
		}
		result = append(result, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot finish listing operation tasks", true, err)
	}
	return result, nil
}

func (s *Store) FindOperationByDedup(ctx context.Context, kind, dedupKey string) (model.Operation, bool, error) {
	if kind == "" || dedupKey == "" {
		return model.Operation{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, operationColumns+" FROM operations WHERE kind=? AND dedup_key=? ORDER BY created_at DESC LIMIT 1", kind, dedupKey)
	op, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Operation{}, false, nil
	}
	if err != nil {
		return model.Operation{}, false, fault.Wrap("DATABASE_FAILED", "cannot find operation by deduplication key", true, err)
	}
	return op, true, nil
}

func (s *Store) ListOperations(ctx context.Context, projectID string) ([]model.Operation, error) {
	query := operationColumns + " FROM operations ORDER BY created_at DESC"
	args := []any{}
	if projectID != "" {
		query = operationColumns + " FROM operations WHERE project_id=? ORDER BY created_at DESC"
		args = append(args, projectID)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot list operations", true, err)
	}
	defer rows.Close()
	var result []model.Operation
	for rows.Next() {
		op, scanErr := scanOperation(rows)
		if scanErr != nil {
			return nil, fault.Wrap("DATABASE_FAILED", "cannot decode operation", false, scanErr)
		}
		result = append(result, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot finish listing operations", true, err)
	}
	return result, nil
}

// ClaimNextOperation atomically claims one due queued operation. Expired
// leases are reclaimed; the caller must persist a terminal state afterwards.
func (s *Store) ClaimNextOperation(ctx context.Context, owner string, lease time.Duration) (model.Operation, bool, error) {
	return s.ClaimNextOperationExcept(ctx, owner, lease, nil)
}

// ClaimNextOperationExcept is the dispatcher variant of ClaimNextOperation.
// Blocked cluster keys are skipped before claiming, so a busy cluster cannot
// starve work for another cluster merely because its operation is older.
func (s *Store) ClaimNextOperationExcept(ctx context.Context, owner string, lease time.Duration, blocked []string) (model.Operation, bool, error) {
	if owner == "" {
		return model.Operation{}, false, fault.New("OPERATION_INVALID", "operation owner is required", false)
	}
	now := time.Now().UTC()
	expires := now.Add(lease)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Operation{}, false, fault.Wrap("DATABASE_BUSY", "cannot claim operation", true, err)
	}
	defer tx.Rollback()
	// Expired running work is safe to reclaim. Submit workers must re-run the
	// existing App idempotency/reconciliation checks before any scheduler call.
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET state=?,lease_owner='',lease_expires_at=NULL,
updated_at=? WHERE state=? AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		model.OperationQueued, now.Format(time.RFC3339Nano), model.OperationRunning, now.Format(time.RFC3339Nano)); err != nil {
		return model.Operation{}, false, fault.Wrap("DATABASE_FAILED", "cannot reclaim operation leases", true, err)
	}
	query := operationColumns + ` FROM operations WHERE state=? AND (next_attempt_at IS NULL OR next_attempt_at<=?)`
	args := []any{model.OperationQueued, now.Format(time.RFC3339Nano)}
	if len(blocked) > 0 {
		placeholders := make([]string, 0, len(blocked))
		for _, key := range blocked {
			if key == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, key)
		}
		if len(placeholders) > 0 {
			query += " AND (cluster_key='' OR cluster_key NOT IN (" + strings.Join(placeholders, ",") + "))"
		}
	}
	query += " ORDER BY created_at LIMIT 1"
	row := tx.QueryRowContext(ctx, query, args...)
	op, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Operation{}, false, nil
	}
	if err != nil {
		return model.Operation{}, false, fault.Wrap("DATABASE_FAILED", "cannot inspect queued operation", false, err)
	}
	started := now
	op.State, op.LeaseOwner, op.LeaseExpiresAt, op.StartedAt = model.OperationRunning, owner, &expires, &started
	op.Attempt++
	op.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET state=?,stage=?,attempt=?,lease_owner=?,lease_expires_at=?,started_at=?,updated_at=? WHERE id=? AND state=?`,
		op.State, op.Stage, op.Attempt, op.LeaseOwner, expires.Format(time.RFC3339Nano), started.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), op.ID, model.OperationQueued); err != nil {
		return model.Operation{}, false, fault.Wrap("DATABASE_FAILED", "cannot claim operation", true, err)
	}
	if err := tx.Commit(); err != nil {
		return model.Operation{}, false, fault.Wrap("DATABASE_FAILED", "cannot commit operation claim", true, err)
	}
	return op, true, nil
}

func (s *Store) UpdateOperation(ctx context.Context, op *model.Operation) error {
	if op == nil || op.ID == "" {
		return fault.New("OPERATION_INVALID", "operation id is required", false)
	}
	if op.UpdatedAt.IsZero() {
		op.UpdatedAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE operations SET state=?,stage=?,payload=?,result=?,attempt=?,max_attempts=?,
retry_deadline_at=?,next_attempt_at=?,lease_owner=?,lease_expires_at=?,error_code=?,error_message=?,retryable=?,
started_at=?,updated_at=?,finished_at=? WHERE id=?`, operationUpdateValues(*op)...)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot update operation", true, err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fault.New("OPERATION_NOT_FOUND", fmt.Sprintf("operation %s not found", op.ID), false)
	}
	return nil
}

// UpdateOperationOwned applies a worker's full operation update only while
// that worker still owns the running lease. This prevents a paused worker
// whose lease was reclaimed from overwriting a replacement worker's state.
func (s *Store) UpdateOperationOwned(ctx context.Context, op *model.Operation, owner string) error {
	if op == nil || op.ID == "" || owner == "" {
		return fault.New("OPERATION_INVALID", "operation id and lease owner are required", false)
	}
	if op.UpdatedAt.IsZero() {
		op.UpdatedAt = time.Now().UTC()
	}
	values := operationUpdateValues(*op)
	values = append(values, model.OperationRunning, owner)
	result, err := s.db.ExecContext(ctx, `UPDATE operations SET state=?,stage=?,payload=?,result=?,attempt=?,max_attempts=?,
retry_deadline_at=?,next_attempt_at=?,lease_owner=?,lease_expires_at=?,error_code=?,error_message=?,retryable=?,
started_at=?,updated_at=?,finished_at=? WHERE id=? AND state=? AND lease_owner=?`, values...)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot update owned operation", true, err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fault.New("OPERATION_LEASE_LOST", "operation lease is no longer owned by this worker", true)
	}
	return nil
}

// RenewOperationLease updates only the lease fields. Keeping this narrow is
// important: the worker may update stage, result, or error details while the
// lease ticker is running, and a full-row write from a stale Operation value
// could otherwise overwrite those newer fields.
func (s *Store) RenewOperationLease(ctx context.Context, operationID, owner string, expires time.Time) error {
	if operationID == "" || owner == "" {
		return fault.New("OPERATION_INVALID", "operation id and lease owner are required", false)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE operations
SET lease_expires_at=?, updated_at=?
WHERE id=? AND state=? AND lease_owner=?`, expires.UTC().Format(time.RFC3339Nano),
		time.Now().UTC().Format(time.RFC3339Nano), operationID, model.OperationRunning, owner)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot renew operation lease", true, err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fault.New("OPERATION_LEASE_LOST", "operation lease is no longer owned by this worker", true)
	}
	return nil
}

// RequeueOwnedOperation releases a worker's lease without writing a stale
// full operation row. The owner predicate prevents a worker whose lease was
// reclaimed from overwriting the replacement worker's state.
func (s *Store) RequeueOwnedOperation(ctx context.Context, operationID, owner string) error {
	if operationID == "" || owner == "" {
		return fault.New("OPERATION_INVALID", "operation id and lease owner are required", false)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE operations
SET state=?, stage=?, retryable=?, next_attempt_at=NULL, lease_owner='', lease_expires_at=NULL,
error_code='', error_message='', updated_at=?
WHERE id=? AND state=? AND lease_owner=?`, model.OperationQueued, "queued", boolInt(true),
		time.Now().UTC().Format(time.RFC3339Nano), operationID, model.OperationRunning, owner)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot requeue operation", true, err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fault.New("OPERATION_LEASE_LOST", "operation lease is no longer owned by this worker", true)
	}
	return nil
}

func (s *Store) AppendOperationEvent(ctx context.Context, operationID, state, stage, message string, data map[string]string) error {
	if data == nil {
		data = map[string]string{}
	}
	encodedBytes, err := json.Marshal(data)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot encode operation event", false, err)
	}
	encoded := string(encodedBytes)
	_, err = s.db.ExecContext(ctx, `INSERT INTO operation_events(operation_id,state,stage,message,data,created_at) VALUES(?,?,?,?,?,?)`,
		operationID, state, stage, message, encoded, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot append operation event", true, err)
	}
	return nil
}

const operationColumns = `SELECT id,kind,project_id,cluster_key,dedup_key,state,stage,payload,result,attempt,max_attempts,
retry_deadline_at,next_attempt_at,lease_owner,lease_expires_at,error_code,error_message,retryable,created_at,started_at,updated_at,finished_at`

func operationValues(op model.Operation) []any {
	return []any{op.ID, op.Kind, op.ProjectID, op.ClusterKey, op.DedupKey, op.State, op.Stage, op.Payload, op.Result,
		op.Attempt, op.MaxAttempts, nullableTime(op.RetryDeadlineAt), nullableTime(op.NextAttemptAt), op.LeaseOwner,
		nullableTime(op.LeaseExpiresAt), op.ErrorCode, op.ErrorMessage, boolInt(op.Retryable), op.CreatedAt.UTC().Format(time.RFC3339Nano),
		nullableTime(op.StartedAt), op.UpdatedAt.UTC().Format(time.RFC3339Nano), nullableTime(op.FinishedAt)}
}

func operationUpdateValues(op model.Operation) []any {
	return []any{op.State, op.Stage, op.Payload, op.Result, op.Attempt, op.MaxAttempts, nullableTime(op.RetryDeadlineAt),
		nullableTime(op.NextAttemptAt), op.LeaseOwner, nullableTime(op.LeaseExpiresAt), op.ErrorCode, op.ErrorMessage,
		boolInt(op.Retryable), nullableTime(op.StartedAt), op.UpdatedAt.UTC().Format(time.RFC3339Nano), nullableTime(op.FinishedAt), op.ID}
}

func scanOperation(row scanner) (model.Operation, error) {
	var op model.Operation
	var retryDeadline, nextAttempt, leaseExpires, created, started, updated, finished sql.NullString
	var retryable int
	err := row.Scan(&op.ID, &op.Kind, &op.ProjectID, &op.ClusterKey, &op.DedupKey, &op.State, &op.Stage, &op.Payload,
		&op.Result, &op.Attempt, &op.MaxAttempts, &retryDeadline, &nextAttempt, &op.LeaseOwner, &leaseExpires,
		&op.ErrorCode, &op.ErrorMessage, &retryable, &created, &started, &updated, &finished)
	if err != nil {
		return op, err
	}
	op.Retryable = retryable != 0
	var parseErr error
	if op.CreatedAt, parseErr = parseOperationTime(created.String); parseErr != nil {
		return op, parseErr
	}
	if op.UpdatedAt, parseErr = parseOperationTime(updated.String); parseErr != nil {
		return op, parseErr
	}
	if op.RetryDeadlineAt, parseErr = nullableParsedTime(retryDeadline); parseErr != nil {
		return op, parseErr
	}
	if op.NextAttemptAt, parseErr = nullableParsedTime(nextAttempt); parseErr != nil {
		return op, parseErr
	}
	if op.LeaseExpiresAt, parseErr = nullableParsedTime(leaseExpires); parseErr != nil {
		return op, parseErr
	}
	if op.StartedAt, parseErr = nullableParsedTime(started); parseErr != nil {
		return op, parseErr
	}
	if op.FinishedAt, parseErr = nullableParsedTime(finished); parseErr != nil {
		return op, parseErr
	}
	return op, nil
}

func parseOperationTime(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }
func nullableParsedTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value.String)
	return &t, err
}
func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	value := strings.ToLower(err.Error())
	return strings.Contains(value, "unique") || strings.Contains(value, "constraint failed")
}
