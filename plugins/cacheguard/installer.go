//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"plugin"
	"runtime"
	"strings"
	"time"
)

type ConfigPluginEntry struct {
	Name    string         `json:"name"`
	Type    string         `json:"type,omitempty"`
	Path    string         `json:"path"`
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config,omitempty"`
}

func main() {
	var (
		action          = flag.String("action", "install", "Action to perform: install, uninstall, verify")
		bifrostDir      = flag.String("bifrost-dir", "", "Path to the Bifrost source checkout")
		configPath      = flag.String("config-path", "", "Path to Bifrost config.json for activation/deactivation")
		pluginOutputDir = flag.String("plugin-output-dir", "", "Custom target directory for plugin .so (default: <bifrost-dir>/plugins/cacheguard/build)")
		dryRun          = flag.Bool("dry-run", false, "Preview actions without modifying any files")
	)
	flag.Parse()

	if *bifrostDir == "" {
		fmt.Println("ERROR: -bifrost-dir is required (e.g., make install BIFROST_DIR=/path/to/Bifrost)")
		os.Exit(1)
	}

	absBifrostDir, err := filepath.Abs(*bifrostDir)
	if err != nil {
		fmt.Printf("ERROR: Cannot resolve BIFROST_DIR: %v\n", err)
		os.Exit(1)
	}

	switch *action {
	case "install":
		runInstall(absBifrostDir, *configPath, *pluginOutputDir, *dryRun)
	case "uninstall":
		runUninstall(absBifrostDir, *configPath, *pluginOutputDir, *dryRun)
	case "verify":
		runVerify(absBifrostDir, *configPath, *pluginOutputDir)
	default:
		fmt.Printf("ERROR: Unknown action %q. Use install, uninstall, or verify.\n", *action)
		os.Exit(1)
	}
}

