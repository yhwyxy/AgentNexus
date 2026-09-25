// Package hostaccess 实现架构设计 §7.3 的 HostAccessPolicy:把 Server 请求的
// 宿主机挂载与环境变量限制在运维显式声明的允许清单内。
//
// 它是纵深防御,不是对抗恶意 Docker daemon 的安全边界:容器实际挂载由远端 daemon
// 决定,本地只能做路径层面的判定。AgentNexus 自身运行在容器里时,宿主机路径在本
// 容器内可能不存在,此时只做词法判定,真正的失败由 daemon 报告。
package hostaccess

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yhwyxy/AgentNexus/internal/server"
)

// Entry 是一条允许的挂载映射:宿主机根 -> 容器根,以及是否允许读写。
// 来自配置 security.hostAccess.mounts。
type Entry struct {
	Host       string
	Container  string
	AllowWrite bool
}

// 固定拒绝清单:即使命中允许清单也不允许挂载的宿主机路径(前缀判定)。
// "/" 不在此列 —— 能力上已由 NewPolicy 拒绝把根目录写进允许清单来封堵。
var deniedRoots = []string{
	"/boot",
	"/dev",
	"/etc",
	"/proc",
	"/root",
	"/sys",
	"/usr",
	"/var/lib/docker",
	"/var/run/docker.sock",
}

// deniedSuffixes 覆盖运行时才知道的 socket(SSH_AUTH_SOCK、docker.sock)。
var deniedSuffixes = []string{
	"docker.sock",
	"ssh-agent",
	"ssh_auth_sock",
}

// 形如 AWS_SECRET_ACCESS_KEY / GITHUB_TOKEN / MYSQL_PASSWORD 的环境变量名:
// Secret 必须走 CredentialID,runtime_spec_json 只存配置。
var secretEnvPattern = regexp.MustCompile(`(?i)(^|_)(API_?KEY|AUTH|CREDENTIALS?|PASSWD|PASSWORD|PRIVATE_?KEY|SECRET|TOKEN)($|_)`)

type resolvedEntry struct {
	host       string // filepath.Clean 后的绝对路径
	hostReal   string // EvalSymlinks 结果;路径不存在时为空
	container  string
	allowWrite bool
}

// Policy 是编译后的允许清单。构造后只读,可被并发调用。
type Policy struct {
	entries []resolvedEntry
	denied  []string
}

// NewPolicy 校验并编译允许清单。socketPaths 是需要额外拒绝的宿主机路径
// (通常是 runtime.docker.host 与 DOCKER_HOST 指向的 docker socket)。
func NewPolicy(entries []Entry, socketPaths ...string) (*Policy, error) {
	policy := &Policy{entries: make([]resolvedEntry, 0, len(entries))}
	seen := make(map[string]struct{}, len(entries))

	for i, entry := range entries {
		host := filepath.Clean(entry.Host)
		container := filepath.Clean(entry.Container)

		switch {
		case entry.Host == "" || !filepath.IsAbs(entry.Host):
			return nil, fmt.Errorf("security.hostAccess.mounts[%d].host must be an absolute path", i)
		case host == string(filepath.Separator):
			return nil, fmt.Errorf("security.hostAccess.mounts[%d].host must not be the host root", i)
		case entry.Container == "" || !filepath.IsAbs(entry.Container):
			return nil, fmt.Errorf("security.hostAccess.mounts[%d].container must be an absolute path", i)
		case container == string(filepath.Separator):
			return nil, fmt.Errorf("security.hostAccess.mounts[%d].container must not be the container root", i)
		}
		if _, exists := seen[host]; exists {
			return nil, fmt.Errorf("security.hostAccess.mounts[%d].host %q is declared twice", i, host)
		}
		seen[host] = struct{}{}

		resolved := resolvedEntry{host: host, container: container, allowWrite: entry.AllowWrite}
		// 允许清单本身可能经过符号链接(如 macOS 的 /tmp -> /private/tmp),
		// 解析成功时把真实路径也作为匹配依据。
		if real, err := filepath.EvalSymlinks(host); err == nil {
			resolved.hostReal = real
		}
		policy.entries = append(policy.entries, resolved)
	}

	policy.denied = append(policy.denied, deniedRoots...)
	policy.denied = append(policy.denied, socketPaths...)
	if agentSocket := os.Getenv("SSH_AUTH_SOCK"); agentSocket != "" {
		policy.denied = append(policy.denied, agentSocket)
	}

	return policy, nil
}

// CheckDocker 判定一次 Docker 运行时请求是否被允许:挂载必须命中允许清单
// (来源在宿主机根内、落地在同一 Entry 的容器根内、只读性不越权),环境变量
// 不得承载 Secret。形状与资源区间由 server.Validate 负责。
func (p *Policy) CheckDocker(spec server.DockerSpec) error {
	for _, mount := range spec.Mounts {
		if err := p.checkMount(mount); err != nil {
			return err
		}
	}
	for key := range spec.Env {
		if secretEnvPattern.MatchString(key) {
			return fmt.Errorf("docker env key %q looks like a secret: use credentialId instead", key)
		}
	}
	return nil
}

func (p *Policy) checkMount(mount server.Mount) error {
	source := filepath.Clean(mount.Source)
	target := filepath.Clean(mount.Target)
	if err := p.checkSource(source); err != nil {
		return err
	}

	entry, ok := p.matchSource(source)
	if !ok {
		return fmt.Errorf("docker mount source %q is outside the allowed host roots", source)
	}
	if !within(entry.container, target) {
		return fmt.Errorf("docker mount target %q is outside the allowed container root %q", target, entry.container)
	}
	if !mount.ReadOnly && !entry.allowWrite {
		return fmt.Errorf("docker mount source %q is allowed read-only only", source)
	}
	return nil
}

// checkSource 对来源路径做词法判定,并在路径存在时对符号链接解析结果再判一次。
func (p *Policy) checkSource(source string) error {
	if p.isDenied(source) {
		return fmt.Errorf("docker mount source %q is denied by host access policy", source)
	}
	real, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil
	}
	if p.isDenied(real) {
		return fmt.Errorf("docker mount source %q is denied by host access policy", source)
	}
	// 解析后的真实路径也必须落在允许清单内,避免"许可目录内的软链指向 /etc"。
	if _, ok := p.matchSource(real); !ok {
		return fmt.Errorf("docker mount source %q resolves outside the allowed host roots", source)
	}
	return nil
}

func (p *Policy) isDenied(path string) bool {
	for _, deny := range p.denied {
		deny = filepath.Clean(deny)
		if within(deny, path) {
			return true
		}
	}
	for _, suffix := range deniedSuffixes {
		if strings.HasSuffix(strings.ToLower(path), suffix) {
			return true
		}
	}
	return false
}

func (p *Policy) matchSource(path string) (resolvedEntry, bool) {
	for _, entry := range p.entries {
		if within(entry.host, path) {
			return entry, true
		}
		if entry.hostReal != "" && within(entry.hostReal, path) {
			return entry, true
		}
	}
	return resolvedEntry{}, false
}

// within 做路径边界判定:root 自身或 root 的子路径,不允许 "/data" 匹配 "/database"。
func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
