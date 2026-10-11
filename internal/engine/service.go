package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/robfig/cron/v3"
	"vaultpilot/internal/platform"
	"vaultpilot/internal/store"
)

type Service struct {
	rt              *platform.Runtime
	mu              sync.Mutex
	runMu           sync.Mutex
	catalogMu       sync.Mutex
	configurationMu sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	sem             chan struct{}
	queue           chan string
	active          map[string]string
	pending         map[string]bool
	cancels         map[string]context.CancelFunc
	cancelled       map[string]bool
	locks           sync.Map
	scheduler       *cron.Cron
	scheduleIDs     map[string]cron.EntryID
	started         bool
}

func New(rt *platform.Runtime) (*Service, error) {
	n := rt.Config.GlobalConcurrency
	if n < 1 {
		n = 2
	}
	s := &Service{rt: rt, sem: make(chan struct{}, n), queue: make(chan string, 1024), active: map[string]string{}, pending: map[string]bool{}, cancels: map[string]context.CancelFunc{}, cancelled: map[string]bool{}, scheduleIDs: map[string]cron.EntryID{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.scheduler = cron.New()
	tasks, err := rt.Store.All(s.ctx, "backup_runs")
	if err != nil {
		return nil, err
	}
	for _, raw := range tasks {
		var t Task
		if json.Unmarshal(raw, &t) == nil && !terminal(t.Status) {
			t.Status = "interrupted"
			t.Stage = "interrupted"
			t.Error = "服务重启导致任务中断；检查仓库后可重新运行"
			t.Code = "NODE_RESTARTED"
			t.FinishedAt = now()
			if err = s.save(&t); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	s.replayPending()
	s.reloadSchedules()
	s.scheduler.Start()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		s.pollCloud()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				s.pollCloud()
			}
		}
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-ctx.Done():
				s.cancel()
				return
			case <-s.ctx.Done():
				return
			case id := <-s.queue:
				select {
				case s.sem <- struct{}{}:
					s.wg.Add(1)
					go func() { defer s.wg.Done(); defer func() { <-s.sem }(); s.execute(id) }()
				case <-s.ctx.Done():
					return
				}
			}
		}
	}()
}
func (s *Service) Close() {
	s.cancel()
	s.scheduler.Stop()
	s.mu.Lock()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
func (s *Service) save(t *Task) error {
	v, err := s.rt.Store.Save(context.Background(), "backup_runs", t.ID, -1, t)
	if err == nil {
		t.Version = v
	}
	if t.Kind == "restore" {
		_, _ = s.rt.Store.Save(context.Background(), "restore_jobs", t.ID, -1, t)
	} else if t.Kind != "backup" && t.Kind != "preview" {
		_, _ = s.rt.Store.Save(context.Background(), "maintenance_jobs", t.ID, -1, t)
	}
	return err
}
func (s *Service) log(t *Task, line string) {
	_ = s.rt.Store.Log(context.Background(), t.ID, "info", line)
}
func (s *Service) stage(t *Task, stage string) { t.Status = "running"; t.Stage = stage; s.save(t) }
func (s *Service) enqueue(kind, plan, key string, request any) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(key) > 200 {
		return Task{}, fail("INVALID_IDEMPOTENCY_KEY", "幂等键过长")
	}
	identity := kind + ":" + plan + ":" + key
	if key != "" {
		var id string
		err := s.rt.Store.DB.QueryRow("SELECT resource FROM idempotency WHERE key=?", identity).Scan(&id)
		if err == nil {
			var t Task
			err = s.rt.Store.Get(s.ctx, "backup_runs", id, &t)
			return t, err
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Task{}, err
		}
	}
	if s.active["account-clear"] != "" {
		return Task{}, fail("ACCOUNT_CLEAR_RUNNING", "网盘清空任务正在执行，请等待完成后再操作")
	}
	pendingClear, pendingErr := s.rt.Store.Meta(s.ctx, "account_clear_pending")
	if pendingErr != nil && !errors.Is(pendingErr, sql.ErrNoRows) {
		return Task{}, pendingErr
	}
	if pendingClear != "" && kind != "account-clear" {
		return Task{}, fail("ACCOUNT_CLEAR_PENDING", "网盘清空尚未完成，请在存储账号中选择原账号重新清空，完成后再运行其他任务")
	}
	if kind == "account-clear" {
		if err := s.noActiveTasks(s.ctx, ""); err != nil {
			return Task{}, err
		}
	}

	if kind == "backup" && s.active[plan] != "" {
		s.pending[plan] = true
		_ = s.rt.Store.SetMeta(context.Background(), "pending:"+plan, "1")
		if key != "" {
			_, _ = s.rt.Store.DB.Exec("INSERT INTO idempotency(key,resource,created_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING", identity, s.active[plan], now())
		}
		var t Task
		err := s.rt.Store.Get(s.ctx, "backup_runs", s.active[plan], &t)
		return t, err
	}
	if kind == "discover" && s.active["discover"] != "" {
		var task Task
		err := s.rt.Store.Get(s.ctx, "backup_runs", s.active["discover"], &task)
		return task, err
	}
	b, err := json.Marshal(request)
	if err != nil {
		return Task{}, err
	}
	t := Task{ID: uid(), Kind: kind, PlanID: plan, Status: "queued", Stage: "queued", Request: b}
	if err = s.save(&t); err != nil {
		return Task{}, err
	}
	if key != "" {
		_, err = s.rt.Store.DB.Exec("INSERT INTO idempotency(key,resource,created_at) VALUES(?,?,?)", identity, t.ID, now())
		if err != nil {
			return Task{}, err
		}
	}
	if kind == "backup" {
		s.active[plan] = t.ID
	}
	if kind == "account-clear" {
		s.active["account-clear"] = t.ID
	}
	if kind == "discover" {
		s.active["discover"] = t.ID
	}
	select {
	case s.queue <- t.ID:
	default:
		t.Status = "failed"
		t.Code = "QUEUE_FULL"
		t.Error = "任务队列已满"
		s.save(&t)
		delete(s.active, plan)
		if kind == "account-clear" {
			delete(s.active, "account-clear")
		}
		if kind == "discover" {
			delete(s.active, "discover")
		}
		return Task{}, fail(t.Code, t.Error)
	}
	return t, nil
}
func (s *Service) execute(id string) {
	var t Task
	if s.rt.Store.Get(s.ctx, "backup_runs", id, &t) != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.mu.Lock()
	s.cancels[id] = cancel
	if t.Kind == "backup" && !s.pending[t.PlanID] {
		_ = s.rt.Store.SetMeta(context.Background(), "pending:"+t.PlanID, "0")
	}
	if s.cancelled[id] {
		cancel()
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.cancels, id)
		delete(s.cancelled, id)
		if t.Kind == "account-clear" {
			delete(s.active, "account-clear")
		}
		if t.Kind == "discover" {
			delete(s.active, "discover")
		}
		pending := false
		if t.Kind == "backup" {
			delete(s.active, t.PlanID)
			pending = s.pending[t.PlanID]
			delete(s.pending, t.PlanID)
			if !pending || s.ctx.Err() == nil {
				_ = s.rt.Store.SetMeta(context.Background(), "pending:"+t.PlanID, "0")
			}
		}
		s.mu.Unlock()
		if pending && s.ctx.Err() == nil {
			_, _ = s.enqueue("backup", t.PlanID, "", nil)
		}
	}()
	if t.CancelRequested {
		cancel()
	}
	t.StartedAt = now()
	s.stage(&t, "checking")
	var err error
	if resourceErr := s.rt.CheckCacheBudget(); resourceErr != nil {
		err = fail("CACHE_LIMIT_EXCEEDED", resourceErr.Error())
	} else if t.Kind == "check" || t.Kind == "retention" || t.Kind == "migrate" || t.Kind == "account-clear" {
		allowed, e := s.rt.AllowedMaintenance(time.Now())
		if e != nil {
			err = fail("INVALID_MAINTENANCE_WINDOW", e.Error())
		} else if !allowed {
			err = fail("OUTSIDE_MAINTENANCE_WINDOW", "当前时间不在配置的维护窗口内")
		}
	}
	if err != nil {
	} else if s.rt.CloudPending() {
		err = fail("CLOUD_CONNECTION_REQUIRED", "请先授权账号并使用原备份密码连接云端备份库")
	} else if !s.rt.Keys.Unlocked() {
		err = fail("VAULT_LOCKED", "请先解锁备份库")
	} else if s.rt.RecoveryMode() && (t.Kind == "backup" || t.Kind == "retention" || t.Kind == "migrate" || t.Kind == "account-clear") {
		err = fail("RECOVERY_READ_ONLY", "恢复节点禁止备份、清理和迁移")
	} else {
		switch t.Kind {
		case "backup", "preview":
			err = s.backup(ctx, &t)
			if err != nil && t.Kind == "backup" {
				var plan Plan
				_ = s.rt.Store.Get(ctx, "backup_plans", t.PlanID, &plan)
				for attempt := 1; attempt <= plan.RetryCount; attempt++ {
					var f *failure
					if !errors.As(err, &f) || !f.Retryable || ctx.Err() != nil {
						break
					}
					t.Attempt = attempt
					s.stage(&t, "retry_wait")
					delay := time.NewTimer(time.Duration(1<<uint(attempt)) * time.Second)
					select {
					case <-ctx.Done():
						delay.Stop()
						err = ctx.Err()
					case <-delay.C:
						err = s.backup(ctx, &t)
					}
					if err == nil {
						break
					}
				}
			}
		case "restore":
			err = s.restore(ctx, &t)
		case "discover":
			err = s.discover(ctx, &t)
		case "check":
			err = s.check(ctx, &t)
		case "retention":
			err = s.retain(ctx, &t)
		case "migrate":
			err = s.migrate(ctx, &t)
		case "account-clear":
			err = s.clearAccounts(ctx, &t)
		default:
			err = fail("UNKNOWN_TASK", "不支持此任务类型")
		}
	}
	if err == nil {
		t.Status = "completed"
		t.Stage = "completed"
		t.Progress = 1
	} else {
		t.Status = "failed"
		t.Stage = "failed"
		t.Error = err.Error()
		t.Code = "ENGINE_FAILED"
		var f *failure
		if errors.As(err, &f) {
			t.Code = f.Code
			t.Retryable = f.Retryable
		}
		if t.Code == "ACCOUNT_CLEAR_PARTIAL" || t.Code == "RESTORE_PARTIAL" || t.Code == "PROTECTION_DEGRADED" || t.partialRestore {
			t.Status = "partial_failed"
		}
		if ctx.Err() != nil {
			t.Status = "cancelled"
			t.Stage = "cancelled"
			t.Code = "CANCELLED"
		}
		s.log(&t, t.Error)
	}
	t.FinishedAt = now()
	s.save(&t)
	if t.Kind == "backup" {
		if notifyErr := s.rt.NotifyBackup(t.ID); notifyErr != nil {
			s.log(&t, "Telegram 通知入队失败，请检查通知设置与数据库状态")
		}
	}
	if t.Kind == "discover" && s.rt.RecoveryMode() {
		_ = s.rt.Store.SetMeta(context.Background(), "cloud_last_checked", now())
		if err == nil {
			_ = s.rt.Store.SetMeta(context.Background(), "cloud_last_success", now())
			_ = s.rt.Store.SetMeta(context.Background(), "cloud_last_error", "")
			var request struct {
				Signature string `json:"cloud_signature"`
			}
			if json.Unmarshal(t.Request, &request) == nil && request.Signature != "" {
				_ = s.rt.Store.SetMeta(context.Background(), "cloud_auto_signature", request.Signature)
			}
		} else {
			_ = s.rt.Store.SetMeta(context.Background(), "cloud_last_error", t.Error)
		}
	}
}
func (s *Service) lock(key string) func() {
	v, _ := s.locks.LoadOrStore(key, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}
func parseSchedule(p Plan) (cron.Schedule, error) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return nil, err
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return parser.Parse("CRON_TZ=" + loc.String() + " " + p.Schedule)
}
func nextRuns(p Plan) []string {
	schedule, err := parseSchedule(p)
	if err != nil {
		return nil
	}
	next := time.Now()
	var out []string
	for i := 0; i < 3; i++ {
		next = schedule.Next(next)
		out = append(out, next.Format(time.RFC3339))
	}
	return out
}
func (s *Service) reloadSchedules() {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	for _, id := range s.scheduleIDs {
		s.scheduler.Remove(id)
	}
	s.scheduleIDs = map[string]cron.EntryID{}
	if s.rt.RecoveryMode() {
		return
	}
	raws, _ := s.rt.Store.All(s.ctx, "backup_plans")
	for _, raw := range raws {
		var p Plan
		if json.Unmarshal(raw, &p) != nil || !p.Enabled || p.Schedule == "" {
			continue
		}
		schedule, err := parseSchedule(p)
		if err != nil {
			continue
		}
		plan := p.ID
		s.scheduleIDs[plan] = s.scheduler.Schedule(schedule, cron.FuncJob(func() {
			_, _ = s.enqueue("backup", plan, "", nil)
			_ = s.rt.Store.SetMeta(context.Background(), "schedule_last:"+plan, now())
		}))
		last, e := s.rt.Store.Meta(s.ctx, "schedule_last:"+plan)
		if e == nil {
			at, e := time.Parse(time.RFC3339Nano, last)
			if e == nil && schedule.Next(at).Before(time.Now()) {
				_, _ = s.enqueue("backup", plan, "", nil)
				_ = s.rt.Store.SetMeta(s.ctx, "schedule_last:"+plan, now())
			}
		} else {
			_ = s.rt.Store.SetMeta(s.ctx, "schedule_last:"+plan, now())
		}
	}
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func problem(w http.ResponseWriter, err error) {
	status := 400
	code := "INVALID_REQUEST"
	retry := false
	var f *failure
	if errors.As(err, &f) {
		code = f.Code
		retry = f.Retryable
	}
	if errors.Is(err, store.ErrConflict) || code == "VERSION_CONFLICT" {
		status = 409
		code = "VERSION_CONFLICT"
	}
	if errors.Is(err, sql.ErrNoRows) {
		status = 404
		code = "NOT_FOUND"
	}
	write(w, status, map[string]any{"error": err.Error(), "code": code, "retryable": retry})
}
func decode(r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 2<<20))
	return d.Decode(out)
}
func paging(r *http.Request) (int, int) {
	l, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	o, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if l < 1 {
		l = 50
	}
	if l > 500 {
		l = 500
	}
	if o < 0 {
		o = 0
	}
	return l, o
}
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/plans", s.plansList)
	mux.HandleFunc("POST /api/plans", s.planSave)
	mux.HandleFunc("PUT /api/plans/{id}", s.planSave)
	mux.HandleFunc("DELETE /api/plans/{id}", s.planDelete)
	for _, kind := range []string{"run", "preview"} {
		k := kind
		mux.HandleFunc("POST /api/plans/{id}/"+k, func(w http.ResponseWriter, r *http.Request) {
			var p Plan
			if err := s.rt.Store.Get(r.Context(), "backup_plans", r.PathValue("id"), &p); err != nil {
				problem(w, err)
				return
			}
			taskKind := "backup"
			if k == "preview" {
				taskKind = "preview"
			}
			t, err := s.enqueue(taskKind, p.ID, r.Header.Get("Idempotency-Key"), nil)
			if err != nil {
				problem(w, err)
				return
			}
			write(w, 202, t)
		})
	}
	mux.HandleFunc("GET /api/tasks", s.tasksList)
	mux.HandleFunc("GET /api/tasks/{id}", s.taskGet)
	mux.HandleFunc("GET /api/tasks/{id}/logs", s.taskLogs)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.taskCancel)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/repositories", s.repositoriesList)
	mux.HandleFunc("GET /api/recovery-points", s.pointsList)
	mux.HandleFunc("GET /api/recovery-points/{id}/files", s.pointFiles)
	mux.HandleFunc("POST /api/recovery-points/{id}/protect", s.protect)
	mux.HandleFunc("POST /api/restore/preview", s.restorePreviewHTTP)
	mux.HandleFunc("POST /api/restore", s.submit("restore"))
	mux.HandleFunc("POST /api/security/discover", s.submit("discover"))
	mux.HandleFunc("GET /api/security/cloud-connection", s.cloudState)
	mux.HandleFunc("POST /api/security/cloud-connection", s.cloudPublish)
	mux.HandleFunc("PUT /api/security/cloud-connection", s.cloudAutoSave)
	mux.HandleFunc("GET /api/security/cloud-vaults", s.cloudVaults)
	mux.HandleFunc("POST /api/security/cloud-connect", s.cloudJoin)
	mux.HandleFunc("POST /api/maintenance/check", s.submit("check"))
	mux.HandleFunc("POST /api/maintenance/retention/preview", s.retentionPreviewHTTP)
	mux.HandleFunc("POST /api/maintenance/retention", s.submit("retention"))
	mux.HandleFunc("POST /api/maintenance/migrate", s.submit("migrate"))
	mux.HandleFunc("POST /api/accounts/clear/preview", s.clearPreviewHTTP)
	mux.HandleFunc("POST /api/accounts/clear", s.clearSubmitHTTP)
}
func (s *Service) submit(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req json.RawMessage
		if err := decode(r, &req); err != nil {
			problem(w, err)
			return
		}
		t, err := s.enqueue(kind, "", r.Header.Get("Idempotency-Key"), req)
		if err != nil {
			problem(w, err)
			return
		}
		write(w, 202, t)
	}
}
func (s *Service) plansList(w http.ResponseWriter, r *http.Request) {
	l, o := paging(r)
	raws, total, err := s.rt.Store.List(r.Context(), "backup_plans", l, o)
	if err != nil {
		problem(w, err)
		return
	}
	out := []Plan{}
	for _, raw := range raws {
		var p Plan
		_ = json.Unmarshal(raw, &p)
		p.NextRuns = nextRuns(p)
		out = append(out, p)
	}
	write(w, 200, map[string]any{"items": out, "total": total})
}
func (s *Service) planSave(w http.ResponseWriter, r *http.Request) {
	s.configurationMu.RLock()
	defer s.configurationMu.RUnlock()
	if err := s.clearMutationAllowed(r.Context()); err != nil {
		problem(w, err)
		return
	}

	var p Plan
	if err := decode(r, &p); err != nil {
		problem(w, err)
		return
	}
	if p.Name == "" || len(p.SourceIDs) == 0 || p.PoolID == "" {
		problem(w, fail("INVALID_PLAN", "计划名称、备份源和存储池不能为空"))
		return
	}
	if p.Timezone == "" {
		p.Timezone = "Asia/Shanghai"
	}
	if p.Schedule != "" {
		if _, err := parseSchedule(p); err != nil {
			problem(w, fail("INVALID_SCHEDULE", "Cron 或时区无效"))
			return
		}
	}
	if p.RetryCount < 0 || p.RetryCount > 8 || p.Retention.Daily < 0 || p.Retention.Weekly < 0 || p.Retention.Monthly < 0 {
		problem(w, fail("INVALID_PLAN", "重试及保留参数无效"))
		return
	}
	var pool Pool
	if err := s.rt.Store.Get(r.Context(), "storage_pools", p.PoolID, &pool); err != nil {
		problem(w, err)
		return
	}
	paths := []string{}
	for _, id := range p.SourceIDs {
		var src Source
		if err := s.rt.Store.Get(r.Context(), "sources", id, &src); err != nil {
			problem(w, err)
			return
		}
		for _, old := range paths {
			if within(old, src.Path) || within(src.Path, old) {
				problem(w, fail("OVERLAPPING_SOURCES", "同一计划不能包含重叠备份源"))
				return
			}
		}
		paths = append(paths, src.Path)
	}
	expected := p.Version
	if r.PathValue("id") != "" {
		p.ID = r.PathValue("id")
		if expected < 1 {
			problem(w, fail("VERSION_REQUIRED", "修改计划需要 version"))
			return
		}
	} else if p.ID == "" {
		p.ID = uid()
		expected = 0
	}
	v, err := s.rt.Store.Save(r.Context(), "backup_plans", p.ID, expected, p)
	if err != nil {
		problem(w, err)
		return
	}
	p.Version = v
	p.NextRuns = nextRuns(p)
	s.reloadSchedules()
	write(w, 201, p)
}
func (s *Service) planDelete(w http.ResponseWriter, r *http.Request) {
	s.configurationMu.RLock()
	defer s.configurationMu.RUnlock()
	if err := s.clearMutationAllowed(r.Context()); err != nil {
		problem(w, err)
		return
	}

	s.mu.Lock()
	active := s.active[r.PathValue("id")]
	s.mu.Unlock()
	if active != "" {
		problem(w, fail("PLAN_RUNNING", "计划正在运行"))
		return
	}
	if err := s.rt.Store.Delete(r.Context(), "backup_plans", r.PathValue("id")); err != nil {
		problem(w, err)
		return
	}
	s.reloadSchedules()
	_ = s.rt.Store.SetMeta(r.Context(), "pending:"+r.PathValue("id"), "0")
	w.WriteHeader(204)
}
func (s *Service) tasksList(w http.ResponseWriter, r *http.Request) {
	l, o := paging(r)
	items, n, err := s.rt.Store.List(r.Context(), "backup_runs", l, o)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "total": n})
}
func (s *Service) taskGet(w http.ResponseWriter, r *http.Request) {
	var t Task
	if err := s.rt.Store.Get(r.Context(), "backup_runs", r.PathValue("id"), &t); err != nil {
		problem(w, err)
		return
	}
	write(w, 200, t)
}
func (s *Service) taskCancel(w http.ResponseWriter, r *http.Request) {
	var t Task
	if err := s.rt.Store.Get(r.Context(), "backup_runs", r.PathValue("id"), &t); err != nil {
		problem(w, err)
		return
	}
	if terminal(t.Status) {
		write(w, 200, t)
		return
	}
	t.CancelRequested = true
	s.save(&t)
	s.mu.Lock()
	s.cancelled[t.ID] = true
	if t.PlanID != "" {
		delete(s.pending, t.PlanID)
		_ = s.rt.Store.SetMeta(context.Background(), "pending:"+t.PlanID, "0")
	}
	if cancel := s.cancels[t.ID]; cancel != nil {
		cancel()
	}
	s.mu.Unlock()
	write(w, 202, t)
}
func (s *Service) taskLogs(w http.ResponseWriter, r *http.Request) {
	l, o := paging(r)
	var n int
	_ = s.rt.Store.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM task_logs WHERE task_id=?", r.PathValue("id")).Scan(&n)
	rows, err := s.rt.Store.DB.QueryContext(r.Context(), "SELECT id,message,created_at FROM task_logs WHERE task_id=? ORDER BY id LIMIT ? OFFSET ?", r.PathValue("id"), l, o)
	if err != nil {
		problem(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var line, at string
		_ = rows.Scan(&id, &line, &at)
		items = append(items, map[string]any{"seq": id, "line": line, "at": at})
	}
	write(w, 200, map[string]any{"items": items, "total": n})
}
func (s *Service) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	f, ok := w.(http.Flusher)
	if !ok {
		return
	}
	timer := time.NewTicker(3 * time.Second)
	defer timer.Stop()
	for {
		items, _, err := s.rt.Store.List(r.Context(), "backup_runs", 50, 0)
		if err != nil {
			return
		}
		b, _ := json.Marshal(items)
		_, _ = fmt.Fprintf(w, "event: tasks\ndata: %s\n\n", b)
		f.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
	}
}
func (s *Service) repositoriesList(w http.ResponseWriter, r *http.Request) {
	l, o := paging(r)
	const predicate = "coalesce(json_extract(data,'$.status'),'') <> 'retired'"
	rows, err := s.rt.Store.DB.QueryContext(r.Context(), "SELECT data FROM repositories WHERE "+predicate+" ORDER BY updated_at DESC,id DESC LIMIT ? OFFSET ?", l, o)
	if err != nil {
		problem(w, err)
		return
	}
	items := []Repository{}
	for rows.Next() {
		var raw []byte
		var repo Repository
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &repo); err != nil {
			break
		}
		repo.Password = ""
		items = append(items, repo)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		problem(w, err)
		return
	}
	var total int
	if err = s.rt.Store.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM repositories WHERE "+predicate).Scan(&total); err != nil {
		problem(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "total": total})
}
func (s *Service) pointsList(w http.ResponseWriter, r *http.Request) {
	l, o := paging(r)
	rows, err := s.rt.Store.DB.QueryContext(r.Context(), "SELECT data FROM recovery_points WHERE coalesce(json_extract(data,\x27$.deleted_at\x27),\x27\x27)=\x27\x27 ORDER BY updated_at DESC LIMIT ? OFFSET ?", l, o)
	if err != nil {
		problem(w, err)
		return
	}
	items := []Point{}
	for rows.Next() {
		var b []byte
		_ = rows.Scan(&b)
		var p Point
		_ = json.Unmarshal(b, &p)
		items = append(items, p)
	}
	rows.Close()
	var n int
	_ = s.rt.Store.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM recovery_points WHERE coalesce(json_extract(data,\x27$.deleted_at\x27),\x27\x27)=\x27\x27").Scan(&n)
	write(w, 200, map[string]any{"items": items, "total": n})
}
func (s *Service) protect(w http.ResponseWriter, r *http.Request) {
	s.configurationMu.RLock()
	defer s.configurationMu.RUnlock()
	if err := s.clearMutationAllowed(r.Context()); err != nil {
		problem(w, err)
		return
	}

	if s.rt.RecoveryMode() {
		problem(w, fail("RECOVERY_READ_ONLY", "恢复节点禁止修改云端保护状态"))
		return
	}
	if !s.rt.Keys.Unlocked() {
		problem(w, fail("VAULT_LOCKED", "请先解锁备份库"))
		return
	}
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	var body struct {
		Protected bool `json:"protected"`
		Version   int  `json:"version"`
	}
	if err := decode(r, &body); err != nil {
		problem(w, err)
		return
	}
	var p Point
	if err := s.rt.Store.Get(r.Context(), "recovery_points", r.PathValue("id"), &p); err != nil {
		problem(w, err)
		return
	}
	if p.DeletedAt != "" {
		problem(w, fail("POINT_DELETED", "恢复点已删除"))
		return
	}
	if body.Version < 1 || body.Version != p.Version {
		problem(w, fail("VERSION_CONFLICT", "恢复点已更新，请刷新后重试"))
		return
	}
	p.Protected = body.Protected
	p.CommitID = uid()
	pool, err := s.catalogPool(r.Context(), p.PlanID)
	if err != nil {
		problem(w, err)
		return
	}
	task := Task{ID: uid(), Kind: "protect"}
	commitErr := s.commit(r.Context(), &task, pool, &p, nil)
	if commitErr != nil && !degraded(commitErr) {
		problem(w, commitErr)
		return
	}
	v, err := s.rt.Store.Save(r.Context(), "recovery_points", p.ID, body.Version, p)
	if err != nil {
		problem(w, err)
		return
	}
	p.Version = v
	_ = s.rt.Store.Audit(r.Context(), "recovery_point.protect", p.ID)
	if commitErr != nil {
		write(w, 202, map[string]any{"point": p, "error": commitErr.Error(), "code": "PROTECTION_DEGRADED", "retryable": true})
		return
	}
	write(w, 200, p)
}
func (s *Service) catalogPool(ctx context.Context, planID string) (Pool, error) {
	var plan Plan
	var pool Pool
	if s.rt.Store.Get(ctx, "backup_plans", planID, &plan) == nil && s.rt.Store.Get(ctx, "storage_pools", plan.PoolID, &pool) == nil {
		return pool, nil
	}
	pool.CatalogReplicas = 2
	raws, err := s.rt.Store.All(ctx, "accounts")
	if err != nil {
		return pool, err
	}
	for _, raw := range raws {
		var a Account
		_ = json.Unmarshal(raw, &a)
		if a.Verified && a.Enabled {
			pool.AccountIDs = append(pool.AccountIDs, a.ID)
		}
	}
	return pool, nil
}
func (s *Service) pointFiles(w http.ResponseWriter, r *http.Request) {
	unlockIndex := s.lock("recovery-index")
	defer unlockIndex()
	var p Point
	if err := s.rt.Store.Get(r.Context(), "recovery_points", r.PathValue("id"), &p); err != nil {
		problem(w, err)
		return
	}
	l, o := paging(r)
	q := r.URL.Query()
	where, args := entryFilter(p.RunID, q.Get("source_id"), q.Get("prefix"), q.Get("search"))
	var n int
	if err := s.rt.Store.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM file_entries WHERE "+where, args...).Scan(&n); err != nil {
		problem(w, err)
		return
	}
	entries, err := s.entries(r.Context(), where, args, l, o)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, map[string]any{"items": entries, "total": n})
}
func entryFilter(run, source, prefix, search string) (string, []any) {
	w := "run_id=?"
	a := []any{run}
	if source != "" {
		w += " AND source_id=?"
		a = append(a, source)
	}
	if prefix != "" {
		w += " AND (path=? OR substr(path,1,?)=?)"
		a = append(a, prefix, utf8.RuneCountInString(prefix)+1, prefix+"/")
	}
	if search != "" {
		w += " AND instr(path,?)>0"
		a = append(a, search)
	}
	return w, a
}
func (s *Service) entries(ctx context.Context, where string, args []any, limit, offset int) ([]Entry, error) {
	a := append(append([]any{}, args...), limit, offset)
	rows, err := s.rt.Store.DB.QueryContext(ctx, "SELECT source_id,path,kind,size,mtime_ns,mode,link_target,repo_id,snapshot_path FROM file_entries WHERE "+where+" ORDER BY source_id,path LIMIT ? OFFSET ?", a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.SourceID, &e.Path, &e.Kind, &e.Size, &e.MtimeNS, &e.Mode, &e.LinkTarget, &e.RepositoryID, &e.SnapshotPath); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func sortPoints(points []Point) {
	sort.Slice(points, func(i, j int) bool { return strings.Compare(points[i].FinishedAt, points[j].FinishedAt) > 0 })
}

func (s *Service) replayPending() {
	if s.rt.RecoveryMode() {
		return
	}
	rows, err := s.rt.Store.DB.QueryContext(s.ctx, "SELECT key FROM metadata WHERE key LIKE 'pending:%' AND value='1'")
	if err != nil {
		return
	}
	plans := []string{}
	for rows.Next() {
		var key string
		if rows.Scan(&key) == nil {
			plans = append(plans, strings.TrimPrefix(key, "pending:"))
		}
	}
	rows.Close()
	for _, planID := range plans {
		var plan Plan
		if s.rt.Store.Get(s.ctx, "backup_plans", planID, &plan) != nil {
			_ = s.rt.Store.SetMeta(s.ctx, "pending:"+planID, "0")
			continue
		}
		_, _ = s.enqueue("backup", planID, "", nil)
	}
}
