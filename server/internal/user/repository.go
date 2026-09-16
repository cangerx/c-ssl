package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	driver "github.com/go-sql-driver/mysql"
)

// ErrEmailTaken 表示邮箱已被注册。
var ErrEmailTaken = errors.New("邮箱已被注册")

// mysqlErrDuplicateEntry 是 MySQL 唯一键冲突的错误号。
const mysqlErrDuplicateEntry = 1062

// Repository 负责用户与会话的数据访问。
type Repository struct {
	db *sql.DB
}

// NewRepository 构造仓储。
func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const userColumns = `
	id, email, password_hash, nickname, status,
	failed_login_count, locked_until, last_login_at,
	created_at, updated_at`

// ── 用户 ──────────────────────────────────────────

// CreateUser 插入新用户。邮箱重复时返回 ErrEmailTaken。
func (r *Repository) CreateUser(ctx context.Context, email, passwordHash, nickname string) (*User, error) {
	result, err := r.db.ExecContext(ctx,
		"INSERT INTO users (email, password_hash, nickname) VALUES (?, ?, ?)",
		email, passwordHash, nickname,
	)
	if err != nil {
		// 依赖唯一索引而不是"先查再插"：后者在并发下必然漏判
		var mysqlErr *driver.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlErrDuplicateEntry {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("创建用户失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("读取新用户 ID 失败: %w", err)
	}

	u, err := r.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("用户 %d 插入后查询不到", id)
	}
	return u, nil
}

// GetByID 按 ID 查询用户。不存在返回 (nil, nil)。
func (r *Repository) GetByID(ctx context.Context, id int64) (*User, error) {
	query := "SELECT " + userColumns + " FROM users WHERE id = ?"
	return r.queryOne(ctx, query, id)
}

// GetByEmail 按邮箱查询用户。不存在返回 (nil, nil)。
//
// 邮箱比较依赖列的 ai_ci 排序规则，天然不区分大小写，
// 因此 Abc@x.com 与 abc@x.com 会命中同一条记录。
func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	query := "SELECT " + userColumns + " FROM users WHERE email = ?"
	return r.queryOne(ctx, query, email)
}

func (r *Repository) queryOne(ctx context.Context, query string, args ...any) (*User, error) {
	var (
		u           User
		status      string
		lockedUntil sql.NullTime
		lastLoginAt sql.NullTime
	)

	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.Nickname, &status,
		&u.FailedLoginCount, &lockedUntil, &lastLoginAt,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}

	u.Status = Status(status)
	if lockedUntil.Valid {
		u.LockedUntil = &lockedUntil.Time
	}
	if lastLoginAt.Valid {
		u.LastLoginAt = &lastLoginAt.Time
	}
	return &u, nil
}

// RecordLoginSuccess 记录一次成功登录：失败计数归零、解除锁定、更新登录时间。
func (r *Repository) RecordLoginSuccess(ctx context.Context, userID int64, now time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users
		 SET failed_login_count = 0, locked_until = NULL, last_login_at = ?
		 WHERE id = ?`,
		now, userID,
	)
	if err != nil {
		return fmt.Errorf("更新登录成功状态失败: %w", err)
	}
	return nil
}

// RecordLoginFailure 累加失败次数，达到阈值时锁定账号。
//
// 累加与判断放在同一条 UPDATE 里，避免"读-判断-写"之间被并发插入
// 导致计数丢失——暴力破解正是高并发场景。
//
// 注意 SET 子句的求值顺序：MySQL 从左到右求值，且前面赋值的结果对
// 后面的表达式可见。这里刻意把 locked_until 放在前面，此时
// failed_login_count 还是自增前的值，用 +1 预测"本次失败后"的次数。
// 若把两行调换，判断就会变成基于自增后的值，导致提前一次锁定。
func (r *Repository) RecordLoginFailure(ctx context.Context, userID int64, now time.Time) error {
	lockUntil := now.Add(LockDuration)

	_, err := r.db.ExecContext(ctx,
		`UPDATE users
		 SET locked_until = IF(failed_login_count + 1 >= ?, ?, locked_until),
		     failed_login_count = failed_login_count + 1
		 WHERE id = ?`,
		MaxFailedLogins, lockUntil, userID,
	)
	if err != nil {
		return fmt.Errorf("更新登录失败计数失败: %w", err)
	}
	return nil
}

// UpdatePasswordHash 更新口令哈希。
func (r *Repository) UpdatePasswordHash(ctx context.Context, userID int64, hash string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE users SET password_hash = ? WHERE id = ?", hash, userID)
	if err != nil {
		return fmt.Errorf("更新口令失败: %w", err)
	}
	return nil
}

// ── 会话 ──────────────────────────────────────────

// CreateSession 写入一条新会话。
func (r *Repository) CreateSession(ctx context.Context, s Session) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO user_sessions
		   (user_id, session_id, secret_hash, device_label, client_ip, issued_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.UserID, s.SessionID, s.SecretHash, s.DeviceLabel, s.ClientIP, s.IssuedAt, s.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("创建会话失败: %w", err)
	}
	return nil
}

// GetSession 按公开会话标识查询会话。不存在返回 (nil, nil)。
func (r *Repository) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	var (
		s          Session
		lastUsedAt sql.NullTime
		revokedAt  sql.NullTime
	)

	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, session_id, secret_hash, device_label, client_ip,
		        issued_at, expires_at, last_used_at, revoked_at, revoked_reason
		 FROM user_sessions WHERE session_id = ?`,
		sessionID,
	).Scan(
		&s.ID, &s.UserID, &s.SessionID, &s.SecretHash, &s.DeviceLabel, &s.ClientIP,
		&s.IssuedAt, &s.ExpiresAt, &lastUsedAt, &revokedAt, &s.RevokedReason,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询会话失败: %w", err)
	}

	if lastUsedAt.Valid {
		s.LastUsedAt = &lastUsedAt.Time
	}
	if revokedAt.Valid {
		s.RevokedAt = &revokedAt.Time
	}
	return &s, nil
}

