package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

var Version = "dev"

const (
	logo = `
  ██╗   ██╗ ██████╗ ██╗      ██████╗ ██████╗  ██████╗ ██╗  ██╗
  ╚██╗ ██╔╝██╔═══██╗██║     ██╔═══██╗██╔══██╗██╔═══██╗╚██╗██╔╝
   ╚████╔╝ ██║   ██║██║     ██║   ██║██████╔╝██║   ██║ ╚███╔╝
    ╚██╔╝  ██║   ██║██║     ██║   ██║██╔══██╗██║   ██║ ██╔██╗
     ██║   ╚██████╔╝███████╗╚██████╔╝██████╔╝╚██████╔╝██╔╝ ██╗
     ╚═╝    ╚═════╝ ╚══════╝ ╚═════╝ ╚═════╝  ╚═════╝ ╚═╝  ╚═╝
`
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorBold   = "\033[1m"
)

// Common API keys to auto-passthrough
var autoPassthroughEnvVars = []string{
	"ANTHROPIC_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"OPENAI_API_KEY",
	"COPILOT_GITHUB_TOKEN",
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"OPENROUTER_API_KEY",
	"GEMINI_API_KEY",
}

// Tool shortcuts - these become direct subcommands (e.g., "yolobox claude")
var toolShortcuts = []string{
	"claude",
	"codex",
	"gemini",
	"opencode",
	"copilot",
}

var sizePattern = regexp.MustCompile(`^\d+(?:\.\d+)?(?:[kKmMgGtTpP](?:i?[bB]?)?|[bB])?$`)

type Config struct {
	Runtime               string   `toml:"runtime"`
	Image                 string   `toml:"image"`
	Mounts                []string `toml:"mounts"`
	Env                   []string `toml:"env"`
	SSHAgent              bool     `toml:"ssh_agent"`
	ReadonlyProject       bool     `toml:"readonly_project"`
	NoNetwork             bool     `toml:"no_network"`
	Network               string   `toml:"network"`
	Pod                   string   `toml:"pod"`
	NoYolo                bool     `toml:"no_yolo"`
	Scratch               bool     `toml:"scratch"`
	ClaudeConfig          bool     `toml:"claude_config"`
	GeminiConfig          bool     `toml:"gemini_config"`
	GitConfig             bool     `toml:"git_config"`
	GhToken               bool     `toml:"gh_token"`
	CopyAgentInstructions bool     `toml:"copy_agent_instructions"`
	Docker                bool     `toml:"docker"`

	// Resource limits
	CPUs        string   `toml:"cpus"`
	Memory      string   `toml:"memory"`
	ShmSize     string   `toml:"shm_size"`
	GPUs        string   `toml:"gpus"`
	Devices     []string `toml:"devices"`
	CapAdd      []string `toml:"cap_add"`
	CapDrop     []string `toml:"cap_drop"`
	RuntimeArgs []string `toml:"runtime_args"`

	// Runtime-only fields (not persisted to config file)
	Setup bool `toml:"-"` // Run interactive setup before starting
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

var errHelp = errors.New("help requested")

// Version check cache
type versionCache struct {
	LatestVersion string    `json:"latest_version"`
	CheckedAt     time.Time `json:"checked_at"`
}

const versionCheckInterval = 24 * time.Hour

func versionCachePath() string {
	configDir, _ := os.UserConfigDir()
	return filepath.Join(configDir, "yolobox", "version-check.json")
}

func checkForUpdates() {
	// Don't block on version check - run with a short timeout
	done := make(chan struct{})
	go func() {
		defer close(done)
		doVersionCheck()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Timeout - skip update check
	}
}

func doVersionCheck() {
	cachePath := versionCachePath()

	// Try to read cache
	var cache versionCache
	if data, err := os.ReadFile(cachePath); err == nil {
		if err := json.Unmarshal(data, &cache); err == nil {
			// Cache is valid, check if it's fresh enough
			if time.Since(cache.CheckedAt) < versionCheckInterval {
				// Use cached version
				showUpdateMessage(cache.LatestVersion)
				return
			}
		}
	}

	// Fetch latest version from GitHub
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/finbarr/yolobox/releases/latest")
	if err != nil {
		return // Silently fail
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")

	// Update cache
	cache = versionCache{
		LatestVersion: latestVersion,
		CheckedAt:     time.Now(),
	}
	if data, err := json.Marshal(cache); err == nil {
		os.MkdirAll(filepath.Dir(cachePath), 0755)
		os.WriteFile(cachePath, data, 0644)
	}

	showUpdateMessage(latestVersion)
}

func showUpdateMessage(latestVersion string) {
	currentVersion := strings.TrimPrefix(Version, "v")
	if latestVersion != "" && latestVersion != currentVersion && latestVersion > currentVersion {
		fmt.Fprintf(os.Stderr, "\n%s💡 yolobox v%s available:%s https://github.com/finbarr/yolobox/releases/tag/v%s\n",
			colorYellow, latestVersion, colorReset, latestVersion)
		fmt.Fprintf(os.Stderr, "   Run %syolobox upgrade%s to update\n\n", colorBold, colorReset)
	}
}

func main() {
	os.Exit(run())
}

func run() int {
	if err := runCmd(); err != nil {
		if errors.Is(err, errHelp) {
			return 0
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		errorf("%v", err)
		return 1
	}
	return 0
}

func runCmd() error {
	args := os.Args[1:]

	// Check for updates (skip for version/help/upgrade commands)
	skipCheck := len(args) > 0 && (args[0] == "version" || args[0] == "help" || args[0] == "upgrade")
	if !skipCheck {
		checkForUpdates()
	}

	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		cfg, rest, err := parseBaseFlags("yolobox", args, projectDir)
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return fmt.Errorf("unexpected args: %v\n  Hint: flags go after the subcommand: yolobox run --flag cmd (not yolobox --flag run cmd)", rest)
		}
		return runShell(cfg)
	}

	switch args[0] {
	case "run":
		cfg, rest, err := parseBaseFlags("run", args[1:], projectDir)
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			return fmt.Errorf("run requires a command")
		}
		return runCommand(cfg, rest, false)
	case "setup":
		_, err := runSetup()
		return err
	case "upgrade":
		return upgradeYolobox()
	case "config":
		cfg, rest, err := parseBaseFlags("config", args[1:], projectDir)
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return fmt.Errorf("unexpected args: %v", rest)
		}
		if err := validateRuntimeConstraints(cfg); err != nil {
			return err
		}
		return printConfig(cfg)
	case "reset":
		return resetVolumes(args[1:])
	case "uninstall":
		return uninstallYolobox(args[1:])
	case "version":
		printVersion()
		return nil
	case "help":
		printUsage()
		return errHelp
	default:
		// Check if it's a tool shortcut (e.g., "yolobox claude", "yolobox codex")
		if isToolShortcut(args[0]) {
			toolName := args[0]
			// Split args so tool-specific flags (like --resume) pass through
			yoloboxArgs, toolArgs := splitToolArgs(args[1:])

			cfg, rest, err := parseBaseFlags(toolName, yoloboxArgs, projectDir)
			if err != nil {
				return err
			}

			// Combine any remaining args from flag parsing with tool args
			allToolArgs := append(rest, toolArgs...)

			// Print logo before running tool
			fmt.Fprint(os.Stderr, colorCyan+logo+colorReset)
			// Build command: tool name + any tool args
			command := append([]string{toolName}, allToolArgs...)
			return runCommand(cfg, command, false)
		}
		return fmt.Errorf("unknown command: %s (try 'yolobox help')\n  Hint: if using flags, put them after the subcommand: yolobox run --flag cmd", args[0])
	}
}

func printVersion() {
	fmt.Printf("%syolobox%s %s%s%s (%s/%s)\n", colorBold, colorReset, colorCyan, Version, colorReset, runtime.GOOS, runtime.GOARCH)
}

