import path from 'node:path';
import { runDoctor } from './doctor.mjs';
import { buildNativeGateway, buildLegacyPlugin } from './builder.mjs';
import { activateStillmarkConfig } from './config.mjs';
import { runGatewayVerification } from './verify.mjs';

/**
 * Runs the end-to-end install workflow.
 * @param {object} options
 */
export async function runInstall(options = {}) {
  const {
    bifrostPath,
    configPath,
    dryRun = false,
    verbose = false,
    legacy = false,
    skipVerify = false,
    outputDir = '',
    customConfig = {}
  } = options;

  if (!bifrostPath) {
    throw new Error('Missing required option: --bifrost <path> (path to Bifrost source directory)');
  }

  const absBifrost = path.resolve(bifrostPath);

  // 1. Run Pre-flight Doctor Checks
  const docResult = await runDoctor({
    bifrostPath: absBifrost,
    configPath
  });

  if (!docResult.environment.go.ok) {
    throw new Error(`Incompatible Go toolchain: ${docResult.environment.go.version}. StillMark requires Go 1.22+.`);
  }

  if (!legacy) {
    if (!docResult.canInstallNative) {
      const scenario = docResult.bifrost ? docResult.bifrost.scenario : 'UNKNOWN';
      const scenarioName = docResult.bifrost ? docResult.bifrost.scenarioName : 'Invalid Path';
      const reasons = docResult.bifrost && docResult.bifrost.reasons ? docResult.bifrost.reasons.join('\n') : '';

      throw new Error(
        `Installation Blocked [Scenario ${scenario}: ${scenarioName}]\n\n${reasons}\n\n` +
          `Remedy: To use native StillMark, ensure you are running on a Bifrost source checkout with native StillMark registered.\n` +
          `(If testing legacy .so plugin, specify --legacy explicitly).`
      );
    }
  } else {
    if (!docResult.canInstallLegacy) {
      throw new Error(`Legacy installation requested (--legacy) but legacy plugin source (plugins/cacheguard) was not found.`);
    }
  }

  const results = {
    mode: legacy ? 'legacy_shared_object' : 'native_compile_time',
    scenario: docResult.bifrost.scenario,
    bifrostPath: absBifrost,
    built: null,
    configured: null,
    verified: null
  };

  // 2. Build Stage
  let buildRes;
  if (!legacy) {
    buildRes = await buildNativeGateway(absBifrost, {
      outputDir,
      dryRun,
      verbose
    });
  } else {
    buildRes = await buildLegacyPlugin(absBifrost, {
      dryRun
    });
  }

  results.built = {
    success: buildRes.success,
    binaryPath: buildRes.binaryPath,
    dryRun: buildRes.dryRun
  };

  // 3. Configuration Stage (if --config provided)
  if (configPath) {
    const configRes = activateStillmarkConfig(configPath, {
      dryRun,
      legacy,
      legacyPluginPath: buildRes.binaryPath,
      customConfig
    });

    results.configured = {
      configPath: configRes.configPath,
      backupPath: configRes.backupPath,
      entry: configRes.activatedEntry,
      dryRun
    };
  }

  // 4. Verification Stage (isolated gateway test)
  if (!skipVerify && !dryRun) {
    try {
      const verifyRes = await runGatewayVerification(buildRes.binaryPath, {
        verbose,
        legacy
      });
      results.verified = {
        success: verifyRes.success,
        port: verifyRes.port,
        checks: verifyRes.checks
      };
    } catch (err) {
      results.verified = {
        success: false,
        error: err.message
      };
    }
  }

  return results;
}

