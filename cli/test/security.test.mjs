import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { validatePathWithin, getSafePermissions, runCommand } from '../src/security.mjs';

test('security: validatePathWithin allows contained paths', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-sec-'));
  try {
    const subFile = path.join(tmpDir, 'sub', 'file.txt');
    fs.mkdirSync(path.join(tmpDir, 'sub'), { recursive: true });
    fs.writeFileSync(subFile, 'hello');

    const validated = validatePathWithin(tmpDir, subFile);
    assert.equal(validated, path.resolve(subFile));
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('security: validatePathWithin rejects directory traversal', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-sec-'));
  try {
    const outsidePath = path.join(tmpDir, '..', 'evil.txt');
    assert.throws(() => {
      validatePathWithin(tmpDir, outsidePath);
    }, /Path traversal denied/);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('security: validatePathWithin detects and rejects symlinks', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-sec-'));
  try {
    const realTarget = path.join(tmpDir, 'real.txt');
    const linkPath = path.join(tmpDir, 'symlink.txt');
    fs.writeFileSync(realTarget, 'data');
    fs.symlinkSync(realTarget, linkPath);

    assert.throws(() => {
      validatePathWithin(tmpDir, linkPath);
    }, /Symlink security violation/);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('security: getSafePermissions caps permissions at 0600', () => {
  assert.equal(getSafePermissions(0o777), 0o600);
  assert.equal(getSafePermissions(0o644), 0o600);
  assert.equal(getSafePermissions(0o600), 0o600);
  assert.equal(getSafePermissions(0o400), 0o400);
  assert.equal(getSafePermissions(0), 0o600);
});

test('security: runCommand executes without shell string interpolation', async () => {
  const { stdout } = await runCommand('node', ['-e', 'console.log(1 + 1)']);
  assert.equal(stdout.trim(), '2');

  // Verify that arguments with spaces/quotes are treated as literal strings and not shell-evaluated
  const literalArg = 'hello; echo evil';
  const { stdout: echoOut } = await runCommand('node', ['-e', 'console.log(process.argv[1])', literalArg]);
  assert.equal(echoOut.trim(), literalArg);
});