func runInstall(bifrostDir, configPath, pluginOutputDir string, dryRun bool) {
	fmt.Println("================================================================================")
	fmt.Println("              STILLMARK (CACHEGUARD) — ONE-COMMAND INSTALLER                    ")
	fmt.Println("================================================================================")

	if dryRun {
		fmt.Println("[DRY RUN MODE] Simulating installation steps without writing files.")
	}

	// 1. Check OS and Architecture
	osName := runtime.GOOS
	archName := runtime.GOARCH
	fmt.Printf("✓ Detected Platform: %s/%s\n", osName, archName)
	if osName == "windows" {
		fmt.Println("FAIL: Windows is not supported by Go's standard plugin package (.so buildmode).")
		fmt.Println("      StillMark requires Linux or macOS.")
		os.Exit(1)
	}

	// 2. Check Go Toolchain
	goVerOutput, err := exec.Command("go", "version").CombinedOutput()
	if err != nil {
		fmt.Printf("FAIL: 'go' command not found in PATH: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Found Go Toolchain: %s", string(goVerOutput))

	// Check CGO compiler (gcc or clang)
	ccEnv := os.Getenv("CC")
	if ccEnv == "" {
		ccEnv = "gcc"
	}
	if _, err := exec.LookPath(ccEnv); err != nil {
		fmt.Printf("FAIL: CGO compiler '%s' not found. Please install gcc/clang to build Go plugins.\n", ccEnv)
		os.Exit(1)
	}
	fmt.Printf("✓ Found CGO Compiler: %s\n", ccEnv)

	// 3. Verify Bifrost Directory and symlink safety
	if err := verifyBifrostDir(bifrostDir); err != nil {
		fmt.Printf("FAIL: Incompatible or invalid Bifrost directory: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Verified Bifrost source checkout at: %s\n", bifrostDir)

	// 4. Determine Plugin Output Directory and File
	pluginInstallDir := pluginOutputDir
	if pluginInstallDir == "" {
		pluginInstallDir = filepath.Join(bifrostDir, "plugins", "cacheguard", "build")
	}
	pluginOutputFile := filepath.Join(pluginInstallDir, "cacheguard.so")

	// Symlink & traversal check on output path
	checkBase := bifrostDir
	if pluginOutputDir != "" {
		checkBase = filepath.Dir(pluginInstallDir)
	}
	if err := validatePathWithin(checkBase, pluginOutputFile); err != nil {
		fmt.Printf("FAIL: Unsafe plugin output path: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("  • Target Plugin Path: %s\n", pluginOutputFile)

	if dryRun {
		fmt.Printf("[DRY RUN] Would build cacheguard.so and place at %s\n", pluginOutputFile)
	} else {
		if err := os.MkdirAll(pluginInstallDir, 0755); err != nil {
			fmt.Printf("FAIL: Cannot create target directory %s: %v\n", pluginInstallDir, err)
			os.Exit(1)
		}

		// Find source directory of cacheguard plugin
		sourceDir, err := os.Getwd()
		if err != nil {
			fmt.Printf("FAIL: Cannot determine current working directory: %v\n", err)
			os.Exit(1)
		}

		// Build to a temporary file in the same directory first to avoid corrupting/overwriting
		// any existing valid plugin before build and verification succeed
		tmpSO := filepath.Join(pluginInstallDir, fmt.Sprintf(".cacheguard.%d.tmp.so", time.Now().UnixNano()))
		defer func() { _ = os.Remove(tmpSO) }()

		fmt.Println("Building plugin shared object (.so)...")
		buildCmd := exec.Command("go", "build",
			"-buildmode=plugin",
			"-o", tmpSO,
			"main.go", "engine.go",
		)
		buildCmd.Dir = sourceDir
		buildCmd.Env = append(os.Environ(),
			"CGO_ENABLED=1",
			"GOWORK="+filepath.Join(bifrostDir, "go.work"),
		)

		if out, err := buildCmd.CombinedOutput(); err != nil {
			fmt.Printf("FAIL: Build failed:\n%s\nError: %v\n", string(out), err)
			os.Exit(1)
		}

		// 5. Verify the compiled plugin artifact before replacing existing binary
		fmt.Println("Verifying compiled plugin binary compatibility...")
		if err := verifyPluginBinary(tmpSO); err != nil {
			fmt.Printf("FAIL: Compiled plugin verification failed: %v\n", err)
			os.Exit(1)
		}

		// Atomic rename of verified binary
		if err := os.Rename(tmpSO, pluginOutputFile); err != nil {
			fmt.Printf("FAIL: Failed to install plugin binary to %s: %v\n", pluginOutputFile, err)
			os.Exit(1)
		}

		fmt.Printf("✓ Compiled and verified plugin shared object successfully: %s\n", pluginOutputFile)
		fmt.Println("✓ Verified plugin binary symbols: GetName, Init, Cleanup, and HTTP/LLM hooks present.")
		fmt.Println("  (Note: exact runtime compatibility requires gateway compiled with matching Go workspace toolchain)")
	}

	// 6. Handle Activation (if config-path provided)
	if configPath == "" {
		fmt.Println("\n--------------------------------------------------------------------------------")
		fmt.Println("STATUS: INSTALLED (NOT YET CONFIGURED IN GATEWAY)")
		fmt.Println("--------------------------------------------------------------------------------")
		fmt.Println("The plugin artifact has been compiled and installed on disk.")
		fmt.Println("To configure StillMark in your Bifrost gateway, add this to your config.json:")
		printConfigSnippet(pluginOutputFile)
		fmt.Println("\nOr configure automatically in one command by providing CONFIG_PATH:")
		fmt.Printf("  make install BIFROST_DIR=%s CONFIG_PATH=/path/to/config.json\n", bifrostDir)
	} else {
		absConfigPath, err := filepath.Abs(configPath)
		if err != nil {
			fmt.Printf("FAIL: Cannot resolve CONFIG_PATH %s: %v\n", configPath, err)
			os.Exit(1)
		}

		if err := validatePathWithin(filepath.Dir(absConfigPath), absConfigPath); err != nil {
			fmt.Printf("FAIL: Unsafe configuration path: %v\n", err)
			os.Exit(1)
		}

		if dryRun {
			fmt.Printf("[DRY RUN] Would update %s to register plugin with path %s\n", absConfigPath, pluginOutputFile)
			fmt.Println("\n--------------------------------------------------------------------------------")
			fmt.Println("STATUS: DRY RUN COMPLETE (NO CHANGES MADE)")
			fmt.Println("--------------------------------------------------------------------------------")
			return
		}

		if err := activatePluginInConfig(absConfigPath, pluginOutputFile); err != nil {
			fmt.Printf("FAIL: Failed to activate plugin in %s: %v\n", absConfigPath, err)
			os.Exit(1)
		}
		fmt.Printf("✓ Successfully configured StillMark in: %s\n", absConfigPath)

		fmt.Println("\n--------------------------------------------------------------------------------")
		fmt.Println("STATUS: INSTALLED & CONFIGURED")
		fmt.Println("--------------------------------------------------------------------------------")
		fmt.Printf("Installed Binary: %s\n", pluginOutputFile)
		fmt.Printf("Config File:      %s\n", absConfigPath)
		fmt.Println("Loaded State:     Restart your Bifrost gateway instance to load the updated plugin.")
	}
	fmt.Println("================================================================================")
}

func runUninstall(bifrostDir, configPath, pluginOutputDir string, dryRun bool) {
	fmt.Println("================================================================================")
	fmt.Println("             STILLMARK (CACHEGUARD) — UNINSTALL WORKFLOW                        ")
	fmt.Println("================================================================================")

	if dryRun {
		fmt.Println("[DRY RUN MODE] Simulating uninstall without deleting or modifying files.")
	}

	// Safety check: ensure bifrostDir is not empty or root directory
	if bifrostDir == "/" || bifrostDir == "." || len(bifrostDir) < 5 {
		fmt.Printf("FAIL: Refusing to operate on unsafe BIFROST_DIR path: %q\n", bifrostDir)
		os.Exit(1)
	}

	pluginInstallDir := pluginOutputDir
	if pluginInstallDir == "" {
		pluginInstallDir = filepath.Join(bifrostDir, "plugins", "cacheguard", "build")
	}
	pluginOutputFile := filepath.Join(pluginInstallDir, "cacheguard.so")
	pluginBuildDir := pluginInstallDir

	// Symlink & traversal check
	checkBase := bifrostDir
	if pluginOutputDir != "" {
		checkBase = filepath.Dir(pluginInstallDir)
	}
	if err := validatePathWithin(checkBase, pluginOutputFile); err != nil {
		fmt.Printf("FAIL: Unsafe plugin output path: %v\n", err)
		os.Exit(1)
	}

	// 1. Transactional order: Deactivate in configuration FIRST
	// Never delete the binary first and leave an enabled plugin pointing to a missing file!
	if configPath != "" {
		absConfigPath, err := filepath.Abs(configPath)
		if err != nil {
			fmt.Printf("FAIL: Cannot resolve CONFIG_PATH %s: %v\n", configPath, err)
			os.Exit(1)
		}
		if err := validatePathWithin(filepath.Dir(absConfigPath), absConfigPath); err != nil {
			fmt.Printf("FAIL: Unsafe configuration path: %v\n", err)
			os.Exit(1)
		}

		if dryRun {
			fmt.Printf("[DRY RUN] Would remove cacheguard entry from %s\n", absConfigPath)
		} else {
			if err := deactivatePluginInConfig(absConfigPath); err != nil {
				fmt.Printf("FAIL: Failed to deactivate plugin in configuration %s: %v\n", absConfigPath, err)
				fmt.Println("ABORTING UNINSTALL: Plugin binary was preserved to prevent corrupting gateway configuration.")
				os.Exit(1)
			}
			fmt.Printf("✓ Removed StillMark entry from configuration: %s\n", absConfigPath)
		}
	}

	// 2. Remove plugin binary only AFTER configuration update succeeded
	if fi, err := os.Lstat(pluginOutputFile); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			fmt.Printf("FAIL: Refusing to delete symlinked plugin path %s\n", pluginOutputFile)
			os.Exit(1)
		}
		if dryRun {
			fmt.Printf("[DRY RUN] Would remove plugin artifact: %s\n", pluginOutputFile)
		} else {
			if err := os.Remove(pluginOutputFile); err != nil {
				fmt.Printf("WARNING: Failed to remove %s: %v\n", pluginOutputFile, err)
			} else {
				fmt.Printf("✓ Removed StillMark plugin binary: %s\n", pluginOutputFile)
			}
			// Attempt to remove build dir if empty
			_ = os.Remove(pluginBuildDir)
		}
	} else {
		fmt.Printf("• Plugin binary not found at %s (already removed or not installed)\n", pluginOutputFile)
	}

	fmt.Println("\n--------------------------------------------------------------------------------")
	fmt.Println("STATUS: UNINSTALL COMPLETE")
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Println("StillMark artifacts removed. All unrelated Bifrost files and configurations preserved.")
	fmt.Println("================================================================================")
}

func runVerify(bifrostDir, configPath, pluginOutputDir string) {
	fmt.Println("================================================================================")
	fmt.Println("              STILLMARK (CACHEGUARD) — VERIFICATION WORKFLOW                    ")
	fmt.Println("================================================================================")
	pluginInstallDir := pluginOutputDir
	if pluginInstallDir == "" {
		pluginInstallDir = filepath.Join(bifrostDir, "plugins", "cacheguard", "build")
	}
	pluginOutputFile := filepath.Join(pluginInstallDir, "cacheguard.so")
	if fi, err := os.Lstat(pluginOutputFile); err != nil {
		fmt.Printf("• [Installed State] Plugin NOT installed at %s\n", pluginOutputFile)
	} else {
		if fi.Mode()&os.ModeSymlink != 0 {
			fmt.Printf("WARNING: Plugin at %s is a symlink!\n", pluginOutputFile)
		}
		if err := verifyPluginBinary(pluginOutputFile); err != nil {
			fmt.Printf("FAIL: Plugin symbol verification failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✓ [Installed State] Plugin binary at %s is present with valid exported symbols.\n", pluginOutputFile)
		fmt.Println("  (Note: plugin.Open verifies symbol interface signatures; runtime gateway loading requires matching workspace build)")
	}

	if configPath != "" {
		absConfigPath, err := filepath.Abs(configPath)
		if err == nil {
			raw, err := os.ReadFile(absConfigPath)
			if err == nil {
				var doc map[string]any
				if err := json.Unmarshal(raw, &doc); err == nil {
					if pluginsList, ok := doc["plugins"].([]any); ok {
						found := false
						for _, item := range pluginsList {
							if m, ok := item.(map[string]any); ok {
								if name, ok := m["name"].(string); ok && (name == "cacheguard" || name == "stillmark") {
									enabled, _ := m["enabled"].(bool)
									fmt.Printf("✓ [Configured State] Registered in %s (enabled: %v)\n", absConfigPath, enabled)
									found = true
									break
								}
							}
						}
						if !found {
							fmt.Printf("• [Configured State] Not registered in %s\n", absConfigPath)
						}
					}
				}
			}
		}
	}
	fmt.Println("• [Loaded State] Check running gateway logs (/health) to verify active runtime state.")
	fmt.Println("================================================================================")
}

func verifyBifrostDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("directory does not exist: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	// Symlink check
	evalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("failed to evaluate symlinks for %s: %w", dir, err)
	}
	if evalDir == "/" || len(evalDir) < 5 {
		return fmt.Errorf("refusing to operate on root directory: %s", evalDir)
	}

	// Check for Bifrost signature files
	goWorkPath := filepath.Join(dir, "go.work")
	if _, err := os.Stat(goWorkPath); err != nil {
		return fmt.Errorf("missing go.work at %s (is this a complete Bifrost workspace?)", dir)
	}

	coreSchemasPath := filepath.Join(dir, "core", "schemas")
	if _, err := os.Stat(coreSchemasPath); err != nil {
		return fmt.Errorf("missing core/schemas at %s", dir)
	}

	return nil
}

func validatePathWithin(baseDir, targetPath string) error {
	cleanBase, err := filepath.Abs(baseDir)
	if err != nil {
		return fmt.Errorf("invalid base directory: %w", err)
	}
	evalBase, err := filepath.EvalSymlinks(cleanBase)
	if err != nil {
		return fmt.Errorf("failed to resolve base directory symlinks: %w", err)
	}

	cleanTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return fmt.Errorf("invalid target path: %w", err)
	}

	// Reject any path that traverses or contains a symlink
	curr := cleanTarget
	for curr != "/" && curr != "." {
		if fi, err := os.Lstat(curr); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				evalCurr, _ := filepath.EvalSymlinks(curr)
				return fmt.Errorf("refusing to operate through symlink %s (resolves to %s)", curr, evalCurr)
			}
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			break
		}
		curr = parent
	}

	evalTarget, err := filepath.EvalSymlinks(filepath.Dir(cleanTarget))
	if err == nil {
		if !strings.HasPrefix(evalTarget, evalBase+string(filepath.Separator)) && evalTarget != evalBase {
			return fmt.Errorf("target %s resolves outside base %s (resolved to %s)", cleanTarget, evalBase, evalTarget)
		}
	}

	return nil
}