func printUsage() {
	fmt.Fprint(os.Stderr, colorCyan+logo+colorReset)
	fmt.Fprintf(os.Stderr, "  %sFull-power AI agents, host-safe by default.%s\n\n", colorYellow, colorReset)
	fmt.Fprintf(os.Stderr, "  %sVersion:%s %s\n\n", colorBold, colorReset, Version)
	fmt.Fprintf(os.Stderr, "%sUSAGE:%s\n", colorBold, colorReset)
	fmt.Fprintln(os.Stderr, "  yolobox                     Start interactive shell in sandbox")
	fmt.Fprintln(os.Stderr, "  yolobox run <cmd...>        Run a command in sandbox")
	fmt.Fprintln(os.Stderr, "  yolobox setup               Configure yolobox settings")
	fmt.Fprintln(os.Stderr, "  yolobox upgrade             Upgrade binary and pull latest image")
	fmt.Fprintln(os.Stderr, "  yolobox config              Print resolved configuration")
	fmt.Fprintln(os.Stderr, "  yolobox reset --force       Remove named volumes (fresh start)")
	fmt.Fprintln(os.Stderr, "  yolobox uninstall --force   Uninstall yolobox completely")
	fmt.Fprintln(os.Stderr, "  yolobox version             Show version info")
	fmt.Fprintln(os.Stderr, "  yolobox help                Show this help")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "%sTOOL SHORTCUTS:%s\n", colorBold, colorReset)
	for _, tool := range toolShortcuts {
		fmt.Fprintf(os.Stderr, "  yolobox %-20sRun %s in sandbox\n", tool, tool)
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "%sFLAGS:%s\n", colorBold, colorReset)
	fmt.Fprintln(os.Stderr, "  --runtime <name>      Container runtime: docker, podman, or container")
	fmt.Fprintln(os.Stderr, "  --image <name>        Base image to use")
	fmt.Fprintln(os.Stderr, "  --pod <name>          Join existing Podman pod (shares its network)")
	fmt.Fprintln(os.Stderr, "  --setup               Run interactive setup before starting")
	fmt.Fprintln(os.Stderr, "  --mount <src:dst>     Extra mount (repeatable)")
	fmt.Fprintln(os.Stderr, "  --env <KEY=val>       Set environment variable (repeatable)")
	fmt.Fprintln(os.Stderr, "  --ssh-agent           Forward SSH agent socket")
	fmt.Fprintln(os.Stderr, "  --no-network          Disable network access (default: network enabled)")
	fmt.Fprintln(os.Stderr, "  --network <name>      Join container network (e.g., docker compose network)")
	fmt.Fprintln(os.Stderr, "  --no-yolo             Disable AI CLIs YOLO mode")
	fmt.Fprintln(os.Stderr, "  --scratch             Fresh environment, no persistent volumes")
	fmt.Fprintln(os.Stderr, "  --readonly-project    Mount project directory read-only")
	fmt.Fprintln(os.Stderr, "  --claude-config       Copy host Claude config to container")
	fmt.Fprintln(os.Stderr, "  --gemini-config       Copy host Gemini config to container")
	fmt.Fprintln(os.Stderr, "  --git-config          Copy host git config to container")
	fmt.Fprintln(os.Stderr, "  --gh-token            Forward GitHub CLI token (from gh auth token)")
	fmt.Fprintln(os.Stderr, "  --copy-agent-instructions  Copy global agent instruction files")
	fmt.Fprintln(os.Stderr, "  --docker              Mount Docker socket and join shared network")
	fmt.Fprintln(os.Stderr, "  --cpus <count>        Limit number of CPUs (supports fractions)")
	fmt.Fprintln(os.Stderr, "  --memory <size>       Cap memory usage (e.g., 4g, 512m)")
	fmt.Fprintln(os.Stderr, "  --shm-size <size>     Size of /dev/shm (e.g., 1g for Playwright)")
	fmt.Fprintln(os.Stderr, "  --gpus <spec>         GPU devices to add (e.g., all, device=0)")
	fmt.Fprintln(os.Stderr, "  --device <spec>       Pass a host device through (repeatable)")
	fmt.Fprintln(os.Stderr, "  --cap-add <name>      Add a Linux capability (repeatable)")
	fmt.Fprintln(os.Stderr, "  --cap-drop <name>     Drop a Linux capability (repeatable)")
	fmt.Fprintln(os.Stderr, "  --runtime-arg <flag>  Raw runtime flag passthrough (repeatable)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "%sCONFIG:%s\n", colorBold, colorReset)
	fmt.Fprintln(os.Stderr, "  Global:  ~/.config/yolobox/config.toml")
	fmt.Fprintln(os.Stderr, "  Project: .yolobox.toml")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "%sAUTO-FORWARDED ENV VARS:%s\n", colorBold, colorReset)
	fmt.Fprintln(os.Stderr, "  ANTHROPIC_API_KEY, OPENAI_API_KEY, COPILOT_GITHUB_TOKEN, GH_TOKEN, GITHUB_TOKEN")
	fmt.Fprintln(os.Stderr, "  OPENROUTER_API_KEY, GEMINI_API_KEY, GEMINI_MODEL, GOOGLE_API_KEY")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "%sEXAMPLES:%s\n", colorBold, colorReset)
	fmt.Fprintln(os.Stderr, "  yolobox                     # Drop into a shell")
	fmt.Fprintln(os.Stderr, "  yolobox run make build      # Run make inside sandbox")
	fmt.Fprintln(os.Stderr, "  yolobox run claude          # Run Claude Code in sandbox")
	fmt.Fprintln(os.Stderr, "  yolobox --no-network        # Paranoid mode: no internet")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "  %sLet your AI go full send. Your home directory stays home.%s\n\n", colorPurple, colorReset)
}

// parseBaseFlags parses CLI flags and merges them with config files.
// projectDir is passed as a parameter (rather than calling os.Getwd() inside)
// to enable testing without mutating global working directory state.
func parseBaseFlags(name string, args []string, projectDir string) (Config, []string, error) {
	cfg, err := loadConfig(projectDir)
	if err != nil {
		return Config{}, nil, err
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = printUsage

	var (
		runtimeFlag           string
		imageFlag             string
		podFlag               string
		networkFlag           string
		sshAgent              bool
		readonlyProject       bool
		noNetwork             bool
		noYolo                bool
		scratch               bool
		claudeConfig          bool
		geminiConfig          bool
		gitConfig             bool
		ghToken               bool
		copyAgentInstructions bool
		docker                bool
		setup                 bool
		mounts                stringSliceFlag
		envVars               stringSliceFlag

		// Resource limits & security
		cpus        string
		memoryLimit string
		shmSize     string
		gpus        string
		devices     stringSliceFlag
		capAdd      stringSliceFlag
		capDrop     stringSliceFlag
		runtimeArgs stringSliceFlag
	)

	fs.StringVar(&runtimeFlag, "runtime", "", "container runtime")
	fs.StringVar(&imageFlag, "image", "", "container image")
	fs.StringVar(&podFlag, "pod", "", "join existing podman pod")
	fs.StringVar(&networkFlag, "network", "", "container network to join")
	fs.BoolVar(&sshAgent, "ssh-agent", false, "mount SSH agent socket")
	fs.BoolVar(&readonlyProject, "readonly-project", false, "mount project read-only")
	fs.BoolVar(&noNetwork, "no-network", false, "disable network")
	fs.BoolVar(&noYolo, "no-yolo", false, "disable AI CLIs YOLO mode")
	fs.BoolVar(&scratch, "scratch", false, "fresh environment, no persistent volumes")
	fs.BoolVar(&claudeConfig, "claude-config", false, "copy host Claude config to container")
	fs.BoolVar(&geminiConfig, "gemini-config", false, "copy host Gemini config to container")
	fs.BoolVar(&gitConfig, "git-config", false, "copy host git config to container")
	fs.BoolVar(&ghToken, "gh-token", false, "forward GitHub CLI token (from gh auth token)")
	fs.BoolVar(&copyAgentInstructions, "copy-agent-instructions", false, "copy agent instruction files (CLAUDE.md, GEMINI.md, AGENTS.md)")
	fs.BoolVar(&docker, "docker", false, "mount Docker socket and join shared network")
	fs.BoolVar(&setup, "setup", false, "run interactive setup before starting")
	fs.Var(&mounts, "mount", "extra mount src:dst")
	fs.Var(&envVars, "env", "environment variable KEY=value")

	// Resource limits & security
	fs.StringVar(&cpus, "cpus", "", "limit number of CPUs (supports fractions)")
	fs.StringVar(&memoryLimit, "memory", "", "memory limit (e.g., 8g)")
	fs.StringVar(&shmSize, "shm-size", "", "size of /dev/shm (e.g., 1g)")
	fs.StringVar(&gpus, "gpus", "", "GPU devices to add (e.g., all or device IDs)")
	fs.Var(&devices, "device", "add host device inside the container (repeatable)")
	fs.Var(&capAdd, "cap-add", "add Linux capability (repeatable)")
	fs.Var(&capDrop, "cap-drop", "drop Linux capability (repeatable)")
	fs.Var(&runtimeArgs, "runtime-arg", "raw runtime flag to pass through to the container engine (repeatable)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage()
			return Config{}, nil, errHelp
		}
		return Config{}, nil, err
	}

	if runtimeFlag != "" {
		cfg.Runtime = runtimeFlag
	}
	if imageFlag != "" {
		cfg.Image = imageFlag
	}
	if podFlag != "" {
		cfg.Pod = podFlag
	}
	if sshAgent {
		cfg.SSHAgent = true
	}
	if readonlyProject {
		cfg.ReadonlyProject = true
	}
	if noNetwork {
		cfg.NoNetwork = true
	}
	if networkFlag != "" {
		cfg.Network = networkFlag
	}
	if noYolo {
		cfg.NoYolo = true
	}
	if scratch {
		cfg.Scratch = true
	}
	if claudeConfig {
		cfg.ClaudeConfig = true
	}
	if geminiConfig {
		cfg.GeminiConfig = true
	}
	if gitConfig {
		cfg.GitConfig = true
	}
	if ghToken {
		cfg.GhToken = true
	}
	if copyAgentInstructions {
		cfg.CopyAgentInstructions = true
	}
	if docker {
		cfg.Docker = true
	}
	if setup {
		cfg.Setup = true
	}
	if len(mounts) > 0 {
		cfg.Mounts = append(cfg.Mounts, mounts...)
	}
	if len(envVars) > 0 {
		cfg.Env = append(cfg.Env, envVars...)
	}

	if cpus != "" {
		cfg.CPUs = cpus
	}
	if memoryLimit != "" {
		cfg.Memory = memoryLimit
	}
	if shmSize != "" {
		cfg.ShmSize = shmSize
	}
	if gpus != "" {
		cfg.GPUs = gpus
	}

	if len(devices) > 0 {
		cfg.Devices = append(cfg.Devices, devices...)
	}
	if len(capAdd) > 0 {
		cfg.CapAdd = append(cfg.CapAdd, capAdd...)
	}
	if len(capDrop) > 0 {
		cfg.CapDrop = append(cfg.CapDrop, capDrop...)
	}
	if len(runtimeArgs) > 0 {
		cfg.RuntimeArgs = append(cfg.RuntimeArgs, runtimeArgs...)
	}

	// Validate conflicting options after config + CLI values have been merged.
	if err := validateConfigConflicts(cfg); err != nil {
		return cfg, nil, err
	}
	if err := validateRuntimeOptions(cfg); err != nil {
		return cfg, nil, err
	}

	return cfg, fs.Args(), nil
}

