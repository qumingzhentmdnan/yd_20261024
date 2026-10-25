package vm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"safeagent/internal/host/vm/store"
)

// Manager 是生命周期操作与 SQLite 记录的唯一协调者。
type Manager struct {
	// resourceLock 阻止其他数据库的 Manager 控制同一后端资源。
	resourceLock io.Closer
	// store 保存配置、epoch 和固定退出结果。
	store *store.Store
	// backend 标识本 Manager 使用的具体后端。
	backend string
	// driver 执行宿主资源副作用。
	driver Driver
	// mu 保护 locks、running 与 closed。
	mu sync.Mutex
	// failedLaunch 保存已确认无遗留进程但尚未提交停止事实的实例。
	failedLaunch map[VMInstance]bool
	// locks 串行化同一资源的操作，等待可取消。
	locks map[string]chan struct{}
	// running 保存当前进程内可观察的运行句柄。
	running map[VMInstance]*managedRun
	// closed 阻止数据库关闭后的新操作。
	closed bool
}

// managedRun 在退出结果持久化后唤醒所有等待者。
type managedRun struct {
	// process 负责实际进程控制和唯一回收。
	process Process
	// done 在结果持久化完成或失败后关闭。
	done chan struct{}
	// exit 是真实进程退出结果。
	exit VMExit
	// persisted 表示真实退出结果已成功提交，关闭前必须满足。
	persisted atomic.Bool
	// err 是进程观察或首次结果持久化错误。
	err error
}

var _ VMBackend = (*Manager)(nil)

// NewManager 打开持久化记录；未完成的运行恢复为 Unknown，禁止盲目重启或删除。
func NewManager(ctx context.Context, path, backend string, driver Driver) (*Manager, error) {
	resourceLock, err := driver.Acquire()
	if err != nil {
		return nil, err
	}
	s, err := store.Open(path)
	if err != nil {
		return nil, errors.Join(err, resourceLock.Close())
	}
	m := &Manager{resourceLock: resourceLock, store: s, backend: backend, driver: driver, locks: make(map[string]chan struct{}), running: make(map[VMInstance]*managedRun), failedLaunch: make(map[VMInstance]bool)}
	if err = s.Bind(ctx, backend+":"+driver.Identity()); err != nil {
		return nil, errors.Join(err, s.Close(), resourceLock.Close())
	}
	records, err := s.List(ctx)
	if err != nil {
		return nil, errors.Join(err, s.Close(), resourceLock.Close())
	}
	for _, r := range records {
		if r.Backend != backend {
			return nil, errors.Join(fmt.Errorf("%w: database backend %s", ErrConflict, r.Backend), s.Close(), resourceLock.Close())
		}
		// 运行副作用可能已发生，数据库记录不能证明原进程已经退出。
		if r.State == string(StateRunning) || r.State == string(StateStarting) || r.State == string(StateStopping) {
			r.State = string(StateUnknown)
			if err = s.Update(ctx, r); err != nil {
				return nil, errors.Join(err, s.Close(), resourceLock.Close())
			}
		}
	}
	return m, nil
}

// lock 取得资源级操作权限；取消不会等待其他资源的进程操作。
func (m *Manager) lock(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	ch := m.locks[id]
	if ch == nil {
		ch = make(chan struct{}, 1)
		m.locks[id] = ch
	}
	m.mu.Unlock()
	select {
	case ch <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		<-ch
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		<-ch
		return nil, err
	}
	return func() { <-ch }, nil
}

// Create 保存创建意图后准备私有资源，重复调用核对原配置。
func (m *Manager) Create(ctx context.Context, c VMConfig) (VMRef, error) {
	if err := c.Validate(); err != nil {
		return VMRef{}, err
	}
	unlock, err := m.lock(ctx, c.ID)
	if err != nil {
		return VMRef{}, err
	}
	defer unlock()
	ref := VMRef{ID: c.ID}
	data, _ := json.Marshal(c)
	r, err := m.store.Get(ctx, c.ID)
	if errors.Is(err, sql.ErrNoRows) {
		r = store.Resource{ID: c.ID, Backend: m.backend, Config: data, State: string(StateUnknown), Operation: "create"}
		if err = m.store.Insert(ctx, r); err != nil {
			return VMRef{}, err
		}
	} else if err != nil {
		return VMRef{}, err
	} else {
		if string(r.Config) != string(data) {
			return VMRef{}, ErrConflict
		}
		if r.Operation != "create" {
			return ref, nil
		}
	}
	if err = m.driver.Prepare(ctx, c); err != nil {
		return VMRef{}, err
	}
	r.State = string(StateCreated)
	r.Operation = ""
	// 副作用已完成，使用独立 context 记录结果，保留请求取消后的真实事实。
	if err = m.store.Update(context.Background(), r); err != nil {
		return VMRef{}, err
	}
	return ref, nil
}

