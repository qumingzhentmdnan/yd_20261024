// Package fakevm 在内存中模拟 VM 生命周期，不启动进程、读取镜像或建立隔离。
package fakevm

import (
	"context"
	"fmt"
	"sync"

	"safeagent/internal/host/vm"
)

type Operation string

const (
	OpCreate  Operation = "create"
	OpStart   Operation = "start"
	OpStop    Operation = "stop"
	OpDestroy Operation = "destroy"
)

// Stats 只统计实际完成的资源变更，重复调用和失败尝试不计数。
type Stats struct {
	// Created 是成功创建资源的次数。
	Created uint64
	// Started 是实际产生新运行实例的次数。
	Started uint64
	// Stopped 是通过 Stop 完成退出的次数，不含外部退出。
	Stopped uint64
	// Destroyed 是成功删除资源的次数。
	Destroyed uint64
}

type resource struct {
	// config 保留首次创建的配置，供重复创建比较。
	config vm.VMConfig
	// state 是当前生命周期状态，由 Backend.mu 保护。
	state vm.VMState
	// instance 是当前或最近一次运行，首次启动前为空。
	instance vm.VMInstance
}

type run struct {
	// done 在退出结果固定后关闭，唤醒所有等待者。
	done chan struct{}
	// exit 只写入一次，运行期间为空。
	exit *vm.VMExit
}

// Backend 通过 New 创建；epoch 在本对象内不复用。宽限期、磁盘部分写入和
// 后端重启恢复不在 fake 的模拟范围内。
type Backend struct {
	// mu 保护资源、运行记录、故障配置和计数器。
	mu sync.Mutex
	// locks 按资源标识串行化变更，不阻塞无关资源。
	locks map[string]chan struct{}
	// items 保存仍未销毁的资源。
	items map[vm.VMRef]*resource
	// runs 保存资源销毁前各次运行的退出结果。
	runs map[vm.VMInstance]*run
	// gates 为操作设置可取消的测试闸门。
	gates map[Operation]*Gate
	// failures 按注入顺序保存待触发的操作错误。
	failures map[Operation][]error
	// graceful 表示是否模拟支持优雅关机，默认开启。
	graceful bool
	// epoch 是本对象内单调递增的启动计数，销毁资源也不重置。
	epoch uint64
	// stats 统计成功完成的资源变更。
	stats Stats
}

var _ vm.VMBackend = (*Backend)(nil)

// New 创建空的内存后端，默认支持立即完成优雅关机。
func New() *Backend {
	return &Backend{
		locks: make(map[string]chan struct{}), items: make(map[vm.VMRef]*resource),
		runs: make(map[vm.VMInstance]*run), gates: make(map[Operation]*Gate),
		failures: make(map[Operation][]error), graceful: true,
	}
}

// SetGate 设置操作闸门，传入 nil 表示移除。
func (b *Backend) SetGate(op Operation, gate *Gate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gates[op] = gate
}

// FailNext 在下次实际操作前返回指定错误，已经有结果的重复调用不消耗该错误。
func (b *Backend) FailNext(op Operation, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures[op] = append(b.failures[op], err)
}

// SetGracefulShutdownSupported 设置 fake 的优雅关机能力。
func (b *Backend) SetGracefulShutdownSupported(supported bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.graceful = supported
}

// Stats 返回成功资源变更的计数快照。
func (b *Backend) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

// lock 串行化同一资源的变更，不阻塞其他资源或查询；等待锁时支持取消。
func (b *Backend) lock(ctx context.Context, ref vm.VMRef) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	sem := b.locks[ref.ID]
	if sem == nil {
		sem = make(chan struct{}, 1)
		sem <- struct{}{}
		b.locks[ref.ID] = sem
	}
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-sem:
		unlock := func() { sem <- struct{}{} }
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	}
}

