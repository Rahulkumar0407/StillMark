import fs from 'node:fs';
import path from 'node:path';
import { runCommand, validatePathWithin } from './security.mjs';

/**
 * Builds the Bifrost HTTP gateway binary containing native StillMark.
 * @param {string} bifrostDir
 * @param {object} options
 */
export async function buildNativeGateway(bifrostDir, options = {}) {
  const { outputDir = '', dryRun = false, verbose = false } = options;

  const targetDir = outputDir
    ? path.resolve(outputDir)
    : path.join(bifrostDir, 'dist');

  validatePathWithin(path.dirname(targetDir), targetDir);

  const finalBinaryPath = path.join(targetDir, 'bifrost-http');

  if (dryRun) {
    return {
      success: true,
      dryRun: true,
      binaryPath: finalBinaryPath,
      message: `[DRY RUN] Would build native Bifrost gateway at ${finalBinaryPath}`
    };
  }

  // Ensure target directory exists
  if (!fs.existsSync(targetDir)) {
    fs.mkdirSync(targetDir, { recursive: true, mode: 0o755 });
  }

  // Build to a temporary file in target directory first to avoid corrupting running binary
  const tmpBinaryPath = path.join(targetDir, `.bifrost-http.${process.pid}.${Date.now()}.tmp`);

  let buildOutput = '';
  try {
    const buildArgs = [
      'build',
      '-o',
      tmpBinaryPath,
      './transports/bifrost-http'
    ];

    const result = await runCommand('go', buildArgs, {
      cwd: bifrostDir,
      env: {
        ...process.env,
        CGO_ENABLED: '1' // Required by Bifrost's go-sqlite3 default config/log store
      }
    });

    buildOutput = (result.stdout + '\n' + result.stderr).trim();

    // Verify the temporary binary exists and is executable
    if (!fs.existsSync(tmpBinaryPath)) {
      throw new Error(`Go build finished with code 0 but binary was not created at '${tmpBinaryPath}'`);
    }

    fs.chmodSync(tmpBinaryPath, 0o755);

    // Atomic rename to final binary path
    fs.renameSync(tmpBinaryPath, finalBinaryPath);

    return {
      success: true,
      dryRun: false,
      binaryPath: finalBinaryPath,
      output: buildOutput
    };
  } catch (err) {
    // Clean up temporary binary if left over
    if (fs.existsSync(tmpBinaryPath)) {
      try {
        fs.unlinkSync(tmpBinaryPath);
      } catch {
        // ignore
      }
    }
    throw new Error(`Failed to build native Bifrost gateway:\n${err.message}`);
  }
}

/**
 * Builds the legacy CacheGuard .so plugin (only when --legacy is explicitly passed).
 * @param {string} bifrostDir
 * @param {object} options
 */
export async function buildLegacyPlugin(bifrostDir, options = {}) {
  const { dryRun = false } = options;
  const legacyDir = path.join(bifrostDir, 'plugins', 'cacheguard');
  const targetDir = path.join(legacyDir, 'build');
  const finalSOPath = path.join(targetDir, 'cacheguard.so');

  if (!fs.existsSync(legacyDir)) {
    throw new Error(`Legacy plugin directory '${legacyDir}' does not exist.`);
  }

  if (dryRun) {
    return {
      success: true,
      dryRun: true,
      binaryPath: finalSOPath,
      message: `[DRY RUN] Would build legacy cacheguard.so at ${finalSOPath}`
    };
  }

  if (!fs.existsSync(targetDir)) {
    fs.mkdirSync(targetDir, { recursive: true, mode: 0o755 });
  }

  const tmpSOPath = path.join(targetDir, `.cacheguard.${process.pid}.${Date.now()}.tmp.so`);

  try {
    const buildArgs = [
      'build',
      '-buildmode=plugin',
      '-o',
      tmpSOPath,
      'main.go',
      'engine.go'
    ];

    const result = await runCommand('go', buildArgs, {
      cwd: legacyDir,
      env: {
        ...process.env,
        CGO_ENABLED: '1',
        GOWORK: path.join(bifrostDir, 'go.work')
      }
    });

    if (!fs.existsSync(tmpSOPath)) {
      throw new Error(`Plugin build completed but .so was not created at '${tmpSOPath}'`);
    }

    fs.chmodSync(tmpSOPath, 0o755);
    fs.renameSync(tmpSOPath, finalSOPath);

    return {
      success: true,
      dryRun: false,
      binaryPath: finalSOPath,
      output: (result.stdout + '\n' + result.stderr).trim()
    };
  } catch (err) {
    if (fs.existsSync(tmpSOPath)) {
      try {
        fs.unlinkSync(tmpSOPath);
      } catch {
        // ignore
      }
    }
    throw new Error(`Failed to build legacy shared object plugin:\n${err.message}`);
  }
}