func validateConfigConflicts(cfg Config) error {
	if cfg.Network != "" && cfg.NoNetwork {
		return fmt.Errorf("cannot use --network with --no-network")
	}
	if cfg.Docker && cfg.NoNetwork {
		return fmt.Errorf("cannot use --docker with --no-network")
	}
	if cfg.Pod != "" {
		if cfg.Network != "" {
			return fmt.Errorf("cannot use --pod with --network")
		}
		if cfg.NoNetwork {
			return fmt.Errorf("cannot use --pod with --no-network")
		}
		if cfg.Docker {
			return fmt.Errorf("cannot use --pod with --docker")
		}
	}
	return nil
}

func validateRuntimeConstraints(cfg Config) error {
	if cfg.Pod == "" {
		return nil
	}

	runtimePath, err := resolveRuntime(cfg.Runtime)
	if err != nil {
		return err
	}
	if filepath.Base(runtimePath) != "podman" {
		return fmt.Errorf("--pod requires the podman runtime (set --runtime podman)")
	}
	return nil
}

func validateRuntimeOptions(cfg Config) error {
	if cfg.CPUs != "" {
		cpuVal, err := strconv.ParseFloat(cfg.CPUs, 64)
		if err != nil || cpuVal <= 0 {
			return fmt.Errorf("invalid --cpus value %q: must be a positive number", cfg.CPUs)
		}
	}

	for _, field := range []struct {
		name  string
		value string
	}{
		{"memory", cfg.Memory},
		{"shm-size", cfg.ShmSize},
	} {
		if field.value == "" {
			continue
		}
		if !sizePattern.MatchString(field.value) {
			return fmt.Errorf("invalid --%s value %q: expected a number optionally followed by k/m/g/t", field.name, field.value)
		}
	}

	for _, arg := range cfg.RuntimeArgs {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("--runtime-arg entries cannot be blank")
		}
	}

	return nil
}

func warnSecurityRelaxations(cfg Config) {
	var categories []string
	if len(cfg.CapAdd) > 0 {
		categories = append(categories, "--cap-add")
	}
	if len(cfg.Devices) > 0 {
		categories = append(categories, "--device")
	}
	if runtimeArgsContainUnconfined(cfg.RuntimeArgs) {
		categories = append(categories, "--security-opt seccomp=unconfined")
	}
	if len(categories) == 0 {
		return
	}
	warn("Security-impacting runtime flags active (%s). Ensure you trust the workload.", strings.Join(categories, ", "))
}

func runtimeArgsContainUnconfined(args []string) bool {
	for _, arg := range args {
		if strings.Contains(strings.ToLower(arg), "seccomp=unconfined") {
			return true
		}
	}
	return false
}

func defaultConfig() Config {
	return Config{
		Image: "ghcr.io/finbarr/yolobox:latest",
	}
}

func loadConfig(projectDir string) (Config, error) {
	cfg := defaultConfig()

	globalPath, err := globalConfigPath()
	if err != nil {
		return Config{}, err
	}
	if err := mergeConfigFile(globalPath, &cfg); err != nil {
		return Config{}, err
	}

	projectPath := filepath.Join(projectDir, ".yolobox.toml")
	if err := mergeConfigFile(projectPath, &cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func globalConfigPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "yolobox", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "yolobox", "config.toml"), nil
}

func mergeConfigFile(path string, cfg *Config) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var fileCfg Config
	if _, err := toml.DecodeFile(path, &fileCfg); err != nil {
		return err
	}

	mergeConfig(cfg, fileCfg)
	return nil
}

func mergeConfig(dst *Config, src Config) {
	if src.Runtime != "" {
		dst.Runtime = src.Runtime
	}
	if src.Image != "" {
		dst.Image = src.Image
	}
	if len(src.Mounts) > 0 {
		dst.Mounts = append([]string{}, src.Mounts...)
	}
	if len(src.Env) > 0 {
		dst.Env = append([]string{}, src.Env...)
	}
	if src.SSHAgent {
		dst.SSHAgent = true
	}
	if src.ReadonlyProject {
		dst.ReadonlyProject = true
	}
	if src.NoNetwork {
		dst.NoNetwork = true
	}
	if src.Network != "" {
		dst.Network = src.Network
	}
	if src.Pod != "" {
		dst.Pod = src.Pod
	}
	if src.NoYolo {
		dst.NoYolo = true
	}
	if src.Scratch {
		dst.Scratch = true
	}
	if src.ClaudeConfig {
		dst.ClaudeConfig = true
	}
	if src.GeminiConfig {
		dst.GeminiConfig = true
	}
	if src.GitConfig {
		dst.GitConfig = true
	}
	if src.GhToken {
		dst.GhToken = true
	}
	if src.CopyAgentInstructions {
		dst.CopyAgentInstructions = true
	}
	if src.Docker {
		dst.Docker = true
	}

	if src.CPUs != "" {
		dst.CPUs = src.CPUs
	}
	if src.Memory != "" {
		dst.Memory = src.Memory
	}
	if src.ShmSize != "" {
		dst.ShmSize = src.ShmSize
	}
	if src.GPUs != "" {
		dst.GPUs = src.GPUs
	}
	if len(src.Devices) > 0 {
		dst.Devices = append([]string{}, src.Devices...)
	}
	if len(src.CapAdd) > 0 {
		dst.CapAdd = append([]string{}, src.CapAdd...)
	}
	if len(src.CapDrop) > 0 {
		dst.CapDrop = append([]string{}, src.CapDrop...)
	}
	if len(src.RuntimeArgs) > 0 {
		dst.RuntimeArgs = append([]string{}, src.RuntimeArgs...)
	}
}

func runShell(cfg Config) error {
	// Run setup if explicitly requested via --setup flag
	if cfg.Setup {
		newCfg, err := runSetup()
		if err != nil {
			// If setup was cancelled, continue with defaults
			if err.Error() == "setup cancelled" {
				info("Using default settings")
			} else {
				return err
			}
		} else {
			// Merge setup results into config (preserving any CLI overrides).
			// mergeConfig applies cfg's non-zero values on top of newCfg,
			// so CLI/config-file settings win and setup fills the gaps.
			mergeConfig(&newCfg, cfg)
			cfg = newCfg
		}
	}

	// Print logo before entering container
	fmt.Fprint(os.Stderr, colorCyan+logo+colorReset)

	err := runCommand(cfg, []string{"bash"}, true)
	if err != nil {
		return fmt.Errorf("failed to start shell: %w", err)
	}
	return nil
}