// before 在副作用发生前等待闸门，并消耗下一条注入错误。
func (b *Backend) before(ctx context.Context, op Operation) error {
	b.mu.Lock()
	gate := b.gates[op]
	b.mu.Unlock()
	if gate != nil {
		if err := gate.wait(ctx); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if pending := b.failures[op]; len(pending) != 0 {
		b.failures[op] = pending[1:]
		return pending[0]
	}
	return nil
}

// Create 按配置创建内存资源，重复创建保持资源不变。
func (b *Backend) Create(ctx context.Context, config vm.VMConfig) (vm.VMRef, error) {
	if err := config.Validate(); err != nil {
		return vm.VMRef{}, err
	}
	ref := vm.VMRef{ID: config.ID}
	unlock, err := b.lock(ctx, ref)
	if err != nil {
		return vm.VMRef{}, err
	}
	defer unlock()
	// 已有结果直接复用，不消耗闸门或故障配置。
	b.mu.Lock()
	item := b.items[ref]
	if item != nil {
		same := item.config == config
		b.mu.Unlock()
		if !same {
			return vm.VMRef{}, vm.ErrConflict
		}
		return ref, nil
	}
	b.mu.Unlock()
	if err := b.before(ctx, OpCreate); err != nil {
		return vm.VMRef{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items[ref] = &resource{config: config, state: vm.StateCreated}
	b.stats.Created++
	return ref, nil
}

// Start 创建新运行实例，运行中的资源复用已有实例。
func (b *Backend) Start(ctx context.Context, ref vm.VMRef) (vm.VMInstance, error) {
	unlock, err := b.lock(ctx, ref)
	if err != nil {
		return vm.VMInstance{}, err
	}
	defer unlock()
	b.mu.Lock()
	item := b.items[ref]
	if item == nil {
		b.mu.Unlock()
		return vm.VMInstance{}, vm.ErrNotFound
	}
	if item.state == vm.StateRunning {
		instance := item.instance
		b.mu.Unlock()
		return instance, nil
	}
	if item.state != vm.StateCreated && item.state != vm.StateStopped {
		b.mu.Unlock()
		return vm.VMInstance{}, vm.ErrInvalidState
	}
	previous := item.state
	// 将启动中的状态暴露给查询，失败时恢复原状态。
	item.state = vm.StateStarting
	b.mu.Unlock()
	if err := b.before(ctx, OpStart); err != nil {
		b.mu.Lock()
		item.state = previous
		b.mu.Unlock()
		return vm.VMInstance{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.epoch++
	instance := vm.VMInstance{Ref: ref, Epoch: fmt.Sprintf("fake-%d", b.epoch)}
	item.state, item.instance = vm.StateRunning, instance
	b.runs[instance] = &run{done: make(chan struct{})}
	b.stats.Started++
	return instance, nil
}

// Inspect 返回独立状态快照，防止调用方修改缓存的退出结果。
func (b *Backend) Inspect(ctx context.Context, ref vm.VMRef) (vm.VMStatus, error) {
	if err := ctx.Err(); err != nil {
		return vm.VMStatus{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.items[ref]
	if item == nil {
		return vm.VMStatus{}, vm.ErrNotFound
	}
	status := vm.VMStatus{Ref: ref, State: item.state, Instance: item.instance}
	if r := b.runs[item.instance]; r != nil && r.exit != nil {
		value := *r.exit
		status.Exit = &value
	}
	return status, nil
}

// Stop 校验 epoch 和关机能力，完成退出后通知等待者。
func (b *Backend) Stop(ctx context.Context, instance vm.VMInstance, options vm.StopOptions) error {
	if err := options.Validate(); err != nil {
		return err
	}
	unlock, err := b.lock(ctx, instance.Ref)
	if err != nil {
		return err
	}
	defer unlock()
	b.mu.Lock()
	item := b.items[instance.Ref]
	if item == nil {
		b.mu.Unlock()
		return vm.ErrNotFound
	}
	if item.instance != instance {
		b.mu.Unlock()
		return vm.ErrStaleInstance
	}
	r := b.runs[instance]
	if r.exit != nil {
		b.mu.Unlock()
		return nil
	}
	graceful := b.graceful
	if !graceful && !options.ForceAfterTimeout {
		b.mu.Unlock()
		return vm.ErrShutdownUnsupported
	}
	item.state = vm.StateStopping
	// 等待闸门时释放全局锁，允许查询和外部退出继续进行。
	b.mu.Unlock()
	err = b.before(ctx, OpStop)
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.exit != nil { // 等待停止期间，VMM 也可能已经自行退出。
		return nil
	}
	if err != nil {
		item.state = vm.StateRunning
		return err
	}
	exit := vm.VMExit{Instance: instance, Reason: vm.ExitStopped}
	if !graceful {
		exit.Code, exit.Signal, exit.Reason = -1, "SIGKILL", vm.ExitForced
	}
	b.finish(item, r, exit)
	b.stats.Stopped++
	return nil
}

// finish 在持有 mu 时调用，先固定退出结果，再通知所有等待者。
func (b *Backend) finish(item *resource, r *run, exit vm.VMExit) {
	r.exit = &exit
	item.state = vm.StateStopped
	close(r.done)
}

// Exit 模拟 VMM 自行退出，可在 Stop 阻塞时触发；相同结果重复调用成功。
func (b *Backend) Exit(instance vm.VMInstance, code int, signal string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.items[instance.Ref]
	if item == nil {
		return vm.ErrNotFound
	}
	if item.instance != instance {
		return vm.ErrStaleInstance
	}
	r := b.runs[instance]
	exit := vm.VMExit{Instance: instance, Code: code, Signal: signal, Reason: vm.ExitProcess}
	if r.exit != nil {
		if *r.exit != exit {
			return vm.ErrConflict
		}
		return nil
	}
	b.finish(item, r, exit)
	return nil
}

// Wait 等待指定运行的固定结果，取消只影响当前调用。
func (b *Backend) Wait(ctx context.Context, instance vm.VMInstance) (vm.VMExit, error) {
	if err := ctx.Err(); err != nil {
		return vm.VMExit{}, err
	}
	b.mu.Lock()
	r := b.runs[instance]
	b.mu.Unlock()
	if r == nil {
		return vm.VMExit{}, vm.ErrNotFound
	}
	select {
	case <-ctx.Done():
		return vm.VMExit{}, ctx.Err()
	case <-r.done:
		return *r.exit, nil
	}
}

// Destroy 删除尚未启动或已停止的内存资源和历史运行，失败时保留记录供重试。
func (b *Backend) Destroy(ctx context.Context, ref vm.VMRef) error {
	unlock, err := b.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	b.mu.Lock()
	item := b.items[ref]
	if item == nil {
		b.mu.Unlock()
		return nil
	}
	if item.state != vm.StateCreated && item.state != vm.StateStopped {
		b.mu.Unlock()
		return vm.ErrInvalidState
	}
	b.mu.Unlock()
	if err := b.before(ctx, OpDestroy); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.items, ref)
	// 已开始等待的调用仍持有运行记录，其结果不受索引释放影响。
	for instance := range b.runs {
		if instance.Ref == ref {
			delete(b.runs, instance)
		}
	}
	b.stats.Destroyed++
	return nil
}
