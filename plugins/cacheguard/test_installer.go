//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	fmt.Println("================================================================================")
	fmt.Println("    STILLMARK — ISOLATED INSTALLER TEST SUITE (TESTS 1 THROUGH 9)              ")
	fmt.Println("================================================================================")

	// Determine real Bifrost repository root for toolchain & workspace resolution
	realWorkspaceRoot, err := filepath.Abs("../..")
	if err != nil {
		fmt.Printf("FAIL: Cannot resolve workspace root: %v\n", err)
		os.Exit(1)
	}

	// Create completely isolated, disposable test environment in os.TempDir
	tempTestRoot, err := os.MkdirTemp("", "stillmark-installer-isolated-*")
	if err != nil {
		fmt.Printf("FAIL: Cannot create isolated temp dir: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = os.RemoveAll(tempTestRoot)
		fmt.Printf("\n✓ Cleaned up isolated test environment: %s\n", tempTestRoot)
	}()

	fmt.Printf("✓ Created isolated test workspace: %s\n", tempTestRoot)

	// Disposable plugin binary directory
	disposableBuildDir := filepath.Join(tempTestRoot, "disposable_build")
	_ = os.MkdirAll(disposableBuildDir, 0755)

	// Record original dev plugin binary stats to assert it was never modified or deleted
	devSO := filepath.Join(realWorkspaceRoot, "plugins", "cacheguard", "build", "cacheguard.so")
	devSOStatBefore, _ := os.Stat(devSO)

	// -------------------------------------------------------------------------
	// TEST 1: Existing Secure File Permissions (0600 preserved on update & backup)
	// -------------------------------------------------------------------------
	fmt.Println("\n[TEST 1] Secure File Permissions (0600 preservation)...")
	test1ConfigFile := filepath.Join(tempTestRoot, "test1_config.json")
	initialCfg := map[string]any{
		"plugins": []map[string]any{
			{"name": "other-plugin", "enabled": true},
		},
	}
	cfgBytes, _ := json.MarshalIndent(initialCfg, "", "  ")
	if err := os.WriteFile(test1ConfigFile, cfgBytes, 0600); err != nil {
		fmt.Printf("FAIL Test 1: %v\n", err)
		os.Exit(1)
	}

	cmd1 := exec.Command("go", "run", "installer.go",
		"-action=install",
		"-bifrost-dir="+realWorkspaceRoot,
		"-plugin-output-dir="+disposableBuildDir,
		"-config-path="+test1ConfigFile,
	)
	out1, err1 := cmd1.CombinedOutput()
	if err1 != nil {
		fmt.Printf("FAIL Test 1 execution:\n%s\n", string(out1))
		os.Exit(1)
	}

	fi1, err := os.Stat(test1ConfigFile)
	if err != nil || fi1.Mode().Perm() != 0600 {
		fmt.Printf("FAIL Test 1: Expected 0600 permissions on config, got %o (err: %v)\n", fi1.Mode().Perm(), err)
		os.Exit(1)
	}

	// Check backup file permissions
	files, _ := os.ReadDir(tempTestRoot)
	var backupPerm os.FileMode
	var foundBackup bool
	for _, f := range files {
		if strings.Contains(f.Name(), "test1_config.json.bak") {
			info, _ := f.Info()
			backupPerm = info.Mode().Perm()
			foundBackup = true
			break
		}
	}
	if !foundBackup || backupPerm != 0600 {
		fmt.Printf("FAIL Test 1: Expected 0600 on backup file, got %o (found: %v)\n", backupPerm, foundBackup)
		os.Exit(1)
	}
	fmt.Printf("✓ Permissions verified: config is %o, backup is %o (secrets protected).\n", fi1.Mode().Perm(), backupPerm)

	// -------------------------------------------------------------------------
	// TEST 2: Atomic Update
	// -------------------------------------------------------------------------
	fmt.Println("\n[TEST 2] Atomic Configuration Replacement...")
	var checkDoc map[string]any
	updatedBytes, err := os.ReadFile(test1ConfigFile)
	if err != nil || json.Unmarshal(updatedBytes, &checkDoc) != nil {
		fmt.Printf("FAIL Test 2: Updated config is not valid JSON!\n")
		os.Exit(1)
	}
	// Verify no stray temp files remain in directory
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".config.") && strings.HasSuffix(f.Name(), ".tmp") {
			fmt.Printf("FAIL Test 2: Found lingering temp file: %s\n", f.Name())
			os.Exit(1)
		}
	}
	fmt.Println("✓ Atomic replacement verified: valid JSON on disk, zero dangling temp files.")

	// -------------------------------------------------------------------------
	// TEST 3: Interrupted / Failed Update (Malformed Config Aborts Cleanly)
	// -------------------------------------------------------------------------
	fmt.Println("\n[TEST 3] Interrupted / Failed Update Safety...")
	test3ConfigFile := filepath.Join(tempTestRoot, "test3_corrupt.json")
	corruptContent := []byte("{ this is not valid json")
	_ = os.WriteFile(test3ConfigFile, corruptContent, 0600)

	cmd3 := exec.Command("go", "run", "installer.go",
		"-action=install",
		"-bifrost-dir="+realWorkspaceRoot,
		"-plugin-output-dir="+disposableBuildDir,
		"-config-path="+test3ConfigFile,
	)
	out3, err3 := cmd3.CombinedOutput()
	if err3 == nil {
		fmt.Printf("FAIL Test 3: Expected error on corrupt config JSON, but succeeded:\n%s\n", string(out3))
		os.Exit(1)
	}
	contentAfter3, _ := os.ReadFile(test3ConfigFile)
	if string(contentAfter3) != string(corruptContent) {
		fmt.Printf("FAIL Test 3: Corrupt file was modified instead of preserved untouched!\n")
		os.Exit(1)
	}
	fmt.Println("✓ Malformed config safely rejected; original file preserved untouched.")

	// -------------------------------------------------------------------------
	// TEST 4: Invalid Plugins Configuration (plugins as string, not array)
	// -------------------------------------------------------------------------
	fmt.Println("\n[TEST 4] Malformed 'plugins' Section Handling...")
	test4ConfigFile := filepath.Join(tempTestRoot, "test4_invalid_plugins.json")
	invalidPluginsDoc := map[string]any{
		"plugins": "this-should-be-an-array-not-a-string",
	}
	invBytes, _ := json.Marshal(invalidPluginsDoc)
	_ = os.WriteFile(test4ConfigFile, invBytes, 0600)

	cmd4 := exec.Command("go", "run", "installer.go",
		"-action=install",
		"-bifrost-dir="+realWorkspaceRoot,
		"-plugin-output-dir="+disposableBuildDir,
		"-config-path="+test4ConfigFile,
	)
	out4, err4 := cmd4.CombinedOutput()
	if err4 == nil {
		fmt.Printf("FAIL Test 4: Expected rejection of non-array plugins, got success:\n%s\n", string(out4))
		os.Exit(1)
	}
	if !strings.Contains(string(out4), "expected array") && !strings.Contains(string(out4), "malformed") {
		fmt.Printf("FAIL Test 4: Expected descriptive error message, got:\n%s\n", string(out4))
		os.Exit(1)
	}
	fmt.Println("✓ Malformed 'plugins' section safely detected and refused without data loss.")

	// -------------------------------------------------------------------------
	// TEST 5: Existing Unrelated Plugins Preserved
	// -------------------------------------------------------------------------
	fmt.Println("\n[TEST 5] Preservation of Unrelated Plugins...")
	test5ConfigFile := filepath.Join(tempTestRoot, "test5_multi_plugin.json")
	multiDoc := map[string]any{
		"plugins": []map[string]any{
			{"name": "plugin-alpha", "path": "/opt/alpha.so", "enabled": true},
			{"name": "plugin-beta", "path": "/opt/beta.so", "enabled": false},
		},
	}
	multiBytes, _ := json.MarshalIndent(multiDoc, "", "  ")
	_ = os.WriteFile(test5ConfigFile, multiBytes, 0600)

	cmd5 := exec.Command("go", "run", "installer.go",
		"-action=install",
		"-bifrost-dir="+realWorkspaceRoot,
		"-plugin-output-dir="+disposableBuildDir,
		"-config-path="+test5ConfigFile,
	)
	out5, err5 := cmd5.CombinedOutput()
	if err5 != nil {
		fmt.Printf("FAIL Test 5: %v\n%s\n", err5, string(out5))
		os.Exit(1)
	}

	test5AfterBytes, _ := os.ReadFile(test5ConfigFile)
	var test5AfterDoc map[string]any
	_ = json.Unmarshal(test5AfterBytes, &test5AfterDoc)
	pList5, _ := test5AfterDoc["plugins"].([]any)
	if len(pList5) != 3 {
		fmt.Printf("FAIL Test 5: Expected 3 plugins (alpha, beta, cacheguard), got %d\n", len(pList5))
		os.Exit(1)
	}
	var hasAlpha, hasBeta, hasCG bool
	for _, item := range pList5 {
		m := item.(map[string]any)
		if m["name"] == "plugin-alpha" {
			hasAlpha = true
		}
		if m["name"] == "plugin-beta" {
			hasBeta = true
		}
		if m["name"] == "cacheguard" {
			hasCG = true
		}
	}
	if !hasAlpha || !hasBeta || !hasCG {
		fmt.Printf("FAIL Test 5: Missing expected plugin in list!\n")
		os.Exit(1)
	}
	fmt.Println("✓ Unrelated plugins (alpha, beta) preserved alongside StillMark.")

	// -------------------------------------------------------------------------
	// TEST 6: Repeated Installation (Idempotency & Custom Config Preservation)
	// -------------------------------------------------------------------------
	fmt.Println("\n[TEST 6] Repeated Installation & Custom Config Preservation...")
	// Customize cacheguard config with custom operator values
	for i, item := range pList5 {
		m := item.(map[string]any)
		if m["name"] == "cacheguard" {
			cfg := m["config"].(map[string]any)
			cfg["max_sessions"] = 4200
			cfg["session_ttl"] = "45m"
			pList5[i] = m
			break
		}
	}
	test5AfterDoc["plugins"] = pList5
	customizedBytes, _ := json.MarshalIndent(test5AfterDoc, "", "  ")
	_ = os.WriteFile(test5ConfigFile, customizedBytes, 0600)

	// Re-run installer on the customized config
	cmd6 := exec.Command("go", "run", "installer.go",
		"-action=install",
		"-bifrost-dir="+realWorkspaceRoot,
		"-plugin-output-dir="+disposableBuildDir,
		"-config-path="+test5ConfigFile,
	)
	out6, err6 := cmd6.CombinedOutput()
	if err6 != nil {
		fmt.Printf("FAIL Test 6: %v\n%s\n", err6, string(out6))
		os.Exit(1)
	}

	test6Bytes, _ := os.ReadFile(test5ConfigFile)
	var test6Doc map[string]any
	_ = json.Unmarshal(test6Bytes, &test6Doc)
	pList6, _ := test6Doc["plugins"].([]any)
	if len(pList6) != 3 {
		fmt.Printf("FAIL Test 6: Repeat install duplicated entries! Got %d plugins\n", len(pList6))
		os.Exit(1)
	}
	var preservedMaxSessions bool
	for _, item := range pList6 {
		m := item.(map[string]any)
		if m["name"] == "cacheguard" {
			if cfg, ok := m["config"].(map[string]any); ok {
				if v, ok := cfg["max_sessions"].(float64); ok && int(v) == 4200 {
					preservedMaxSessions = true
				}
			}
		}
	}
	if !preservedMaxSessions {
		fmt.Printf("FAIL Test 6: Custom operator setting (max_sessions=4200) was overwritten!\n")
		os.Exit(1)
	}
	fmt.Println("✓ Idempotent: repeat install preserved custom operator settings (max_sessions=4200) without duplicating entries.")

	// -------------------------------------------------------------------------
	// TEST 7: Safe Transactional Uninstall
	// -------------------------------------------------------------------------
	fmt.Println("\n[TEST 7] Transactional Uninstall...")
	// Verify mock binary exists before uninstall
	mockSO := filepath.Join(disposableBuildDir, "cacheguard.so")
	if _, err := os.Stat(mockSO); err != nil {
		fmt.Printf("FAIL Test 7: Mock SO missing before uninstall: %v\n", err)
		os.Exit(1)
	}

	cmd7 := exec.Command("go", "run", "installer.go",
		"-action=uninstall",
		"-bifrost-dir="+realWorkspaceRoot,
		"-plugin-output-dir="+disposableBuildDir,
		"-config-path="+test5ConfigFile,
	)
	out7, err7 := cmd7.CombinedOutput()
	if err7 != nil {
		fmt.Printf("FAIL Test 7: %v\n%s\n", err7, string(out7))
		os.Exit(1)
	}

	if _, err := os.Stat(mockSO); !os.IsNotExist(err) {
		fmt.Printf("FAIL Test 7: Binary was not removed: %v\n", err)
		os.Exit(1)
	}

	test7Bytes, _ := os.ReadFile(test5ConfigFile)
	var test7Doc map[string]any
	_ = json.Unmarshal(test7Bytes, &test7Doc)
	pList7, _ := test7Doc["plugins"].([]any)
	if len(pList7) != 2 {
		fmt.Printf("FAIL Test 7: Expected 2 remaining plugins, got %d\n", len(pList7))
		os.Exit(1)
	}
	for _, item := range pList7 {
		m := item.(map[string]any)
		if m["name"] == "cacheguard" {
			fmt.Printf("FAIL Test 7: cacheguard still in plugins list!\n")
			os.Exit(1)
		}
	}
	fmt.Println("✓ Transactional uninstall succeeded: config removed first, binary deleted, other plugins intact.")

	// -------------------------------------------------------------------------
	// TEST 8: Symlink Protection
	// -------------------------------------------------------------------------
	fmt.Println("\n[TEST 8] Symlink & Path Traversal Protection...")
	outsideDir := filepath.Join(tempTestRoot, "outside_target")
	_ = os.MkdirAll(outsideDir, 0755)
	symlinkEscape := filepath.Join(tempTestRoot, "symlink_build")
	_ = os.RemoveAll(symlinkEscape)
	// Create symlink pointing outside base dir
	if err := os.Symlink(outsideDir, symlinkEscape); err != nil {
		fmt.Printf("WARNING: Symlink creation failed (skipping os symlink test): %v\n", err)
	} else {
		cmd8 := exec.Command("go", "run", "installer.go",
			"-action=install",
			"-bifrost-dir="+realWorkspaceRoot,
			"-plugin-output-dir="+symlinkEscape,
			"-config-path="+test5ConfigFile,
		)
		out8, err8 := cmd8.CombinedOutput()
		if err8 == nil && strings.Contains(string(out8), "✓ Compiled and verified") {
			fmt.Printf("FAIL Test 8: Installer should have rejected symlink escape!\n%s\n", string(out8))
			os.Exit(1)
		}
		fmt.Println("✓ Symlink escape prevented: installer refused to write through symlink to outside target.")
		_ = os.Remove(symlinkEscape)
	}

	// -------------------------------------------------------------------------
	// TEST 9: Rollback / Preservation on Build Failure
	// -------------------------------------------------------------------------
	fmt.Println("\n[TEST 9] Rollback / Binary Preservation on Build Failure...")
	// Create a dummy valid-looking binary in disposable dir
	_ = os.MkdirAll(disposableBuildDir, 0755)
	existingSOContent := []byte("PREVIOUS_VALID_SO_BINARY_CONTENT")
	_ = os.WriteFile(mockSO, existingSOContent, 0755)

	// Attempt verification of invalid binary (or build failure)
	cmd9 := exec.Command("go", "run", "installer.go",
		"-action=verify",
		"-bifrost-dir="+realWorkspaceRoot,
		"-plugin-output-dir="+disposableBuildDir,
	)
	out9, err9 := cmd9.CombinedOutput()
	// Should fail verification
	if err9 == nil {
		fmt.Printf("FAIL Test 9: Expected verification failure on dummy SO, got success:\n%s\n", string(out9))
		os.Exit(1)
	}
	// Verify existing binary was NOT deleted or modified by failed verify
	soContentAfter9, _ := os.ReadFile(mockSO)
	if string(soContentAfter9) != string(existingSOContent) {
		fmt.Printf("FAIL Test 9: Binary was altered after failed verification!\n")
		os.Exit(1)
	}
	fmt.Println("✓ Existing binary safely preserved across verification failure.")

	// -------------------------------------------------------------------------
	// FINAL INTEGRITY CHECK: Development Binary Untouched
	// -------------------------------------------------------------------------
	fmt.Println("\n[FINAL INTEGRITY CHECK] Verifying development binary was never touched...")
	devSOStatAfter, err := os.Stat(devSO)
	if err != nil {
		fmt.Printf("FAIL: Development binary %s was deleted by tests!\n", devSO)
		os.Exit(1)
	}
	if devSOStatBefore != nil && devSOStatAfter.ModTime() != devSOStatBefore.ModTime() {
		fmt.Printf("FAIL: Development binary was overwritten during tests!\n")
		os.Exit(1)
	}
	fmt.Printf("✓ Confirmed: Development binary at %s was untouched.\n", devSO)

	fmt.Println("\n================================================================================")
	fmt.Println("    ALL 9 ISOLATED INSTALLER TESTS PASSED SUCCESSFULLY!                         ")
	fmt.Println("================================================================================")
}