func verifyPluginBinary(soPath string) error {
	p, err := plugin.Open(soPath)
	if err != nil {
		return fmt.Errorf("plugin.Open failed: %w", err)
	}

	getNameSym, err := p.Lookup("GetName")
	if err != nil {
		return fmt.Errorf("missing required symbol 'GetName': %w", err)
	}
	getNameFn, ok := getNameSym.(func() string)
	if !ok {
		return fmt.Errorf("symbol 'GetName' does not have signature func() string")
	}
	name := getNameFn()
	if name != "cacheguard" && name != "stillmark" {
		return fmt.Errorf("unexpected plugin name: got %q, expected 'cacheguard' or 'stillmark'", name)
	}

	// Verify required hook symbols exist
	for _, symName := range []string{"Init", "Cleanup", "PreLLMHook", "PostLLMHook", "HTTPTransportPreHook", "HTTPTransportResponseHeadersHook"} {
		if _, err := p.Lookup(symName); err != nil {
			return fmt.Errorf("missing hook symbol %q: %w", symName, err)
		}
	}

	return nil
}

func createSafeBackup(configPath string, raw []byte, perm os.FileMode) (string, error) {
	// Ensure backup permissions are never more permissive than source, capped at 0600
	safePerm := perm & 0600
	if safePerm == 0 {
		safePerm = 0600
	}

	for attempt := 0; attempt < 1000; attempt++ {
		backupPath := fmt.Sprintf("%s.bak.%d_%d", configPath, time.Now().UnixNano(), attempt)
		f, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, safePerm)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", fmt.Errorf("failed to open backup file %s: %w", backupPath, err)
		}
		_, writeErr := f.Write(raw)
		closeErr := f.Close()
		if writeErr != nil {
			_ = os.Remove(backupPath)
			return "", fmt.Errorf("failed writing backup: %w", writeErr)
		}
		if closeErr != nil {
			_ = os.Remove(backupPath)
			return "", fmt.Errorf("failed closing backup: %w", closeErr)
		}
		return backupPath, nil
	}
	return "", fmt.Errorf("exhausted backup filename attempts for %s", configPath)
}

