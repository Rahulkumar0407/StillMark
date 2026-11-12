import process from 'node:process';
import path from 'node:path';
import fs from 'node:fs';
import { getCliVersion, Logger, colorize } from './utils.mjs';
import { runDoctor } from './doctor.mjs';
import { runInstall } from './install.mjs';
import { runGatewayVerification } from './verify.mjs';
import { runUninstall } from './uninstall.mjs';

function printHelp() {
  const v = getCliVersion();
  console.log(`
${colorize('bright', 'StillMark CLI')} (v${v})
One-command installer, verification engine, and diagnostics manager for StillMark in Bifrost.

${colorize('bright', 'USAGE')}
  $ npx @stillmark/cli <command> [options]
  $ stillmark <command> [options]

${colorize('bright', 'COMMANDS')}
  ${colorize('green', 'doctor')}       Check environment, Go toolchain, and Bifrost compatibility
  ${colorize('green', 'install')}      Compile native Bifrost with StillMark, configure, & verify
  ${colorize('green', 'verify')}       Run live end-to-end checks on an isolated gateway instance
  ${colorize('green', 'uninstall')}    Safely deactivate StillMark in configuration with rollback

${colorize('bright', 'OPTIONS')}
  --bifrost <path>   Path to the Bifrost source checkout directory
  --config <path>    Path to Bifrost config.json for activation/deactivation
  --dry-run          Simulate actions without writing files or building
  --legacy           Enable explicit legacy shared-object (.so) plugin mode
  --skip-verify      Skip the isolated gateway verification step during install
  --clean-artifacts  Remove staged build binaries during uninstall
  --port <port>      Preferred port for isolated verification (default: 8086)
  --output-dir <dir> Custom directory for the built binary (default: <bifrost>/dist)
  --verbose          Show detailed debug logs and build output
  --json             Output results as formatted JSON
  --help, -h         Show this help message
  --version, -v      Show CLI version

${colorize('bright', 'EXAMPLES')}
  # 1. Run environment and checkout compatibility diagnostic:
  $ stillmark doctor --bifrost ./Bifrost --config ./Bifrost/config.json

  # 2. Complete one-command native build, configuration, and verification:
  $ stillmark install --bifrost ./Bifrost --config ./Bifrost/config.json

  # 3. Verify an existing compiled gateway against an isolated mock provider:
  $ stillmark verify --bifrost ./Bifrost/dist/bifrost-http

  # 4. Safely deactivate StillMark:
  $ stillmark uninstall --config ./Bifrost/config.json
`);
}

/**
 * Parses command-line arguments into a structured options object.
 * @param {string[]} argv
 */
export function parseArgs(argv) {
  const args = argv.slice(2);
  const options = {
    command: '',
    bifrostPath: '',
    configPath: '',
    dryRun: false,
    verbose: false,
    json: false,
    legacy: false,
    skipVerify: false,
    cleanArtifacts: false,
    outputDir: '',
    port: 8086,
    help: false,
    version: false
  };

  let i = 0;
  while (i < args.length) {
    const arg = args[i];

    if (arg === '--help' || arg === '-h') {
      options.help = true;
    } else if (arg === '--version' || arg === '-v') {
      options.version = true;
    } else if (arg === '--dry-run') {
      options.dryRun = true;
    } else if (arg === '--verbose') {
      options.verbose = true;
    } else if (arg === '--json') {
      options.json = true;
    } else if (arg === '--legacy') {
      options.legacy = true;
    } else if (arg === '--skip-verify') {
      options.skipVerify = true;
    } else if (arg === '--clean-artifacts') {
      options.cleanArtifacts = true;
    } else if (arg === '--bifrost') {
      i++;
      options.bifrostPath = args[i] || '';
    } else if (arg === '--config') {
      i++;
      options.configPath = args[i] || '';
    } else if (arg === '--output-dir') {
      i++;
      options.outputDir = args[i] || '';
    } else if (arg === '--port') {
      i++;
      options.port = parseInt(args[i], 10) || 8086;
    } else if (!arg.startsWith('-') && !options.command) {
      options.command = arg.toLowerCase();
    } else {
      throw new Error(`Unknown option or argument: '${arg}'`);
    }
    i++;
  }

  return options;
}

