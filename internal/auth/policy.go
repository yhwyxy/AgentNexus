package auth

import (
	"fmt"
	"strings"
)

// Rule 是某个路径前缀下允许的角色集合。
type Rule struct {
	Prefix string
	Roles  []Role
}

// Policy 描述"哪些路径无需凭证"与"哪些前缀需要哪些角色"。
// 未命中任何规则的路径按默认拒绝处理（返回 ErrPermissionDenied），
// 因此新增路由若忘记登记策略会立刻表现为 403，而不是静默放行。
type Policy struct {
	Public []string
	Rules  []Rule
}

// DefaultPolicy 是 v0.1 的线上策略（详细设计 §13 的路由表）：
// 探针公开，管理 API 仅 admin，/mcp 数据面 admin 与 agent 都可调用，
// /metrics 仅 admin（label 含资产名，属运维面信息）。
func DefaultPolicy() Policy {
	return Policy{
		Public: []string{"/health/live", "/health/ready"},
		Rules: []Rule{
			{Prefix: "/api/v1/", Roles: []Role{RoleAdmin}},
			{Prefix: "/mcp", Roles: []Role{RoleAgent, RoleAdmin}},
			{Prefix: "/metrics", Roles: []Role{RoleAdmin}},
		},
	}
}

// isPublic 只做精确匹配：不存在"整个 /health/ 前缀公开"这种隐式扩张。
func (p Policy) isPublic(path string) bool {
	for _, candidate := range p.Public {
		if candidate == path {
			return true
		}
	}
	return false
}

// allows 取最长匹配前缀的规则，再判断角色是否在允许集合内。
func (p Policy) allows(path string, role Role) bool {
	found := -1
	for i := range p.Rules {
		if !strings.HasPrefix(path, p.Rules[i].Prefix) {
			continue
		}
		if found < 0 || len(p.Rules[i].Prefix) > len(p.Rules[found].Prefix) {
			found = i
		}
	}
	if found < 0 {
		return false
	}
	for _, allowed := range p.Rules[found].Roles {
		if allowed == role {
			return true
		}
	}
	return false
}

func (p Policy) validate() error {
	for _, path := range p.Public {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("public path %q must be an absolute path", path)
		}
	}
	for _, rule := range p.Rules {
		if !strings.HasPrefix(rule.Prefix, "/") {
			return fmt.Errorf("policy rule prefix %q must be an absolute path", rule.Prefix)
		}
		if len(rule.Roles) == 0 {
			return fmt.Errorf("policy rule %q must allow at least one role", rule.Prefix)
		}
		for _, role := range rule.Roles {
			if !role.valid() {
				return fmt.Errorf("policy rule %q: unsupported role %q", rule.Prefix, role)
			}
		}
	}
	return nil
}
