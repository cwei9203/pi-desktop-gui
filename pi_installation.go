package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const supportedPiVersion = "1.4.0"

type PiInstallation struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
}

func (d *Desktop) DetectPi() PiInstallation {
	d.mu.Lock()
	saved := d.data.PiPath
	d.mu.Unlock()
	home, _ := os.UserHomeDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return findPi(ctx, piCandidates(saved, home, os.Environ()))
}

func piCandidates(saved, home string, environ []string) []string {
	env := make(map[string]string)
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	var candidates []string
	seen := make(map[string]bool)
	add := func(path string) {
		if path == "" || !filepath.IsAbs(path) || seen[path] || len(candidates) >= 128 {
			return
		}
		seen[path] = true
		candidates = append(candidates, path)
	}
	add(saved)
	for _, directory := range filepath.SplitList(env["PATH"]) {
		if filepath.IsAbs(directory) {
			add(filepath.Join(directory, "pi"))
		}
	}
	for _, path := range []string{"/opt/homebrew/bin/pi", "/usr/local/bin/pi", "/usr/bin/pi"} {
		add(path)
	}
	for _, key := range []string{"NPM_CONFIG_PREFIX", "npm_config_prefix", "BUN_INSTALL", "VOLTA_HOME", "N_PREFIX", "FNM_MULTISHELL_PATH"} {
		if env[key] != "" {
			add(filepath.Join(env[key], "bin/pi"))
		}
	}
	if env["PNPM_HOME"] != "" {
		add(filepath.Join(env["PNPM_HOME"], "pi"))
	}
	if home != "" {
		for _, path := range []string{
			".local/bin/pi", ".pi/agent/bin/pi", ".npm-global/bin/pi", ".npm/bin/pi",
			".bun/bin/pi", ".volta/bin/pi", ".n/bin/pi", ".asdf/shims/pi",
			".local/share/mise/shims/pi", ".local/share/pnpm/pi", "Library/pnpm/pi",
		} {
			add(filepath.Join(home, path))
		}
	}
	roots := []struct{ key, fallback, pattern string }{
		{"NVM_DIR", ".nvm", "versions/node/*/bin/pi"},
		{"FNM_DIR", ".local/share/fnm", "node-versions/*/installation/bin/pi"},
		{"", "Library/Application Support/fnm", "node-versions/*/installation/bin/pi"},
		{"ASDF_DATA_DIR", ".asdf", "installs/nodejs/*/bin/pi"},
		{"MISE_DATA_DIR", ".local/share/mise", "installs/node/*/bin/pi"},
		{"NODENV_ROOT", ".nodenv", "versions/*/bin/pi"},
	}
	for _, root := range roots {
		directory := env[root.key]
		if directory == "" && home != "" {
			directory = filepath.Join(home, root.fallback)
		}
		if !filepath.IsAbs(directory) {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(directory, root.pattern))
		for i := len(matches) - 1; i >= 0; i-- {
			add(matches[i])
		}
	}
	return candidates
}

func findPi(ctx context.Context, candidates []string) PiInstallation {
	result := PiInstallation{Status: "missing", Detail: "未找到 Pi。请安装 Pi 1.4.0，或在高级设置中指定已有的可执行文件。"}
	for _, candidate := range candidates {
		installation := probePi(ctx, candidate)
		if installation.Status == "ready" {
			return installation
		}
		if installation.Status == "unsupported" || (result.Status == "missing" && installation.Status == "unavailable") {
			result = installation
		}
		if ctx.Err() != nil {
			break
		}
	}
	return result
}

var piVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$`)

func probePi(ctx context.Context, path string) PiInstallation {
	result := PiInstallation{Path: path, Status: "unavailable"}
	if !filepath.IsAbs(path) {
		resolved, err := exec.LookPath(path)
		if err != nil || !filepath.IsAbs(resolved) {
			result.Detail = "找不到 Pi，请指定可执行文件的绝对路径。"
			return result
		}
		path, result.Path = resolved, resolved
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			result.Status = "missing"
			result.Detail = "Pi 路径已失效，请重新检测或在高级设置中修改路径。"
		} else {
			result.Detail = fmt.Sprintf("无法访问 Pi 可执行文件：%v", err)
		}
		return result
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		result.Detail = "找到 Pi 路径，但它不是可执行文件。请检查文件权限或重新安装。"
		return result
	}
	probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(probeCtx, path, "--version")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	command.WaitDelay = 250 * time.Millisecond
	var stdout, stderr probeOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		if probeCtx.Err() != nil {
			result.Detail = "找到 Pi，但版本检测超时。请检查此安装是否能正常运行。"
		} else if output := strings.ToLower(string(stderr.data)); strings.Contains(output, "node") && (strings.Contains(output, "no such file") || strings.Contains(output, "not found")) {
			result.Detail = "找到 Pi，但桌面应用无法找到 Node.js。npm 安装需要可用的 Node.js；可改用 Pi standalone 安装，或修复桌面应用的 PATH。"
		} else {
			result.Detail = fmt.Sprintf("找到 Pi，但无法运行 --version（%v）。请检查安装与运行环境；npm 安装还需要 Node.js。", err)
		}
		return result
	}
	version := strings.TrimSpace(string(stdout.data))
	if stdout.truncated || !piVersionPattern.MatchString(version) {
		result.Detail = "找到可执行文件，但无法识别 Pi 版本输出。请确认路径指向 Pi。"
		return result
	}
	result.Version = version
	if version != supportedPiVersion {
		result.Status = "unsupported"
		result.Detail = fmt.Sprintf("当前仅验证 Pi %s；检测到 %s。请安装兼容版本，或选择其他 Pi 安装。", supportedPiVersion, version)
		return result
	}
	result.Status, result.Detail = "ready", "Pi 1.4.0 已就绪。"
	return result
}

type probeOutput struct {
	data      []byte
	truncated bool
}

func (output *probeOutput) Write(data []byte) (int, error) {
	length := min(len(data), 4096-len(output.data))
	output.data = append(output.data, data[:length]...)
	output.truncated = output.truncated || length < len(data)
	return len(data), nil
}