func writeConfigAtomically(configPath string, outBytes []byte, perm os.FileMode) error {
	// Validate JSON before writing
	var docCheck map[string]any
	if err := json.Unmarshal(outBytes, &docCheck); err != nil {
		return fmt.Errorf("refusing to write invalid JSON config: %w", err)
	}

	dir := filepath.Dir(configPath)
	tmpFile, err := os.CreateTemp(dir, ".config.*.tmp")
	if err != nil {
		return fmt.Errorf("failed creating temporary config file in %s: %w", dir, err)
	}
	tmpName := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	safePerm := perm & 0600
	if safePerm == 0 {
		safePerm = 0600
	}
	if err := tmpFile.Chmod(safePerm); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed setting permissions on temp config: %w", err)
	}

	if _, err := tmpFile.Write(outBytes); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed writing temp config: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed syncing temp config: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed closing temp config: %w", err)
	}

	if err := os.Rename(tmpName, configPath); err != nil {
		return fmt.Errorf("failed atomically replacing %s: %w", configPath, err)
	}

	cleanup = false
	return nil
}

func activatePluginInConfig(configPath, pluginPath string) error {
	fi, err := os.Stat(configPath)
	if err != nil {
		return fmt.Errorf("cannot stat config %s: %w", configPath, err)
	}
	origPerm := fi.Mode().Perm()

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("cannot read config: %w", err)
	}

	// Safe collision-free backup with preserved/secure permissions
	backupPath, err := createSafeBackup(configPath, raw, origPerm)
	if err != nil {
		return fmt.Errorf("failed to create config backup: %w", err)
	}
	fmt.Printf("✓ Created safe backup: %s\n", backupPath)

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("invalid config JSON: %w", err)
	}

	// Inspect plugins section safely
	pluginsRaw, exists := doc["plugins"]
	var pluginList []any
	if exists && pluginsRaw != nil {
		list, ok := pluginsRaw.([]any)
		if !ok {
			return fmt.Errorf("invalid config in %s: 'plugins' property is malformed (expected array, got %T)", configPath, pluginsRaw)
		}
		pluginList = list
	}

	// Idempotent search: check if cacheguard or stillmark already exists
	foundIndex := -1
	var existingConfig map[string]any
	for i, item := range pluginList {
		if m, ok := item.(map[string]any); ok {
			if name, ok := m["name"].(string); ok && (name == "cacheguard" || name == "stillmark") {
				foundIndex = i
				if cfgMap, ok := m["config"].(map[string]any); ok {
					existingConfig = cfgMap
				}
				break
			}
		}
	}

	// Preserve existing operator configuration options!
	mergedConfig := map[string]any{
		"enabled":            true,
		"diagnostic_logging": true,
		"severity_threshold": "info",
	}
	for k, v := range existingConfig {
		mergedConfig[k] = v
	}
	mergedConfig["enabled"] = true

	newEntry := map[string]any{
		"name":    "cacheguard",
		"type":    "shared_object",
		"path":    pluginPath,
		"enabled": true,
		"config":  mergedConfig,
	}

	if foundIndex >= 0 {
		pluginList[foundIndex] = newEntry
	} else {
		pluginList = append(pluginList, newEntry)
	}

	doc["plugins"] = pluginList

	outBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal updated config: %w", err)
	}

	return writeConfigAtomically(configPath, outBytes, origPerm)
}