func runCommand(cfg Config, command []string, interactive bool) error {
	projectDir, err := os.Getwd()
	if err != nil {
		return err
	}

	// Warn about scratch mode implications
	if cfg.Scratch {
		warn("Scratch mode: /home/yolo and /var/cache are ephemeral (data will not persist)")
		if cfg.ReadonlyProject {
			warn("Scratch mode with readonly-project: /output is ephemeral (copy files out before exiting)")
		}
	}

	if err := validateRuntimeConstraints(cfg); err != nil {
		return err
	}
	warnSecurityRelaxations(cfg)

	// Warn if Docker has low memory (can cause OOM with Claude)
	checkDockerMemory(cfg.Runtime)

	// Ensure Docker network exists before starting container
	if cfg.Docker {
		networkName := cfg.Network
		if networkName == "" {
			networkName = "yolobox-net"
		}
		if err := ensureDockerNetwork(cfg.Runtime, networkName); err != nil {
			return err
		}
	}

	args, cleanupPaths, err := buildRunArgs(cfg, projectDir, command, interactive)
	if err != nil {
		return err
	}
	// Clean up temp files after the container exits, regardless of outcome
	defer func() {
		for _, p := range cleanupPaths {
			os.RemoveAll(p)
		}
	}()
	return execRuntime(cfg.Runtime, args)
}

func printConfig(cfg Config) error {
	projectDir, err := os.Getwd()
	if err != nil {
		return err
	}
	fmt.Printf("%sruntime:%s %s\n", colorBold, colorReset, resolvedRuntimeName(cfg.Runtime))
	fmt.Printf("%simage:%s %s\n", colorBold, colorReset, cfg.Image)
	fmt.Printf("%sproject:%s %s\n", colorBold, colorReset, projectDir)
	fmt.Printf("%sssh_agent:%s %t\n", colorBold, colorReset, cfg.SSHAgent)
	fmt.Printf("%sreadonly_project:%s %t\n", colorBold, colorReset, cfg.ReadonlyProject)
	fmt.Printf("%sno_network:%s %t\n", colorBold, colorReset, cfg.NoNetwork)
	fmt.Printf("%snetwork:%s %s\n", colorBold, colorReset, cfg.Network)
	fmt.Printf("%spod:%s %s\n", colorBold, colorReset, cfg.Pod)
	fmt.Printf("%sno_yolo:%s %t\n", colorBold, colorReset, cfg.NoYolo)
	fmt.Printf("%sscratch:%s %t\n", colorBold, colorReset, cfg.Scratch)
	fmt.Printf("%sclaude_config:%s %t\n", colorBold, colorReset, cfg.ClaudeConfig)
	fmt.Printf("%sgemini_config:%s %t\n", colorBold, colorReset, cfg.GeminiConfig)
	fmt.Printf("%sgit_config:%s %t\n", colorBold, colorReset, cfg.GitConfig)
	fmt.Printf("%sgh_token:%s %t\n", colorBold, colorReset, cfg.GhToken)
	fmt.Printf("%scopy_agent_instructions:%s %t\n", colorBold, colorReset, cfg.CopyAgentInstructions)
	fmt.Printf("%sdocker:%s %t\n", colorBold, colorReset, cfg.Docker)

	printStringConfigField("cpus", cfg.CPUs)
	printStringConfigField("memory", cfg.Memory)
	printStringConfigField("shm_size", cfg.ShmSize)
	printStringConfigField("gpus", cfg.GPUs)
	printSliceConfigField("devices", cfg.Devices)
	printSliceConfigField("cap_add", cfg.CapAdd)
	printSliceConfigField("cap_drop", cfg.CapDrop)
	printSliceConfigField("runtime_args", cfg.RuntimeArgs)

	if len(cfg.Mounts) > 0 {
		fmt.Printf("%smounts:%s\n", colorBold, colorReset)
		for _, m := range cfg.Mounts {
			fmt.Printf("  - %s\n", m)
		}
	}
	if len(cfg.Env) > 0 {
		fmt.Printf("%senv:%s\n", colorBold, colorReset)
		for _, e := range cfg.Env {
			fmt.Printf("  - %s\n", e)
		}
	}
	return nil
}

func printStringConfigField(name, value string) {
	fmt.Printf("%s%s:%s %s\n", colorBold, name, colorReset, configValueOrNotSet(value))
}

func printSliceConfigField(name string, values []string) {
	if len(values) == 0 {
		fmt.Printf("%s%s:%s (none)\n", colorBold, name, colorReset)
		return
	}
	fmt.Printf("%s%s:%s\n", colorBold, name, colorReset)
	for _, v := range values {
		fmt.Printf("  - %s\n", v)
	}
}

func configValueOrNotSet(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(not set)"
	}
	return value
}

