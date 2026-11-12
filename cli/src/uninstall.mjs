import fs from 'node:fs';
import path from 'node:path';
import { deactivateStillmarkConfig } from './config.mjs';
import { validatePathWithin } from './security.mjs';

/**
 * Runs the uninstall workflow.
 * @param {object} options
 */
export async function runUninstall(options = {}) {
  const { bifrostPath = '', configPath = '', dryRun = false, removeEntry = true, cleanArtifacts = false } = options;

  const results = {
    deconfigured: null,
    removedArtifacts: []
  };

  // 1. Transactional Step: Deconfigure in config.json FIRST
  if (configPath) {
    const configRes = deactivateStillmarkConfig(configPath, {
      dryRun,
      removeEntry
    });
    results.deconfigured = configRes;
  }

  // 2. Step 2: If cleanArtifacts is true and bifrostPath provided, remove staged build artifacts
  if (cleanArtifacts && bifrostPath) {
    const absBifrost = path.resolve(bifrostPath);
    validatePathWithin(path.dirname(absBifrost), absBifrost);

    const stagedBin = path.join(absBifrost, 'dist', 'bifrost-http');
    const legacySO = path.join(absBifrost, 'plugins', 'cacheguard', 'build', 'cacheguard.so');

    for (const artifact of [stagedBin, legacySO]) {
      if (fs.existsSync(artifact)) {
        validatePathWithin(absBifrost, artifact);
        if (dryRun) {
          results.removedArtifacts.push(`(dry-run: would remove) ${artifact}`);
        } else {
          try {
            fs.unlinkSync(artifact);
            results.removedArtifacts.push(artifact);
          } catch (err) {
            // Log warning but continue
          }
        }
      }
    }
  }

  return results;
}