// Start 在分配并持久化 epoch 后启动一次 VMM，并建立唯一退出观察者。
func (m *Manager) Start(ctx context.Context, ref VMRef) (VMInstance, error) {
	if err := ref.Validate(); err != nil {
		return VMInstance{}, err
	}
	unlock, err := m.lock(ctx, ref.ID)
	if err != nil {
		return VMInstance{}, err
	}
	defer unlock()
	r, err := m.get(ctx, ref)
	if err != nil {
		return VMInstance{}, err
	}
	i := VMInstance{Ref: ref, Epoch: r.Epoch}
	if r.State == string(StateRunning) {
		return i, nil
	}
	if r.Operation != "" || r.State != string(StateCreated) && r.State != string(StateStopped) {
		return VMInstance{}, ErrInvalidState
	}
	var c VMConfig
	if err = json.Unmarshal(r.Config, &c); err != nil {
		return VMInstance{}, err
	}
	epoch, err := m.store.BeginRun(ctx, ref.ID)
	if err != nil {
		return VMInstance{}, err
	}
	i.Epoch = epoch
	process, err := m.driver.Launch(ctx, c, i)
	if err != nil {
		// Launch 失败保证进程未遗留，但没有真实进程退出结果。
		r.Epoch = epoch
		r.State = string(StateStopped)
		r.Operation = ""
		persistErr := m.store.Update(context.Background(), r)
		if persistErr != nil {
			m.mu.Lock()
			m.failedLaunch[i] = true
			m.mu.Unlock()
		}
		return VMInstance{}, errors.Join(err, persistErr)
	}
	run := &managedRun{process: process, done: make(chan struct{})}
	m.mu.Lock()
	m.running[i] = run
	m.mu.Unlock()
	r.Epoch = epoch
	r.State = string(StateRunning)
	r.Operation = ""
	err = m.store.Update(context.Background(), r)
	go m.observe(i, run)
	if err != nil {
		return VMInstance{}, err
	}
	return i, nil
}

// observe 统一回收一次运行并在持久化真实退出结果后发布通知。
func (m *Manager) observe(i VMInstance, run *managedRun) {
	exit, err := run.process.Wait()
	unlock, _ := m.lock(context.Background(), i.Ref.ID)
	if err == nil {
		data, _ := json.Marshal(exit)
		err = m.store.FinishRun(context.Background(), store.Run{ID: i.Ref.ID, Epoch: i.Epoch, Exit: data})
	}
	if err == nil {
		run.persisted.Store(true)
	}
	run.exit, run.err = exit, err
	unlock()
	close(run.done)
}

// Inspect 返回已持久化的最近观察状态及当前实例退出结果。
func (m *Manager) Inspect(ctx context.Context, ref VMRef) (VMStatus, error) {
	unlock, err := m.lock(ctx, ref.ID)
	if err != nil {
		return VMStatus{}, err
	}
	defer unlock()
	r, err := m.get(ctx, ref)
	if err != nil {
		return VMStatus{}, err
	}
	s := VMStatus{Ref: ref, State: VMState(r.State)}
	if r.Epoch != "" {
		s.Instance = VMInstance{Ref: ref, Epoch: r.Epoch}
		run, e := m.store.GetRun(ctx, ref.ID, r.Epoch)
		if e != nil {
			return VMStatus{}, e
		}
		if len(run.Exit) > 0 {
			var exit VMExit
			if err = json.Unmarshal(run.Exit, &exit); err != nil {
				return VMStatus{}, err
			}
			s.Exit = &exit
		}
	}
	return s, nil
}