// globalConfigExists checks if the global config file exists
func globalConfigExists() bool {
	path, err := globalConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// saveGlobalConfig writes config to the global config file
func saveGlobalConfig(cfg Config) error {
	path, err := globalConfigPath()
	if err != nil {
		return err
	}

	// Create config directory if needed
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Build TOML content (only non-default values)
	var lines []string
	if cfg.GitConfig {
		lines = append(lines, "git_config = true")
	}
	if cfg.GhToken {
		lines = append(lines, "gh_token = true")
	}
	if cfg.SSHAgent {
		lines = append(lines, "ssh_agent = true")
	}
	if cfg.NoNetwork {
		lines = append(lines, "no_network = true")
	}
	if cfg.Network != "" {
		lines = append(lines, fmt.Sprintf("network = %q", cfg.Network))
	}
	if cfg.NoYolo {
		lines = append(lines, "no_yolo = true")
	}
	if cfg.Docker {
		lines = append(lines, "docker = true")
	}
	if cfg.Pod != "" {
		lines = append(lines, fmt.Sprintf("pod = %q", cfg.Pod))
	}
	if cfg.CPUs != "" {
		lines = append(lines, fmt.Sprintf("cpus = %q", cfg.CPUs))
	}
	if cfg.Memory != "" {
		lines = append(lines, fmt.Sprintf("memory = %q", cfg.Memory))
	}
	if cfg.ShmSize != "" {
		lines = append(lines, fmt.Sprintf("shm_size = %q", cfg.ShmSize))
	}
	if cfg.GPUs != "" {
		lines = append(lines, fmt.Sprintf("gpus = %q", cfg.GPUs))
	}
	if len(cfg.Devices) > 0 {
		lines = append(lines, fmt.Sprintf("devices = %s", formatTomlStringSlice(cfg.Devices)))
	}
	if len(cfg.CapAdd) > 0 {
		lines = append(lines, fmt.Sprintf("cap_add = %s", formatTomlStringSlice(cfg.CapAdd)))
	}
	if len(cfg.CapDrop) > 0 {
		lines = append(lines, fmt.Sprintf("cap_drop = %s", formatTomlStringSlice(cfg.CapDrop)))
	}
	if len(cfg.RuntimeArgs) > 0 {
		lines = append(lines, fmt.Sprintf("runtime_args = %s", formatTomlStringSlice(cfg.RuntimeArgs)))
	}

	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	return nil
}

func formatTomlStringSlice(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	if len(quoted) == 0 {
		return "[]"
	}
	return fmt.Sprintf("[%s]", strings.Join(quoted, ", "))
}

func parseMultilineInput(input string) []string {
	if input == "" {
		return nil
	}
	input = strings.ReplaceAll(input, "\r\n", "\n")
	lines := strings.Split(input, "\n")
	var values []string
	for _, line := range lines {
		val := strings.TrimSpace(line)
		if val == "" {
			continue
		}
		values = append(values, val)
	}
	return values
}

// yoloboxTheme returns a custom huh theme matching the yolobox brand
func yoloboxTheme() *huh.Theme {
	t := huh.ThemeBase()

	purple := lipgloss.Color("35") // magenta/purple
	cyan := lipgloss.Color("36")   // cyan
	yellow := lipgloss.Color("33") // yellow
	white := lipgloss.Color("15")  // bright white

	// Title styling - purple and bold
	t.Focused.Title = t.Focused.Title.Foreground(purple).Bold(true)
	t.Focused.Description = t.Focused.Description.Foreground(white)

	// Selection styling
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(yellow)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(cyan)
	t.Focused.UnselectedOption = t.Focused.UnselectedOption.Foreground(white)

	// Multi-select styling
	t.Focused.MultiSelectSelector = t.Focused.MultiSelectSelector.Foreground(yellow)
	t.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(cyan).SetString("[x] ")
	t.Focused.UnselectedPrefix = lipgloss.NewStyle().Foreground(white).SetString("[ ] ")

	return t
}

// runSetup runs the interactive setup wizard
func runSetup() (Config, error) {
	cfg := Config{}

	// Load existing config as defaults
	if globalConfigExists() {
		projectDir, _ := os.Getwd()
		existing, err := loadConfig(projectDir)
		if err == nil {
			cfg = existing
		}
	}

	// Form fields
	var selectedOptions []string
	podName := cfg.Pod
	cpuLimit := cfg.CPUs
	memoryLimit := cfg.Memory
	shmLimit := cfg.ShmSize
	gpuSetting := cfg.GPUs
	deviceList := strings.Join(cfg.Devices, "\n")
	capAddList := strings.Join(cfg.CapAdd, "\n")
	capDropList := strings.Join(cfg.CapDrop, "\n")
	runtimeArgList := strings.Join(cfg.RuntimeArgs, "\n")

	// Initialize from current config
	if cfg.GitConfig {
		selectedOptions = append(selectedOptions, "git_config")
	}
	if cfg.GhToken {
		selectedOptions = append(selectedOptions, "gh_token")
	}
	if cfg.SSHAgent {
		selectedOptions = append(selectedOptions, "ssh_agent")
	}
	if cfg.NoNetwork {
		selectedOptions = append(selectedOptions, "no_network")
	}
	if cfg.NoYolo {
		selectedOptions = append(selectedOptions, "no_yolo")
	}
	if cfg.Docker {
		selectedOptions = append(selectedOptions, "docker")
	}

	// Print header with box
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("35")). // purple
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("35")).
		Padding(0, 2)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, headerStyle.Render("yolobox setup"))
	fmt.Fprintln(os.Stderr)

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("What do you want inside the box?").
				Options(
					huh.NewOption("Git identity (copy ~/.gitconfig)", "git_config"),
					huh.NewOption("GitHub CLI token (forward gh auth)", "gh_token"),
					huh.NewOption("SSH agent (for git over SSH)", "ssh_agent"),
					huh.NewOption("Docker socket (run containers from sandbox)", "docker"),
					huh.NewOption("No network (disable internet access)", "no_network"),
					huh.NewOption("No YOLO (disable auto-confirm in AI CLIs)", "no_yolo"),
				).
				Value(&selectedOptions),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Podman pod (optional)").
				Description("Join an existing Podman pod by name (shares its network)").
				Placeholder("e.g. mypod").
				Value(&podName),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Default CPU limit (--cpus)").
				Description("Optional, supports fractions like 1.5").
				Placeholder("e.g. 2").
				Value(&cpuLimit),
			huh.NewInput().
				Title("Default memory limit (--memory)").
				Description("Optional, e.g., 4g or 512m").
				Placeholder("e.g. 4g").
				Value(&memoryLimit),
			huh.NewInput().
				Title("Default shm size (--shm-size)").
				Description("Optional, e.g., 1g for Playwright").
				Placeholder("e.g. 1g").
				Value(&shmLimit),
			huh.NewInput().
				Title("Default GPUs (--gpus)").
				Description("Optional, e.g., all or device=0").
				Placeholder("e.g. all").
				Value(&gpuSetting),
		),
		huh.NewGroup(
			huh.NewText().
				Title("Devices (--device)").
				Description("One per line, e.g., /dev/kvm:/dev/kvm").
				Lines(3).
				Placeholder("/dev/kvm:/dev/kvm").
				Value(&deviceList),
			huh.NewText().
				Title("Added capabilities (--cap-add)").
				Description("One per line, e.g., SYS_PTRACE").
				Lines(3).
				Placeholder("SYS_PTRACE").
				Value(&capAddList),
			huh.NewText().
				Title("Dropped capabilities (--cap-drop)").
				Description("One per line").
				Lines(3).
				Value(&capDropList),
			huh.NewText().
				Title("Raw runtime args (--runtime-arg)").
				Description("One per line, passed directly to Docker/Podman").
				Lines(4).
				Placeholder("--security-opt seccomp=unconfined").
				Value(&runtimeArgList),
		),
	).WithTheme(yoloboxTheme())

	err := form.Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return Config{}, fmt.Errorf("setup cancelled")
		}
		return Config{}, err
	}

	// Build config from form values
	cfg.GitConfig = contains(selectedOptions, "git_config")
	cfg.GhToken = contains(selectedOptions, "gh_token")
	cfg.SSHAgent = contains(selectedOptions, "ssh_agent")
	cfg.Docker = contains(selectedOptions, "docker")
	cfg.NoNetwork = contains(selectedOptions, "no_network")
	cfg.NoYolo = contains(selectedOptions, "no_yolo")
	cfg.Pod = strings.TrimSpace(podName)
	cfg.CPUs = strings.TrimSpace(cpuLimit)
	cfg.Memory = strings.TrimSpace(memoryLimit)
	cfg.ShmSize = strings.TrimSpace(shmLimit)
	cfg.GPUs = strings.TrimSpace(gpuSetting)
	cfg.Devices = parseMultilineInput(deviceList)
	cfg.CapAdd = parseMultilineInput(capAddList)
	cfg.CapDrop = parseMultilineInput(capDropList)
	cfg.RuntimeArgs = parseMultilineInput(runtimeArgList)

	if err := validateConfigConflicts(cfg); err != nil {
		return cfg, err
	}
	if err := validateRuntimeOptions(cfg); err != nil {
		return cfg, err
	}

	// Save to global config
	if err := saveGlobalConfig(cfg); err != nil {
		return cfg, err
	}

	path, _ := globalConfigPath()
	success("Locked in! Config saved to %s", path)
	fmt.Fprintf(os.Stderr, "  %sRun %syolobox setup%s%s anytime to change these settings.%s\n\n", colorCyan, colorBold, colorReset, colorCyan, colorReset)

	return cfg, nil
}

// contains checks if a string slice contains a value
func contains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

// isToolShortcut checks if a command is a tool shortcut
func isToolShortcut(cmd string) bool {
	return contains(toolShortcuts, cmd)
}

// splitToolArgs separates yolobox flags from tool flags for shortcuts.
// This allows `yolobox claude --resume` to pass --resume to claude instead of
// failing because --resume is not a known yolobox flag.
func splitToolArgs(args []string) (yoloboxArgs, toolArgs []string) {
	knownFlags := map[string]bool{
		"runtime": true, "image": true, "network": true, "pod": true,
		"ssh-agent": true, "readonly-project": true, "no-network": true,
		"no-yolo": true, "scratch": true, "claude-config": true,
		"gemini-config": true, "git-config": true, "gh-token": true,
		"copy-agent-instructions": true, "docker": true, "setup": true, "mount": true,
		"env": true, "h": true, "help": true,
		"cpus": true, "memory": true, "shm-size": true, "gpus": true,
		"device": true, "cap-add": true, "cap-drop": true, "runtime-arg": true,
	}

	flagsWithValues := map[string]bool{
		"runtime": true, "image": true, "network": true, "pod": true,
		"mount": true, "env": true, "cpus": true, "memory": true,
		"shm-size": true, "device": true, "cap-add": true, "cap-drop": true,
		"gpus": true, "runtime-arg": true,
	}

	i := 0
	for i < len(args) {
		arg := args[i]

		if arg == "--" {
			// Everything after -- goes to the tool
			return yoloboxArgs, args[i+1:]
		}

		if !strings.HasPrefix(arg, "-") {
			// Non-flag argument - this and rest go to tool
			return yoloboxArgs, args[i:]
		}

		// It's a flag, extract the name
		flagName := strings.TrimLeft(arg, "-")
		hasValue := false
		if idx := strings.Index(flagName, "="); idx != -1 {
			flagName = flagName[:idx]
			hasValue = true
		}

		if !knownFlags[flagName] {
			// Unknown flag - this and rest go to tool
			return yoloboxArgs, args[i:]
		}

		// Known yolobox flag
		yoloboxArgs = append(yoloboxArgs, arg)
		i++

		// If it's a flag that takes a value and doesn't have =, consume next arg
		if flagsWithValues[flagName] && !hasValue && i < len(args) && !strings.HasPrefix(args[i], "-") {
			yoloboxArgs = append(yoloboxArgs, args[i])
			i++
		}
	}

	return yoloboxArgs, nil
}

func resetVolumes(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "remove volumes")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage()
			return errHelp
		}
		return err
	}
	if !*force {
		return fmt.Errorf("reset requires --force (this will delete all cached data)")
	}

	cfg, err := loadConfigFromEnv()
	if err != nil {
		return err
	}
	runtime, err := resolveRuntime(cfg.Runtime)
	if err != nil {
		return err
	}

	warn("Removing yolobox volumes...")
	volumes := []string{"yolobox-home", "yolobox-cache"}
	args = append([]string{"volume", "rm"}, volumes...)
	if err := execCommand(runtime, args); err != nil {
		return err
	}
	success("Fresh start! All volumes removed.")
	return nil
}

