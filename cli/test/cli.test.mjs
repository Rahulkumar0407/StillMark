import test from 'node:test';
import assert from 'node:assert/strict';
import { parseArgs } from '../src/cli.mjs';

test('parseArgs: parses default flags correctly', () => {
  const opts = parseArgs(['node', 'stillmark', 'doctor']);
  assert.equal(opts.command, 'doctor');
  assert.equal(opts.dryRun, false);
  assert.equal(opts.verbose, false);
  assert.equal(opts.json, false);
  assert.equal(opts.legacy, false);
  assert.equal(opts.port, 8086);
});

test('parseArgs: parses options and flags correctly', () => {
  const opts = parseArgs([
    'node',
    'stillmark',
    'install',
    '--bifrost',
    '/tmp/test-bifrost',
    '--config',
    '/tmp/config.json',
    '--dry-run',
    '--verbose',
    '--json',
    '--port',
    '9090'
  ]);

  assert.equal(opts.command, 'install');
  assert.equal(opts.bifrostPath, '/tmp/test-bifrost');
  assert.equal(opts.configPath, '/tmp/config.json');
  assert.equal(opts.dryRun, true);
  assert.equal(opts.verbose, true);
  assert.equal(opts.json, true);
  assert.equal(opts.port, 9090);
});

test('parseArgs: handles --help and --version', () => {
  const optsHelp = parseArgs(['node', 'stillmark', '--help']);
  assert.equal(optsHelp.help, true);

  const optsVer = parseArgs(['node', 'stillmark', '-v']);
  assert.equal(optsVer.version, true);
});

test('parseArgs: rejects unknown options', () => {
  assert.throws(() => {
    parseArgs(['node', 'stillmark', 'install', '--unrecognized-flag']);
  }, /Unknown option or argument/);
});