// TouchSession 记录会话最近一次使用时间。
func (r *Repository) TouchSession(ctx context.Context, sessionID string, now time.Time) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE user_sessions SET last_used_at = ? WHERE session_id = ?", now, sessionID)
	if err != nil {
		return fmt.Errorf("更新会话使用时间失败: %w", err)
	}
	return nil
}

// RevokeSession 撤销单条会话。已撤销的会话不会被重复覆盖，
// 保留首次撤销的原因与时间，便于审计追溯。
func (r *Repository) RevokeSession(ctx context.Context, sessionID, reason string, now time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE user_sessions
		 SET revoked_at = ?, revoked_reason = ?
		 WHERE session_id = ? AND revoked_at IS NULL`,
		now, reason, sessionID,
	)
	if err != nil {
		return fmt.Errorf("撤销会话失败: %w", err)
	}
	return nil
}

// RevokeAllSessions 撤销某用户的全部有效会话。
//
// 用于改密与管理员封禁：必须让所有已发出的刷新令牌立即失效，
// 否则攻击者手里的令牌还能继续换取访问令牌。
func (r *Repository) RevokeAllSessions(ctx context.Context, userID int64, reason string, now time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE user_sessions
		 SET revoked_at = ?, revoked_reason = ?
		 WHERE user_id = ? AND revoked_at IS NULL`,
		now, reason, userID,
	)
	if err != nil {
		return fmt.Errorf("撤销用户全部会话失败: %w", err)
	}
	return nil
}

// ErrSessionRotated 表示会话已被并发刷新轮换掉。
//
// 同一刷新令牌被提交两次时会走到这里（例如浏览器开了两个标签页同时刷新）。
// 这与"令牌被窃取后重放"是两回事，调用方需要区分处理。
var ErrSessionRotated = errors.New("会话已被轮换")

// RotateSession 在单个事务内撤销旧会话并写入新会话。
//
// 必须原子：如果先撤销再插入，插入失败会让用户无端掉线；
// 先插入再撤销，中间态会出现两个同时有效的会话，轮换检测就失效了。
//
// UPDATE 带 `revoked_at IS NULL` 条件，保证并发刷新时只有一方成功，
// 另一方拿到 ErrSessionRotated。
func (r *Repository) RotateSession(ctx context.Context, oldSessionID string, next Session, reason string, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启轮换事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`UPDATE user_sessions
		 SET revoked_at = ?, revoked_reason = ?
		 WHERE session_id = ? AND revoked_at IS NULL`,
		now, reason, oldSessionID,
	)
	if err != nil {
		return fmt.Errorf("撤销旧会话失败: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取撤销结果失败: %w", err)
	}
	if affected == 0 {
		return ErrSessionRotated
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_sessions
		   (user_id, session_id, secret_hash, device_label, client_ip, issued_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		next.UserID, next.SessionID, next.SecretHash, next.DeviceLabel,
		next.ClientIP, next.IssuedAt, next.ExpiresAt,
	); err != nil {
		return fmt.Errorf("写入新会话失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交轮换事务失败: %w", err)
	}
	return nil
}

// CountActiveSessions 统计用户当前有效会话数。
func (r *Repository) CountActiveSessions(ctx context.Context, userID int64, now time.Time) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_sessions
		 WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ?`,
		userID, now,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("统计有效会话失败: %w", err)
	}
	return count, nil
}

// DeleteExpiredSessions 清理已过期且超过保留期的会话，供 cron 调用。
//
// 保留一段时间再删是为了让审计日志还能对上会话记录。
func (r *Repository) DeleteExpiredSessions(ctx context.Context, before time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM user_sessions WHERE expires_at < ?", before)
	if err != nil {
		return 0, fmt.Errorf("清理过期会话失败: %w", err)
	}
	return result.RowsAffected()
}