func uninstallYolobox(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "confirm uninstall")
	keepVolumes := fs.Bool("keep-volumes", false, "keep Docker volumes")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage()
			return errHelp
		}
		return err
	}
	if !*force {
		fmt.Println("This will remove:")
		fmt.Println("  - yolobox binary")
		fmt.Println("  - ~/.config/yolobox/ (config and cache)")
		if !*keepVolumes {
			fmt.Println("  - Docker volumes (yolobox-home, yolobox-cache)")
		}
		fmt.Println("")
		return fmt.Errorf("run with --force to confirm (use --keep-volumes to preserve Docker data)")
	}

	// Remove config directory
	configDir, err := os.UserConfigDir()
	if err == nil {
		yoloboxConfig := filepath.Join(configDir, "yolobox")
		if _, err := os.Stat(yoloboxConfig); err == nil {
			info("Removing %s...", yoloboxConfig)
			os.RemoveAll(yoloboxConfig)
		}
	}

	// Remove Docker volumes unless --keep-volumes
	if !*keepVolumes {
		cfg, err := loadConfigFromEnv()
		if err == nil {
			runtime, err := resolveRuntime(cfg.Runtime)
			if err == nil {
				info("Removing Docker volumes...")
				volumes := []string{"yolobox-home", "yolobox-cache", "yolobox-output"}
				execCommand(runtime, append([]string{"volume", "rm", "-f"}, volumes...))
			}
		}
	}

	// Remove binary (do this last)
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	info("Removing %s...", execPath)
	if err := os.Remove(execPath); err != nil {
		return fmt.Errorf("failed to remove binary: %w (try: sudo rm %s)", err, execPath)
	}

	success("yolobox has been uninstalled. Goodbye!")
	return nil
}

func loadConfigFromEnv() (Config, error) {
	projectDir, err := os.Getwd()
	if err != nil {
		return Config{}, err
	}
	return loadConfig(projectDir)
}

// isAppleContainer checks if the resolved runtime is Apple's container tool
func isAppleContainer(runtime string) bool {
	path, err := resolveRuntime(runtime)
	if err != nil {
		return false
	}
	return strings.HasSuffix(path, "/container")
}

// prepareFileMountDir creates a temp directory with copies of files for Apple container
// (which only supports directory mounts, not file mounts). Returns the temp dir path.
// dirContainsSymlinks reports whether dir contains any symbolic links.
func dirContainsSymlinks(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			return true
		}
		// Check subdirectories recursively
		if e.IsDir() {
			if dirContainsSymlinks(filepath.Join(dir, e.Name())) {
				return true
			}
		}
	}
	return false
}

// stageDirResolvingSymlinks copies src to a temp directory under ~/.yolobox/tmp/,
// dereferencing all symlinks so that every entry is a regular file or directory.
// Returns the path to the staged copy of the directory.
func stageDirResolvingSymlinks(src string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	tmpBase := filepath.Join(home, ".yolobox", "tmp")
	if err := os.MkdirAll(tmpBase, 0700); err != nil {
		return "", err
	}
	dst, err := os.MkdirTemp(tmpBase, "staged-*")
	if err != nil {
		return "", err
	}
	if err := copyDirDereferenced(src, dst); err != nil {
		os.RemoveAll(dst)
		return "", err
	}
	return dst, nil
}

// copyDirDereferenced recursively copies src into dst, following all symlinks.
// Broken symlinks are silently skipped.
func copyDirDereferenced(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())

		// Use os.Stat (follows symlinks) to get the real type
		info, err := os.Stat(srcPath)
		if err != nil {
			continue // skip broken symlinks
		}
		if info.IsDir() {
			if err := copyDirDereferenced(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			data, err := os.ReadFile(srcPath)
			if err != nil {
				continue // skip unreadable files
			}
			if err := os.WriteFile(dstPath, data, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

func prepareFileMountDir(files map[string]string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "yolobox-mounts-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir for file mounts: %w", err)
	}

	for srcPath, destName := range files {
		data, err := os.ReadFile(srcPath)
		if err != nil {
			continue // Skip files that can't be read
		}
		destPath := filepath.Join(tmpDir, destName)
		// Create subdirectories if needed
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			continue
		}
		if err := os.WriteFile(destPath, data, 0644); err != nil {
			continue
		}
	}

	return tmpDir, nil
}

// findDockerSocket returns the Docker socket path to use as a volume mount source.
// On macOS, Docker always runs inside a VM (Docker Desktop or Colima), and the
// socket inside the VM is at /var/run/docker.sock regardless of the host-side path.
// On Linux, Docker runs natively so we return the actual host socket path.
func findDockerSocket() (string, error) {
	const vmInternalSocket = "/var/run/docker.sock"

	// Check DOCKER_HOST env var
	if dh := os.Getenv("DOCKER_HOST"); dh != "" {
		if strings.HasPrefix(dh, "unix://") {
			sock := strings.TrimPrefix(dh, "unix://")
			if _, err := os.Stat(sock); err == nil {
				if runtime.GOOS == "darwin" {
					return vmInternalSocket, nil
				}
				return sock, nil
			}
		}
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		"/var/run/docker.sock",                                   // Standard path (Linux, or macOS if symlinked)
		filepath.Join(home, ".docker", "run", "docker.sock"),     // Docker Desktop macOS
		filepath.Join(home, ".colima", "default", "docker.sock"), // Colima macOS
	}

	for _, sock := range candidates {
		if _, err := os.Stat(sock); err == nil {
			if runtime.GOOS == "darwin" {
				// On macOS, the mount source is resolved inside the Docker VM,
				// not on the macOS host. The socket inside any Docker VM is
				// always at /var/run/docker.sock.
				return vmInternalSocket, nil
			}
			return sock, nil
		}
	}

	return "", fmt.Errorf("Docker socket not found. Is Docker running?")
}

// findSSHAgentSocket returns the SSH agent socket path to use as a volume mount source.
// On Linux, SSH_AUTH_SOCK works directly. On macOS, the host's SSH_AUTH_SOCK path
// doesn't exist inside the Docker VM, so we need the VM-internal path instead.
func findSSHAgentSocket() (string, error) {
	if runtime.GOOS != "darwin" {
		// Linux: SSH_AUTH_SOCK works as-is
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return "", fmt.Errorf("SSH_AUTH_SOCK not set")
		}
		return sock, nil
	}

	// macOS: the host SSH_AUTH_SOCK path doesn't exist inside the Docker VM.
	// We need to find the VM-internal socket path.
	home, _ := os.UserHomeDir()

	// Detect Docker Desktop vs Colima by checking socket paths
	dockerDesktopSock := filepath.Join(home, ".docker", "run", "docker.sock")
	if _, err := os.Stat(dockerDesktopSock); err == nil {
		// Docker Desktop: well-known VM-internal path for SSH agent
		return "/run/host-services/ssh-auth.sock", nil
	}

	colimaSock := filepath.Join(home, ".colima", "default", "docker.sock")
	if _, err := os.Stat(colimaSock); err == nil {
		// Colima: query the VM for its SSH_AUTH_SOCK
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "colima", "ssh", "--", "printenv", "SSH_AUTH_SOCK")
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("SSH agent forwarding requires Colima's forwardAgent: true.\nEdit ~/.colima/default/colima.yaml, set forwardAgent: true, then: colima stop && colima start")
		}
		sock := strings.TrimSpace(string(out))
		if sock == "" {
			return "", fmt.Errorf("SSH agent forwarding requires Colima's forwardAgent: true.\nEdit ~/.colima/default/colima.yaml, set forwardAgent: true, then: colima stop && colima start")
		}
		return sock, nil
	}

	// Fallback: check if /var/run/docker.sock exists (symlinked or OrbStack)
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		// Try Docker Desktop path as a reasonable default
		return "/run/host-services/ssh-auth.sock", nil
	}

	return "", fmt.Errorf("could not determine SSH agent socket path for macOS Docker VM")
}

// ensureDockerNetwork creates the yolobox-net Docker network if it doesn't exist.
func ensureDockerNetwork(runtimeName string, networkName string) error {
	runtimePath, err := resolveRuntime(runtimeName)
	if err != nil {
		return err
	}

	cmd := exec.Command(runtimePath, "network", "create", networkName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Ignore "already exists" errors
		if strings.Contains(string(output), "already exists") {
			return nil
		}
		return fmt.Errorf("failed to create Docker network %q: %s", networkName, strings.TrimSpace(string(output)))
	}
	return nil
}

func appendRunFlag(args []string, flagName, value string) []string {
	if value == "" {
		return args
	}
	return append(args, "--"+flagName, value)
}