/**
 * CLI Main execution entry point.
 * @param {string[]} argv
 */
export async function runCli(argv = process.argv) {
  let options;
  try {
    options = parseArgs(argv);
  } catch (err) {
    console.error(colorize('red', `ERROR: ${err.message}`));
    console.error('Use --help to see available commands and options.');
    return 1;
  }

  const logger = new Logger(options);

  if (options.version) {
    console.log(`v${getCliVersion()}`);
    return 0;
  }

  if (options.help || !options.command) {
    printHelp();
    return 0;
  }

  try {
    switch (options.command) {
      case 'doctor': {
        logger.header('StillMark Pre-flight Doctor Check');
        const report = await runDoctor(options);

        if (options.json) {
          logger.outputJson(report);
          return 0;
        }

        logger.section('Environment & Toolchain');
        if (report.environment.node.ok) {
          logger.success(`Node.js runtime: ${report.environment.node.version} (supported >= 18.0.0)`);
        } else {
          logger.error(`Node.js runtime: ${report.environment.node.version} (unsupported, requires >= 18.0.0)`);
        }

        if (report.environment.go.ok) {
          logger.success(`Go toolchain: ${report.environment.go.version} (supported >= 1.22)`);
        } else {
          logger.error(`Go toolchain: ${report.environment.go.version} (requires Go 1.22+)`);
        }

        if (report.bifrost) {
          logger.section('Bifrost Source Checkout');
          logger.info(`Path: ${report.bifrost.absPath}`);
          logger.info(`Revision: ${report.bifrost.gitRevision || 'n/a'}`);
          logger.info(`Scenario: [${report.bifrost.scenario}] ${report.bifrost.scenarioName}`);

          if (report.bifrost.supported) {
            logger.success('Native StillMark plugin registration verified in plugins.go and go.work');
          } else {
            logger.warn('Native StillMark installation not supported on this checkout:');
            for (const r of report.bifrost.reasons || []) {
              logger.info(`  • ${r}`);
            }
          }
        }

        if (report.config) {
          logger.section('Bifrost Configuration (config.json)');
          logger.info(`Path: ${report.config.path}`);
          if (report.config.valid) {
            logger.success(`Valid JSON configuration (${report.config.pluginCount} plugin entries)`);
            if (report.config.hasStillmark) {
              logger.info(`StillMark status: ${report.config.stillmarkEnabled ? colorize('green', 'ENABLED') : colorize('yellow', 'DISABLED')}`);
            } else {
              logger.info('StillMark status: NOT CONFIGURED');
            }
          } else {
            logger.error(`Invalid configuration: ${report.config.error}`);
          }
        }

        logger.section('Readiness Verdict');
        if (report.canInstallNative) {
          logger.success('READY: Bifrost checkout is compatible for one-command native installation.');
        } else if (report.canInstallLegacy && options.legacy) {
          logger.warn('LEGACY READY: Compatible for shared-object (.so) installation via --legacy.');
        } else {
          logger.error('NOT READY: Resolve missing prerequisites before proceeding with install.');
        }

        return report.canInstallNative || (report.canInstallLegacy && options.legacy) ? 0 : 1;
      }

      case 'install': {
        logger.header('StillMark One-Command Installation');
        if (options.dryRun) {
          logger.warn('[DRY-RUN MODE] Simulating steps without writing files or building.');
        }

        const installResult = await runInstall(options);

        if (options.json) {
          logger.outputJson(installResult);
          return 0;
        }

        logger.section('Phase 1: Gateway Compilation (BUILT)');
        logger.success(`Compiled binary ready at: ${installResult.built.binaryPath}`);

        if (installResult.configured) {
          logger.section('Phase 2: Configuration Update (CONFIGURED)');
          logger.success(`Updated configuration at: ${installResult.configured.configPath}`);
          logger.info(`Safety backup created at: ${installResult.configured.backupPath}`);
        } else {
          logger.section('Phase 2: Configuration (NOT REQUESTED)');
          logger.info('Pass --config <path/to/config.json> to automatically activate StillMark.');
        }

        if (installResult.verified) {
          logger.section('Phase 3: Automated Verification (VERIFIED ACTIVE)');
          if (installResult.verified.success) {
            logger.success(`Verified active against isolated gateway on port ${installResult.verified.port}`);
            for (const chk of installResult.verified.checks) {
              logger.info(`  • ${chk.name}: ${chk.passed ? colorize('green', 'PASS') : colorize('red', 'FAIL')}`);
            }
          } else {
            logger.error(`Verification checks failed: ${installResult.verified.error || 'Check response mismatch'}`);
          }
        }

        logger.section('Summary');
        logger.success('StillMark installation completed successfully.');
        return 0;
      }

      case 'verify': {
        logger.header('StillMark Isolated Gateway Verification');
        let targetBinary = options.bifrostPath;

        if (!targetBinary) {
          throw new Error('Please specify gateway binary or Bifrost checkout via --bifrost <path>');
        }

        // If a directory was provided, look for dist/bifrost-http or tmp/bifrost-http
        if (fs.existsSync(targetBinary) && fs.statSync(targetBinary).isDirectory()) {
          const cand1 = path.join(targetBinary, 'dist', 'bifrost-http');
          const cand2 = path.join(targetBinary, 'tmp', 'bifrost-http');
          if (fs.existsSync(cand1)) {
            targetBinary = cand1;
          } else if (fs.existsSync(cand2)) {
            targetBinary = cand2;
          } else {
            throw new Error(`Gateway binary not found in ${targetBinary}/dist/ or tmp/. Build first with 'stillmark install'.`);
          }
        }

        logger.info(`Testing gateway binary: ${targetBinary}`);
        const verifyRes = await runGatewayVerification(targetBinary, {
          preferredPort: options.port,
          verbose: options.verbose,
          legacy: options.legacy
        });

        if (options.json) {
          logger.outputJson(verifyRes);
          return verifyRes.success ? 0 : 1;
        }

        logger.section('Verification Results');
        for (const chk of verifyRes.checks) {
          logger.info(`  • ${chk.name}: ${chk.passed ? colorize('green', 'PASS') : colorize('red', 'FAIL')}`);
        }

        if (verifyRes.success) {
          logger.success('All live verification checks passed cleanly! StillMark is fully operational.');
          return 0;
        } else {
          logger.error('Verification failed. Diagnostic headers or status mismatch.');
          return 1;
        }
      }

      case 'uninstall': {
        logger.header('StillMark Deactivation & Uninstall');
        const uninstallRes = await runUninstall(options);

        if (options.json) {
          logger.outputJson(uninstallRes);
          return 0;
        }

        if (uninstallRes.deconfigured) {
          logger.section('Configuration Deactivation');
          if (uninstallRes.deconfigured.modified) {
            logger.success(`StillMark deactivated in ${uninstallRes.deconfigured.configPath}`);
            logger.info(`Backup created at: ${uninstallRes.deconfigured.backupPath}`);
          } else {
            logger.info(`StillMark was already inactive in ${uninstallRes.deconfigured.configPath}`);
          }
        }

        if (uninstallRes.removedArtifacts.length > 0) {
          logger.section('Artifact Cleanup');
          for (const art of uninstallRes.removedArtifacts) {
            logger.success(`Removed: ${art}`);
          }
        }

        logger.success('StillMark uninstall workflow completed.');
        return 0;
      }

      default:
        console.error(colorize('red', `Unknown command: '${options.command}'`));
        printHelp();
        return 1;
    }
  } catch (err) {
    if (options.json) {
      logger.outputJson({
        error: err.message,
        stack: options.verbose ? err.stack : undefined
      });
    } else {
      logger.error(err.message);
      if (options.verbose && err.stack) {
        console.error(colorize('gray', err.stack));
      }
    }
    return 1;
  }
}