// Stop 仅控制当前 epoch，调用取消不取消独立退出观察者。
func (m *Manager) Stop(ctx context.Context, i VMInstance, o StopOptions) error {
	if err := o.Validate(); err != nil {
		return err
	}
	unlock, err := m.lock(ctx, i.Ref.ID)
	if err != nil {
		return err
	}
	r, err := m.get(ctx, i.Ref)
	if err != nil {
		unlock()
		return err
	}
	if r.Epoch == "" || r.Epoch != i.Epoch {
		unlock()
		return ErrStaleInstance
	}
	if r.State == string(StateStopped) {
		unlock()
		return nil
	}
	m.mu.Lock()
	run := m.running[i]
	m.mu.Unlock()
	if run == nil {
		unlock()
		return ErrInvalidState
	}
	r.Operation = "stop"
	if err = m.store.Update(ctx, r); err != nil {
		unlock()
		return err
	}
	err = run.process.Stop(o)
	if err != nil {
		r.Operation = ""
		persistErr := m.store.Update(context.Background(), r)
		unlock()
		return errors.Join(err, persistErr)
	}
	r.State = string(StateStopping)
	err = m.store.Update(context.Background(), r)
	unlock()
	if err != nil {
		return err
	}
	select {
	case <-run.done:
		return run.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait 返回持久化的固定结果，运行无法恢复时明确报告结果不可用。
func (m *Manager) Wait(ctx context.Context, i VMInstance) (VMExit, error) {
	unlock, err := m.lock(ctx, i.Ref.ID)
	if err != nil {
		return VMExit{}, err
	}
	r, err := m.store.GetRun(ctx, i.Ref.ID, i.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		unlock()
		return VMExit{}, ErrNotFound
	}
	if err != nil {
		unlock()
		return VMExit{}, err
	}
	if len(r.Exit) > 0 {
		var e VMExit
		err = json.Unmarshal(r.Exit, &e)
		unlock()
		return e, err
	}
	m.mu.Lock()
	run := m.running[i]
	m.mu.Unlock()
	unlock()
	if run == nil {
		return VMExit{}, ErrExitUnavailable
	}
	return m.waitRun(ctx, i, run)
}

// waitRun 等待已取得的运行句柄，持久结果不依赖 Manager 继续开放。
func (m *Manager) waitRun(ctx context.Context, i VMInstance, run *managedRun) (VMExit, error) {
	select {
	case <-run.done:
		if run.persisted.Load() {
			return run.exit, nil
		}
		unlock, err := m.lock(ctx, i.Ref.ID)
		if err != nil {
			return VMExit{}, err
		}
		defer unlock()
		if err = m.persist(ctx, i, run); err != nil {
			return VMExit{}, err
		}
		return run.exit, nil
	case <-ctx.Done():
		return VMExit{}, ctx.Err()
	}
}

// Destroy 先记录删除意图，清理成功后才删除资源和全部运行历史。
func (m *Manager) Destroy(ctx context.Context, ref VMRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	unlock, err := m.lock(ctx, ref.ID)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := m.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.State != string(StateCreated) && r.State != string(StateStopped) && r.Operation != "create" && r.Operation != "destroy" {
		return ErrInvalidState
	}
	r.Operation = "destroy"
	if err = m.store.Update(ctx, r); err != nil {
		return err
	}
	if err = m.driver.Remove(ctx, ref); err != nil {
		return err
	}
	if err = m.store.Delete(context.Background(), ref.ID); err != nil {
		return err
	}
	m.mu.Lock()
	for i := range m.running {
		if i.Ref == ref {
			delete(m.running, i)
		}
	}
	m.mu.Unlock()
	return nil
}

// get 将数据库中不存在的资源转换为共享契约错误。
func (m *Manager) get(ctx context.Context, ref VMRef) (store.Resource, error) {
	r, err := m.store.Get(ctx, ref.ID)
	if err == nil && r.Epoch != "" {
		i := VMInstance{Ref: ref, Epoch: r.Epoch}
		m.mu.Lock()
		run := m.running[i]
		failed := m.failedLaunch[i]
		m.mu.Unlock()
		if failed {
			r.State = string(StateStopped)
			r.Operation = ""
			if err = m.store.Update(ctx, r); err != nil {
				return store.Resource{}, err
			}
			m.mu.Lock()
			delete(m.failedLaunch, i)
			m.mu.Unlock()
			return r, nil
		}
		if run != nil {
			select {
			case <-run.done:
				if !run.persisted.Load() {
					if err = m.persist(ctx, i, run); err != nil {
						return store.Resource{}, err
					}
					return m.store.Get(ctx, ref.ID)
				}
			default:
				if r.Operation == "start" {
					r.State = string(StateRunning)
					r.Operation = ""
					if err = m.store.Update(ctx, r); err != nil {
						return store.Resource{}, err
					}
				}
			}
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

// Close 只在没有未完成的本进程运行和操作时释放数据库；不停止 VM。
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if len(m.failedLaunch) > 0 {
		return ErrInvalidState
	}
	for _, ch := range m.locks {
		if len(ch) != 0 {
			return ErrInvalidState
		}
	}
	for _, run := range m.running {
		select {
		case <-run.done:
			if !run.persisted.Load() {
				return ErrInvalidState
			}
		default:
			return ErrInvalidState
		}
	}
	m.closed = true
	return errors.Join(m.store.Close(), m.resourceLock.Close())
}

// persist 在首次写库失败后重试提交已取得的真实结果，不重新回收进程。
func (m *Manager) persist(ctx context.Context, i VMInstance, run *managedRun) error {
	if run.persisted.Load() {
		return nil
	}
	if run.exit.Instance != i {
		return run.err
	}
	data, _ := json.Marshal(run.exit)
	if err := m.store.FinishRun(ctx, store.Run{ID: i.Ref.ID, Epoch: i.Epoch, Exit: data}); err != nil {
		return err
	}
	run.persisted.Store(true)
	return nil
}