func buildRunArgs(cfg Config, projectDir string, command []string, interactive bool) ([]string, []string, error) {
	absProject, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, nil, err
	}

	// cleanupPaths collects temp files/dirs created during arg building
	// that should be removed after the container exits
	var cleanupPaths []string

	// Check if we're using Apple container (doesn't support file mounts)
	appleContainer := isAppleContainer(cfg.Runtime)

	args := []string{"run", "--rm"}

	// Add -it if explicitly interactive, or if stdin/stdout are both terminals
	// This allows "yolobox run claude" to work interactively while still
	// supporting piping (e.g., "echo foo | yolobox run cat")
	isTTY := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	if interactive || isTTY {
		args = append(args, "-it")
	}

	args = append(args, "-w", absProject)
	args = append(args, "-e", "YOLOBOX=1")
	args = append(args, "-e", "YOLOBOX_PROJECT_PATH="+absProject)
	if cfg.NoYolo {
		args = append(args, "-e", "NO_YOLO=1")
	}
	if termEnv := os.Getenv("TERM"); termEnv != "" {
		args = append(args, "-e", "TERM="+termEnv)
	}
	if lang := os.Getenv("LANG"); lang != "" {
		args = append(args, "-e", "LANG="+lang)
	}
	if tz := detectTimezone(); tz != "" {
		args = append(args, "-e", "TZ="+tz)
	}

	// Auto-passthrough common API keys
	for _, key := range autoPassthroughEnvVars {
		if val := os.Getenv(key); val != "" {
			args = append(args, "-e", key+"="+val)
		}
	}

	// Forward GitHub CLI token (extracted from keychain/credential store)
	if cfg.GhToken {
		if token := getGhToken(); token != "" {
			args = append(args, "-e", "GH_TOKEN="+token)
		}
	}

	// User-specified env vars
	for _, env := range cfg.Env {
		args = append(args, "-e", env)
	}

	// Project mount at its real host path (for session continuity)
	// A symlink /workspace -> real path is created by the entrypoint
	projectMount := absProject + ":" + absProject
	if cfg.ReadonlyProject {
		projectMount += ":ro"
		// Create a writable output directory
		if cfg.Scratch {
			args = append(args, "-v", "/output") // anonymous volume, deleted with container
		} else {
			args = append(args, "-v", "yolobox-output:/output")
		}
	}
	args = append(args, "-v", projectMount)

	// Named volumes for persistence (skip if --scratch)
	if !cfg.Scratch {
		args = append(args, "-v", "yolobox-home:/home/yolo")
		args = append(args, "-v", "yolobox-cache:/var/cache")
	}

	// For Apple container, we need to collect files and mount via a temp directory
	// (Apple container only supports directory mounts, not file mounts)
	var appleContainerFiles map[string]string
	if appleContainer {
		appleContainerFiles = make(map[string]string)
	}

	// Mount Claude config from host to staging area (copied to /home/yolo by entrypoint)
	if cfg.ClaudeConfig {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, err
		}
		claudeConfigDir := filepath.Join(home, ".claude")
		if _, err := os.Stat(claudeConfigDir); err == nil {
			mountSrc := claudeConfigDir
			if dirContainsSymlinks(claudeConfigDir) {
				staged, err := stageDirResolvingSymlinks(claudeConfigDir)
				if err != nil {
					warn("Failed to resolve symlinks in %s: %s", claudeConfigDir, err)
				} else {
					mountSrc = staged
					cleanupPaths = append(cleanupPaths, staged)
				}
			}
			args = append(args, "-v", mountSrc+":/host-claude/.claude:ro")
		}
		claudeConfigFile := filepath.Join(home, ".claude.json")
		if _, err := os.Stat(claudeConfigFile); err == nil {
			// Preprocess to remove installMethod (host install method doesn't apply in container)
			if processedPath := preprocessClaudeConfig(claudeConfigFile); processedPath != "" {
				cleanupPaths = append(cleanupPaths, processedPath)
				if appleContainer {
					appleContainerFiles[processedPath] = "claude/.claude.json"
				} else {
					args = append(args, "-v", processedPath+":/host-claude/.claude.json:ro")
				}
			}
		}
		// On macOS, extract OAuth credentials from Keychain and mount as .credentials.json
		// Write to unique temp file in ~/.yolobox/tmp/ (unique per invocation to
		// avoid conflicts when multiple yolobox instances run concurrently)
		if creds := getClaudeCredentials(); creds != "" {
			tmpDir := filepath.Join(home, ".yolobox", "tmp")
			os.MkdirAll(tmpDir, 0700)
			f, err := os.CreateTemp(tmpDir, "claude-credentials-*.json")
			if err == nil {
				if _, writeErr := f.Write([]byte(creds)); writeErr == nil {
					f.Close()
					os.Chmod(f.Name(), 0600)
					credsPath := f.Name()
					cleanupPaths = append(cleanupPaths, credsPath)
					if appleContainer {
						appleContainerFiles[credsPath] = "claude/.credentials.json"
					} else {
						args = append(args, "-v", credsPath+":/host-claude/.credentials.json:ro")
					}
				} else {
					f.Close()
					os.Remove(f.Name())
				}
			}
		}
	}

	// Mount Gemini config from host to staging area (copied to /home/yolo by entrypoint)
	if cfg.GeminiConfig {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, err
		}
		geminiConfigDir := filepath.Join(home, ".gemini")
		if _, err := os.Stat(geminiConfigDir); err == nil {
			mountSrc := geminiConfigDir
			if dirContainsSymlinks(geminiConfigDir) {
				staged, err := stageDirResolvingSymlinks(geminiConfigDir)
				if err != nil {
					warn("Failed to resolve symlinks in %s: %s", geminiConfigDir, err)
				} else {
					mountSrc = staged
					cleanupPaths = append(cleanupPaths, staged)
				}
			}
			args = append(args, "-v", mountSrc+":/host-gemini/.gemini:ro")
		}
	}

	// Mount git config from host to staging area (copied to /home/yolo by entrypoint)
	if cfg.GitConfig {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, err
		}
		gitConfigFile := filepath.Join(home, ".gitconfig")
		if _, err := os.Stat(gitConfigFile); err == nil {
			if appleContainer {
				appleContainerFiles[gitConfigFile] = "git/.gitconfig"
			} else {
				args = append(args, "-v", gitConfigFile+":/host-git/.gitconfig:ro")
			}
		}
	}

	// Mount global agent instruction files from host to staging area (copied by entrypoint)
	// These are the global/user-level instruction files, not project-level ones
	if cfg.CopyAgentInstructions {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, err
		}
		// Claude: ~/.claude/CLAUDE.md
		claudeMd := filepath.Join(home, ".claude", "CLAUDE.md")
		if _, err := os.Stat(claudeMd); err == nil {
			if appleContainer {
				appleContainerFiles[claudeMd] = "agent-instructions/claude/CLAUDE.md"
			} else {
				args = append(args, "-v", claudeMd+":/host-agent-instructions/claude/CLAUDE.md:ro")
			}
		}
		// Gemini: ~/.gemini/GEMINI.md
		geminiMd := filepath.Join(home, ".gemini", "GEMINI.md")
		if _, err := os.Stat(geminiMd); err == nil {
			if appleContainer {
				appleContainerFiles[geminiMd] = "agent-instructions/gemini/GEMINI.md"
			} else {
				args = append(args, "-v", geminiMd+":/host-agent-instructions/gemini/GEMINI.md:ro")
			}
		}
		// Codex: ~/.codex/AGENTS.md
		codexMd := filepath.Join(home, ".codex", "AGENTS.md")
		if _, err := os.Stat(codexMd); err == nil {
			if appleContainer {
				appleContainerFiles[codexMd] = "agent-instructions/codex/AGENTS.md"
			} else {
				args = append(args, "-v", codexMd+":/host-agent-instructions/codex/AGENTS.md:ro")
			}
		}
		// Copilot: ~/.copilot/agents/ directory (this is already a directory, works with Apple container)
		copilotAgents := filepath.Join(home, ".copilot", "agents")
		if info, err := os.Stat(copilotAgents); err == nil && info.IsDir() {
			mountSrc := copilotAgents
			if dirContainsSymlinks(copilotAgents) {
				staged, err := stageDirResolvingSymlinks(copilotAgents)
				if err != nil {
					warn("Failed to resolve symlinks in %s: %s", copilotAgents, err)
				} else {
					mountSrc = staged
					cleanupPaths = append(cleanupPaths, staged)
				}
			}
			args = append(args, "-v", mountSrc+":/host-agent-instructions/copilot/agents:ro")
		}
	}

	// For Apple container: create temp dir with collected files and mount it
	if appleContainer && len(appleContainerFiles) > 0 {
		tmpDir, err := prepareFileMountDir(appleContainerFiles)
		if err != nil {
			return nil, nil, err
		}
		cleanupPaths = append(cleanupPaths, tmpDir)
		// Mount the temp dir; entrypoint will need to handle the different paths
		args = append(args, "-v", tmpDir+":/host-files:ro")
		args = append(args, "-e", "YOLOBOX_HOST_FILES=/host-files")
	}

	// Extra mounts
	for _, mount := range cfg.Mounts {
		resolved, err := resolveMount(mount, absProject)
		if err != nil {
			return nil, nil, err
		}
		args = append(args, "-v", resolved)
	}

	// Resource & security controls
	args = appendRunFlag(args, "cpus", cfg.CPUs)
	args = appendRunFlag(args, "memory", cfg.Memory)
	args = appendRunFlag(args, "shm-size", cfg.ShmSize)
	args = appendRunFlag(args, "gpus", cfg.GPUs)
	for _, d := range cfg.Devices {
		args = append(args, "--device", d)
	}
	for _, c := range cfg.CapAdd {
		args = append(args, "--cap-add", c)
	}
	for _, c := range cfg.CapDrop {
		args = append(args, "--cap-drop", c)
	}

	// SSH agent forwarding
	if cfg.SSHAgent {
		if appleContainer {
			// Apple container uses --ssh flag instead of socket mounts
			args = append(args, "--ssh")
		} else {
			sock, err := findSSHAgentSocket()
			if err != nil {
				warn("%s", err)
			} else {
				args = append(args, "-v", sock+":/ssh-agent")
				args = append(args, "-e", "SSH_AUTH_SOCK=/ssh-agent")
			}
		}
	}

	// Docker socket forwarding
	if cfg.Docker {
		sock, err := findDockerSocket()
		if err != nil {
			return nil, nil, err
		}
		args = append(args, "-v", sock+":/var/run/docker.sock")
		// Default to yolobox-net if no explicit network is set
		if cfg.Network == "" {
			cfg.Network = "yolobox-net"
		}
		args = append(args, "-e", "YOLOBOX_NETWORK="+cfg.Network)
	}

	// Network configuration
	if cfg.Pod != "" {
		args = append(args, "--pod", cfg.Pod)
	} else {
		if cfg.NoNetwork {
			args = append(args, "--network", "none")
		} else if cfg.Network != "" {
			args = append(args, "--network", cfg.Network)
		}
	}

	if len(cfg.RuntimeArgs) > 0 {
		args = append(args, cfg.RuntimeArgs...)
	}

	args = append(args, cfg.Image)
	args = append(args, command...)
	return args, cleanupPaths, nil
}

