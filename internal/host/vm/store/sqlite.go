// Package store 保存 VM 资源与运行记录，不执行宿主资源操作。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	_ "modernc.org/sqlite"
)

// Resource 保存与后端无关的持久资源元数据。
type Resource struct {
	// ID 是稳定资源标识。
	ID string
	// Backend 是负责资源副作用的后端名称。
	Backend string
	// Config 是冻结的配置 JSON。
	Config []byte
	// State 是最近确认的生命周期状态。
	State string
	// Epoch 是当前或最近一次运行，首次启动前为空。
	Epoch string
	// Operation 是尚未确认完成的操作，空值表示无未决操作。
	Operation string
}

// Run 保存一次运行及其可空的真实退出结果。
type Run struct {
	// ID 是资源标识。
	ID string
	// Epoch 是不可复用的运行标识。
	Epoch string
	// Exit 是退出结果 JSON；空值表示结果未取得。
	Exit []byte
}

// Store 持有 SQLite 连接和覆盖整个生命周期的独占文件锁。
type Store struct {
	// db 是只使用单个连接的 SQLite 数据库。
	db *sql.DB
	// lock 是阻止第二个生命周期写者的文件锁。
	lock *os.File
}

// Open 打开数据库并取得同一数据库的独占写者锁。
func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("vm store: database path must be absolute")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := lock.Stat()
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	if !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, errors.Join(errors.New("vm store: database aliases are unsupported"), lock.Close())
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
 CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS vms(id TEXT PRIMARY KEY, backend TEXT NOT NULL, config BLOB NOT NULL, state TEXT NOT NULL, epoch TEXT NOT NULL, operation TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS epochs(id INTEGER PRIMARY KEY AUTOINCREMENT);
 CREATE TABLE IF NOT EXISTS runs(vm_id TEXT NOT NULL REFERENCES vms(id) ON DELETE CASCADE, epoch TEXT NOT NULL, exit BLOB, PRIMARY KEY(vm_id,epoch));`)
	if err != nil {
		return nil, errors.Join(err, db.Close(), lock.Close())
	}
	return &Store{db: db, lock: lock}, nil
}

// Close 关闭数据库并释放独占写者锁。
func (s *Store) Close() error { return errors.Join(s.db.Close(), s.lock.Close()) }

// List 返回启动恢复需要的全部资源记录。
func (s *Store) List(ctx context.Context) ([]Resource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,backend,config,state,epoch,operation FROM vms`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Resource
	for rows.Next() {
		var r Resource
		if err = rows.Scan(&r.ID, &r.Backend, &r.Config, &r.State, &r.Epoch, &r.Operation); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// Get 返回资源记录；不存在时返回 sql.ErrNoRows。
func (s *Store) Get(ctx context.Context, id string) (Resource, error) {
	var r Resource
	err := s.db.QueryRowContext(ctx, `SELECT id,backend,config,state,epoch,operation FROM vms WHERE id=?`, id).Scan(&r.ID, &r.Backend, &r.Config, &r.State, &r.Epoch, &r.Operation)
	return r, err
}

// Insert 在副作用发生前记录创建意图。
func (s *Store) Insert(ctx context.Context, r Resource) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO vms VALUES(?,?,?,?,?,?)`, r.ID, r.Backend, r.Config, r.State, r.Epoch, r.Operation)
	return err
}

// Update 更新已有资源，不重新创建已删除的记录。
func (s *Store) Update(ctx context.Context, r Resource) error {
	_, err := s.db.ExecContext(ctx, `UPDATE vms SET state=?,epoch=?,operation=? WHERE id=?`, r.State, r.Epoch, r.Operation, r.ID)
	return err
}

// BeginRun 在启动副作用前原子分配 epoch 并保存运行和启动意图。
func (s *Store) BeginRun(ctx context.Context, id string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO epochs DEFAULT VALUES`)
	if err != nil {
		return "", err
	}
	n, _ := result.LastInsertId()
	epoch := strconv.FormatInt(n, 10)
	if _, err = tx.ExecContext(ctx, `INSERT INTO runs(vm_id,epoch) VALUES(?,?)`, id, epoch); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE vms SET epoch=?,state='starting',operation='start' WHERE id=?`, epoch, id); err != nil {
		return "", err
	}
	// 计数器保留在 sqlite_sequence 中，避免销毁 VM 后复用 epoch。
	if _, err = tx.ExecContext(ctx, `DELETE FROM epochs`); err != nil {
		return "", err
	}
	return epoch, tx.Commit()
}

// FinishRun 原子保存固定退出结果和当前实例的停止状态。
func (s *Store) FinishRun(ctx context.Context, r Run) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE runs SET exit=? WHERE vm_id=? AND epoch=? AND exit IS NULL`, r.Exit, r.ID, r.Epoch); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE vms SET state='stopped',operation='' WHERE id=? AND epoch=?`, r.ID, r.Epoch); err != nil {
		return err
	}
	return tx.Commit()
}

// GetRun 返回历史运行；Exit 为空时调用者不能编造退出结果。
func (s *Store) GetRun(ctx context.Context, id, epoch string) (Run, error) {
	r := Run{ID: id, Epoch: epoch}
	err := s.db.QueryRowContext(ctx, `SELECT exit FROM runs WHERE vm_id=? AND epoch=?`, id, epoch).Scan(&r.Exit)
	return r, err
}

// Delete 在私有资源清理成功后原子删除资源及其运行历史。
func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM vms WHERE id=?`, id)
	return err
}

// Bind 验证数据库不可变的后端资源根目录绑定。
func (s *Store) Bind(ctx context.Context, identity string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO metadata VALUES('resource_identity',?)`, identity); err != nil {
		return err
	}
	var actual string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='resource_identity'`).Scan(&actual); err != nil {
		return err
	}
	if actual != identity {
		return fmt.Errorf("vm store: resource identity mismatch: %s", actual)
	}
	return nil
}