func deactivatePluginInConfig(configPath string) error {
	fi, err := os.Stat(configPath)
	if err != nil {
		return fmt.Errorf("cannot stat config %s: %w", configPath, err)
	}
	origPerm := fi.Mode().Perm()

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("cannot read config: %w", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("invalid config JSON: %w", err)
	}

	pluginsRaw, exists := doc["plugins"]
	if !exists || pluginsRaw == nil {
		return nil
	}

	list, ok := pluginsRaw.([]any)
	if !ok {
		return fmt.Errorf("invalid config in %s: 'plugins' property is malformed (expected array, got %T)", configPath, pluginsRaw)
	}

	// Safe backup before modifying
	backupPath, err := createSafeBackup(configPath, raw, origPerm)
	if err != nil {
		return fmt.Errorf("failed to create config backup: %w", err)
	}
	fmt.Printf("✓ Created safe backup: %s\n", backupPath)

	filtered := make([]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if name, ok := m["name"].(string); ok && (name == "cacheguard" || name == "stillmark") {
				continue // exclude
			}
		}
		filtered = append(filtered, item)
	}

	doc["plugins"] = filtered

	outBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return writeConfigAtomically(configPath, outBytes, origPerm)
}

func printConfigSnippet(pluginPath string) {
	snippet := map[string]any{
		"plugins": []map[string]any{
			{
				"name":    "cacheguard",
				"type":    "shared_object",
				"path":    pluginPath,
				"enabled": true,
				"config": map[string]any{
					"enabled":            true,
					"diagnostic_logging": true,
					"severity_threshold": "info",
				},
			},
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("  ", "  ")
	_ = enc.Encode(snippet)
	fmt.Printf("  %s\n", strings.TrimSpace(buf.String()))
}
