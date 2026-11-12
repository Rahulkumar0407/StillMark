import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { validatePathWithin, getSafePermissions } from './security.mjs';

/**
 * Creates a collision-free backup of configPath with preserved and secure permissions.
 * @param {string} configPath
 * @param {Buffer|string} content
 * @param {number} mode
 * @returns {string} Path to the created backup file
 */
export function createSafeBackup(configPath, content, mode) {
  const dir = path.dirname(configPath);
  const safePerm = getSafePermissions(mode);

  for (let attempt = 0; attempt < 1000; attempt++) {
    const timestamp = Date.now();
    const backupPath = `${configPath}.bak.${timestamp}_${attempt}`;
    try {
      // Use wx flag for atomic exclusive creation (O_CREAT | O_EXCL)
      fs.writeFileSync(backupPath, content, { mode: safePerm, flag: 'wx' });
      return backupPath;
    } catch (err) {
      if (err.code === 'EEXIST') {
        continue;
      }
      throw new Error(`Failed to create backup file '${backupPath}': ${err.message}`);
    }
  }
  throw new Error(`Exhausted backup filename attempts for '${configPath}'`);
}

/**
 * Atomically writes content to configPath using a temporary file in the same directory.
 * @param {string} configPath
 * @param {string} jsonString
 * @param {number} mode
 */
export function writeConfigAtomically(configPath, jsonString, mode) {
  // Validate JSON string syntax before writing
  try {
    JSON.parse(jsonString);
  } catch (err) {
    throw new Error(`Refusing to write invalid JSON to configuration: ${err.message}`);
  }

  const dir = path.dirname(configPath);
  const safePerm = getSafePermissions(mode);
  const randomSuffix = crypto.randomBytes(6).toString('hex');
  const tmpPath = path.join(dir, `.config.${process.pid}.${Date.now()}.${randomSuffix}.tmp`);

  let cleanup = true;
  try {
    const fd = fs.openSync(tmpPath, 'wx', safePerm);
    try {
      fs.writeFileSync(fd, jsonString, { encoding: 'utf8' });
      fs.fsyncSync(fd);
    } finally {
      fs.closeSync(fd);
    }

    fs.renameSync(tmpPath, configPath);
    cleanup = false;
  } finally {
    if (cleanup && fs.existsSync(tmpPath)) {
      try {
        fs.unlinkSync(tmpPath);
      } catch {
        // Ignore temporary cleanup errors
      }
    }
  }
}

/**
 * Reads and validates a Bifrost config.json file.
 * @param {string} configPath
 */
export function readBifrostConfig(configPath) {
  const absPath = path.resolve(configPath);
  validatePathWithin(path.dirname(absPath), absPath);

  if (!fs.existsSync(absPath)) {
    throw new Error(`Configuration file not found: '${absPath}'`);
  }

  const stat = fs.statSync(absPath);
  const rawContent = fs.readFileSync(absPath, 'utf8');

  let parsed;
  try {
    parsed = JSON.parse(rawContent);
  } catch (err) {
    throw new Error(`Malformed JSON in configuration file '${absPath}': ${err.message}`);
  }

  if (typeof parsed !== 'object' || parsed === null) {
    throw new Error(`Invalid configuration: root must be a JSON object in '${absPath}'`);
  }

  return {
    absPath,
    stat,
    rawContent,
    data: parsed
  };
}

/**
 * Enables StillMark in Bifrost configuration with safe backup and atomic write.
 * @param {string} configPath
 * @param {object} options
 */
export function activateStillmarkConfig(configPath, options = {}) {
  const { dryRun = false, legacy = false, legacyPluginPath = '', customConfig = {} } = options;
  const cfg = readBifrostConfig(configPath);
  const { absPath, stat, rawContent, data } = cfg;

  // Validate or initialize plugins section
  let pluginsList = [];
  if ('plugins' in data) {
    if (!Array.isArray(data.plugins)) {
      throw new Error(`Malformed configuration: 'plugins' property must be an array, got ${typeof data.plugins}`);
    }
    pluginsList = [...data.plugins];
  }

  // Check if StillMark or CacheGuard entry already exists
  const targetName = legacy ? 'cacheguard' : 'stillmark';
  const existingIdx = pluginsList.findIndex(
    (p) => typeof p === 'object' && p !== null && (p.name === 'stillmark' || p.name === 'cacheguard')
  );

  let existingPluginConfig = {};
  if (existingIdx >= 0) {
    const existing = pluginsList[existingIdx];
    if (typeof existing.config === 'object' && existing.config !== null) {
      existingPluginConfig = { ...existing.config };
    }
  }

  // Merge default settings, existing operator settings, and any custom options
  const mergedPluginConfig = {
    max_sessions: 10000,
    session_ttl: '30m',
    expose_schema_paths: false,
    diagnostic_logging: true,
    ...existingPluginConfig,
    ...customConfig
  };

  const newPluginEntry = legacy
    ? {
        name: 'cacheguard',
        type: 'shared_object',
        path: legacyPluginPath,
        enabled: true,
        config: mergedPluginConfig
      }
    : {
        name: 'stillmark',
        enabled: true,
        config: mergedPluginConfig
      };

  if (existingIdx >= 0) {
    pluginsList[existingIdx] = newPluginEntry;
  } else {
    pluginsList.push(newPluginEntry);
  }

  data.plugins = pluginsList;
  const newJsonString = JSON.stringify(data, null, 2) + '\n';

  if (dryRun) {
    return {
      backupPath: '(dry-run: none)',
      configPath: absPath,
      activatedEntry: newPluginEntry
    };
  }

  const backupPath = createSafeBackup(absPath, rawContent, stat.mode);
  writeConfigAtomically(absPath, newJsonString, stat.mode);

  return {
    backupPath,
    configPath: absPath,
    activatedEntry: newPluginEntry
  };
}

/**
 * Deactivates or removes StillMark from Bifrost configuration.
 * @param {string} configPath
 * @param {object} options
 */
export function deactivateStillmarkConfig(configPath, options = {}) {
  const { dryRun = false, removeEntry = false } = options;
  const cfg = readBifrostConfig(configPath);
  const { absPath, stat, rawContent, data } = cfg;

  if (!Array.isArray(data.plugins)) {
    return {
      backupPath: '(no plugins section)',
      configPath: absPath,
      modified: false
    };
  }

  let modified = false;
  let pluginsList = [...data.plugins];

  if (removeEntry) {
    const initialLen = pluginsList.length;
    pluginsList = pluginsList.filter(
      (p) => !(typeof p === 'object' && p !== null && (p.name === 'stillmark' || p.name === 'cacheguard'))
    );
    if (pluginsList.length !== initialLen) {
      modified = true;
    }
  } else {
    for (let i = 0; i < pluginsList.length; i++) {
      const p = pluginsList[i];
      if (typeof p === 'object' && p !== null && (p.name === 'stillmark' || p.name === 'cacheguard')) {
        if (p.enabled !== false) {
          pluginsList[i] = { ...p, enabled: false };
          modified = true;
        }
      }
    }
  }

  if (!modified) {
    return {
      backupPath: '(already inactive)',
      configPath: absPath,
      modified: false
    };
  }

  data.plugins = pluginsList;
  const newJsonString = JSON.stringify(data, null, 2) + '\n';

  if (dryRun) {
    return {
      backupPath: '(dry-run: none)',
      configPath: absPath,
      modified: true
    };
  }

  const backupPath = createSafeBackup(absPath, rawContent, stat.mode);
  writeConfigAtomically(absPath, newJsonString, stat.mode);

  return {
    backupPath,
    configPath: absPath,
    modified: true
  };
}

