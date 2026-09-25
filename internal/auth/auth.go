// 认证与授权的领域模型：不依赖 net/http、SQL 或 MCP SDK。
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
)

// Role 是 v0.1 的授权粒度（整体架构设计 §8.2：API Key + Server 启用/禁用）。
type Role string

const (
	// RoleAdmin 可调用管理 API 与 /mcp。
	RoleAdmin Role = "admin"
	// RoleAgent 只能调用 /mcp 数据面。
	RoleAgent Role = "agent"
)

func (r Role) valid() bool {
	return r == RoleAdmin || r == RoleAgent
}

// 认证与授权失败的哨兵错误：HTTP 层据此映射 401/403。
var (
	ErrUnauthenticated  = errors.New("unauthenticated")
	ErrPermissionDenied = errors.New("permission denied")
)

// Principal 是通过认证的调用者。Name 非空（构造 Authorizer 时保证），
// 因此它可以安全地出现在日志与后续的审计事件里。
type Principal struct {
	Name string
	Role Role
}

// KeyConfig 是一条已解析的密钥配置：配置层负责 keyEnv 解引用，Secret 在此已是明文。
type KeyConfig struct {
	Name   string
	Role   Role
	Secret string
}

type entry struct {
	name   string
	role   Role
	secret []byte
}

// Authorizer 用静态密钥表做认证、用 Policy 做授权。构造后不可变，可并发使用。
type Authorizer struct {
	entries []entry
	policy  Policy
}

// NewAuthorizer 校验并冻结密钥表与策略。空密钥表合法：它表达"没有任何主体被授权"，
// 于是除公开路径外全部请求都会被判为未认证。
func NewAuthorizer(keys []KeyConfig, policy Policy) (*Authorizer, error) {
	entries := make([]entry, 0, len(keys))
	names := make(map[string]struct{}, len(keys))
	for i, key := range keys {
		if key.Name == "" {
			return nil, fmt.Errorf("api key %d: name must not be empty", i)
		}
		if !key.Role.valid() {
			return nil, fmt.Errorf("api key %q: unsupported role %q", key.Name, key.Role)
		}
		if key.Secret == "" {
			return nil, fmt.Errorf("api key %q: secret must not be empty", key.Name)
		}
		if _, exists := names[key.Name]; exists {
			return nil, fmt.Errorf("api key name %q is duplicated", key.Name)
		}
		names[key.Name] = struct{}{}
		entries = append(entries, entry{name: key.Name, role: key.Role, secret: []byte(key.Secret)})
	}
	for i := range entries {
		for j := i + 1; j < len(entries); j++ {
			if subtle.ConstantTimeCompare(entries[i].secret, entries[j].secret) == 1 {
				return nil, fmt.Errorf("api keys %q and %q share the same secret", entries[i].name, entries[j].name)
			}
		}
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	return &Authorizer{entries: entries, policy: policy}, nil
}

// Authorize 是唯一的认证/授权判定点。公开路径返回 (nil, nil) 表示放行且无身份；
// 其余情况要么返回身份，要么返回 ErrUnauthenticated / ErrPermissionDenied。
func (a *Authorizer) Authorize(path, presented string) (*Principal, error) {
	if a.policy.isPublic(path) {
		return nil, nil
	}
	if presented == "" {
		return nil, ErrUnauthenticated
	}
	presentedBytes := []byte(presented)
	// 常量时间比对，且不提前返回：短路会泄漏"前缀命中"的信息。
	// 长度不同会立即返回 0（只泄漏长度），密钥长度不是秘密。
	matched := -1
	for i := range a.entries {
		if subtle.ConstantTimeCompare(a.entries[i].secret, presentedBytes) == 1 && matched < 0 {
			matched = i
		}
	}
	if matched < 0 {
		return nil, ErrUnauthenticated
	}
	hit := a.entries[matched]
	if !a.policy.allows(path, hit.role) {
		return nil, ErrPermissionDenied
	}
	return &Principal{Name: hit.name, Role: hit.role}, nil
}

type principalContextKey struct{}

// WithPrincipal 把调用者写入请求上下文：这是后续 audit/metrics 读取调用者的唯一 seam。
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFrom 取出上下文里的调用者；公开路径（无身份）返回 false。
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