func resolveMount(mount string, projectDir string) (string, error) {
	parts := strings.SplitN(mount, ":", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid mount %q; expected src:dst", mount)
	}
	src := parts[0]
	dst := parts[1]
	var opts string
	if len(parts) == 3 {
		opts = parts[2]
	}

	resolved, err := resolvePath(src, projectDir)
	if err != nil {
		return "", err
	}
	if opts != "" {
		return fmt.Sprintf("%s:%s:%s", resolved, dst, opts), nil
	}
	return fmt.Sprintf("%s:%s", resolved, dst), nil
}

func resolvePath(path string, projectDir string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
	}
	if strings.HasPrefix(path, ".") || strings.HasPrefix(path, "/") {
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectDir, path)
		}
		return filepath.Clean(path), nil
	}
	return path, nil
}

func resolvedRuntimeName(name string) string {
	if name == "" {
		return "auto"
	}
	if name == "colima" {
		return "docker"
	}
	return name
}

func resolveRuntime(name string) (string, error) {
	if name == "" {
		if path, err := exec.LookPath("docker"); err == nil {
			return path, nil
		}
		if path, err := exec.LookPath("podman"); err == nil {
			return path, nil
		}
		if path, err := exec.LookPath("container"); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("no container runtime found. Install docker, podman, or Apple container and try again")
	}
	if name == "colima" {
		name = "docker"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("runtime %q not found in PATH", name)
	}
	return path, nil
}

func execRuntime(runtime string, args []string) error {
	runtimePath, err := resolveRuntime(runtime)
	if err != nil {
		return err
	}
	return execCommand(runtimePath, args)
}

// getGhToken extracts the GitHub CLI token from the host's credential store
// Returns empty string if gh is not installed or not logged in
func getGhToken() string {
	cmd := exec.Command("gh", "auth", "token")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// getClaudeCredentials extracts Claude Code OAuth credentials from macOS Keychain
// Returns empty string on non-macOS or if not logged in
func getClaudeCredentials() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	cmd := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// preprocessClaudeConfig reads ~/.claude.json, removes installMethod (which is
// host-specific and causes issues in the container), and writes to a temp file.
// Returns the temp file path, or empty string on error.
func preprocessClaudeConfig(srcPath string) string {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return ""
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return ""
	}

	// Remove installMethod - let Claude detect it fresh in the container
	// The host's installMethod (e.g., "native") doesn't apply inside the container
	delete(config, "installMethod")

	processed, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return ""
	}

	// Write to unique temp file in ~/.yolobox/tmp/ (unique per invocation to
	// avoid conflicts when multiple yolobox instances run concurrently)
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	tmpDir := filepath.Join(home, ".yolobox", "tmp")
	os.MkdirAll(tmpDir, 0700)
	f, err := os.CreateTemp(tmpDir, "claude-config-*.json")
	if err != nil {
		return ""
	}
	defer f.Close()

	if _, err := f.Write(processed); err != nil {
		os.Remove(f.Name())
		return ""
	}

	return f.Name()
}

// detectTimezone returns the host's IANA timezone (e.g., "America/New_York").
// Returns empty string if detection fails.
func detectTimezone() string {
	// Check TZ env var first
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}

	// Try reading /etc/localtime symlink (works on macOS and Linux)
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}

	// Extract timezone from path like /var/db/timezone/zoneinfo/America/New_York
	// or /usr/share/zoneinfo/America/New_York
	const marker = "zoneinfo/"
	if idx := strings.LastIndex(target, marker); idx != -1 {
		return target[idx+len(marker):]
	}

	return ""
}

// checkDockerMemory warns if Docker has less than 4GB RAM available
func checkDockerMemory(runtime string) {
	runtimePath, err := resolveRuntime(runtime)
	if err != nil {
		return
	}

	// Skip memory check for Apple container (uses native VM with dynamic memory)
	if strings.HasSuffix(runtimePath, "/container") {
		return
	}

	cmd := exec.Command(runtimePath, "info", "--format", "{{.MemTotal}}")
	output, err := cmd.Output()
	if err != nil {
		return
	}

	memBytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return
	}

	memGB := float64(memBytes) / (1024 * 1024 * 1024)
	if memGB < 3.5 {
		warn("Docker has only %.1fGB RAM. Claude Code may get OOM killed.", memGB)
		warn("Increase Docker/Colima memory to 4GB+ for best results.")
	}
}

func execCommand(bin string, args []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// GitHub release info
type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func upgradeYolobox() error {
	info("Checking for updates...")

	// Get latest release from GitHub
	resp, err := http.Get("https://api.github.com/repos/finbarr/yolobox/releases/latest")
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to check for updates: HTTP %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("failed to parse release info: %w", err)
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	currentVersion := strings.TrimPrefix(Version, "v")

	if latestVersion == currentVersion {
		success("Already at latest version (%s)", Version)
	} else {
		info("New version available: %s (current: %s)", latestVersion, currentVersion)

		// Find the right binary for this platform
		binaryName := fmt.Sprintf("yolobox-%s-%s", runtime.GOOS, runtime.GOARCH)
		var downloadURL string
		for _, asset := range release.Assets {
			if asset.Name == binaryName {
				downloadURL = asset.BrowserDownloadURL
				break
			}
		}

		if downloadURL == "" {
			return fmt.Errorf("no binary available for %s/%s", runtime.GOOS, runtime.GOARCH)
		}

		// Download new binary
		info("Downloading %s...", binaryName)
		resp, err := http.Get(downloadURL)
		if err != nil {
			return fmt.Errorf("failed to download: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return fmt.Errorf("failed to download: HTTP %d", resp.StatusCode)
		}

		// Get current executable path
		execPath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("failed to get executable path: %w", err)
		}
		execPath, err = filepath.EvalSymlinks(execPath)
		if err != nil {
			return fmt.Errorf("failed to resolve executable path: %w", err)
		}

		// Write to temp file first
		tmpFile, err := os.CreateTemp(filepath.Dir(execPath), "yolobox-upgrade-*")
		if err != nil {
			return fmt.Errorf("failed to create temp file: %w", err)
		}
		tmpPath := tmpFile.Name()

		_, err = io.Copy(tmpFile, resp.Body)
		tmpFile.Close()
		if err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("failed to write binary: %w", err)
		}

		// Make executable
		if err := os.Chmod(tmpPath, 0755); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("failed to chmod: %w", err)
		}

		// Replace current binary
		if err := os.Rename(tmpPath, execPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("failed to replace binary: %w", err)
		}

		success("Binary upgraded to %s", latestVersion)
	}

	// Also pull latest Docker image
	info("Pulling latest Docker image...")
	cfg := defaultConfig()
	runtime, err := resolveRuntime(cfg.Runtime)
	if err != nil {
		return err
	}
	if err := execCommand(runtime, []string{"pull", cfg.Image}); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	// Clean up dangling images to reclaim disk space (old yolobox images
	// become dangling after pull replaces the :latest tag)
	info("Cleaning up old images...")
	_ = execCommand(runtime, []string{"image", "prune", "-f"})

	success("Upgrade complete!")
	return nil
}

// Output helpers with colors
func success(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, colorGreen+"✓ "+colorReset+format+"\n", args...)
}

func info(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, colorBlue+"→ "+colorReset+format+"\n", args...)
}

func warn(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, colorYellow+"⚠ "+colorReset+format+"\n", args...)
}

func errorf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, colorRed+"✗ "+colorReset+format+"\n", args...)
}
