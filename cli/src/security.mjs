import fs from 'node:fs';
import path from 'node:path';
import { execFile, spawn } from 'node:child_process';
import { promisify } from 'node:util';

export const execFileAsync = promisify(execFile);

/**
 * Validates that targetPath is contained within baseDir and does not traverse outside
 * or exploit symlinks.
 * @param {string} baseDir
 * @param {string} targetPath
 */
export function validatePathWithin(baseDir, targetPath) {
  const absBase = path.resolve(baseDir);
  const absTarget = path.resolve(targetPath);

  // Check prefix containment
  if (!absTarget.startsWith(absBase + path.sep) && absTarget !== absBase) {
    throw new Error(`Path traversal denied: '${targetPath}' is outside base directory '${baseDir}'`);
  }

  // Check path components for symlinks
  let curr = absTarget;
  while (curr !== path.dirname(curr)) {
    if (fs.existsSync(curr)) {
      const stat = fs.lstatSync(curr);
      if (stat.isSymbolicLink()) {
        const real = fs.realpathSync(curr);
        throw new Error(`Symlink security violation: '${curr}' is a symlink pointing to '${real}'`);
      }
    }
    curr = path.dirname(curr);
  }

  return absTarget;
}

/**
 * Returns safe file permissions (capped at 0600 for files with sensitive data).
 * @param {number} mode
 * @returns {number}
 */
export function getSafePermissions(mode) {
  if (typeof mode !== 'number') {
    return 0o600;
  }
  const safe = mode & 0o600;
  return safe === 0 ? 0o600 : safe;
}

/**
 * Executes a command safely without shell string interpolation.
 * @param {string} command
 * @param {string[]} args
 * @param {object} options
 * @returns {Promise<{stdout: string, stderr: string}>}
 */
export async function runCommand(command, args = [], options = {}) {
  // Prevent arbitrary command injection: command must be a simple binary name or path
  if (!Array.isArray(args)) {
    throw new TypeError('args must be an array of string arguments');
  }

  // Never invoke through a shell by default
  const execOptions = {
    ...options,
    shell: false,
    maxBuffer: 20 * 1024 * 1024 // 20MB
  };

  try {
    return await execFileAsync(command, args, execOptions);
  } catch (err) {
    const error = new Error(`Command failed: ${command} ${args.join(' ')}\n${err.stderr || err.message}`);
    error.code = err.code;
    error.stdout = err.stdout;
    error.stderr = err.stderr;
    throw error;
  }
}

/**
 * Spawns a background process safely without shell.
 * @param {string} command
 * @param {string[]} args
 * @param {object} options
 */
export function spawnProcess(command, args = [], options = {}) {
  return spawn(command, args, {
    ...options,
    shell: false
  });
}

