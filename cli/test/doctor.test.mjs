import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { inspectBifrostCheckout, checkEnvironment, runDoctor } from '../src/doctor.mjs';

test('doctor: checkEnvironment detects Node.js and Go', async () => {
  const env = await checkEnvironment();
  assert.equal(env.node.ok, true);
  assert.ok(env.node.version.startsWith('v'));
  assert.equal(env.go.ok, true);
  assert.ok(env.go.major >= 1);
});

test('doctor: inspectBifrostCheckout detects missing directory', async () => {
  const res = await inspectBifrostCheckout('/nonexistent/path/for/testing');
  assert.equal(res.exists, false);
  assert.equal(res.scenario, 'NONE');
});

test('doctor: inspectBifrostCheckout detects Scenario C (Binary only)', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-bin-'));
  try {
    const fakeBin = path.join(tmpDir, 'bifrost-http');
    fs.writeFileSync(fakeBin, '#!/bin/sh\necho bifrost', { mode: 0o755 });

    const res = await inspectBifrostCheckout(fakeBin);
    assert.equal(res.scenario, 'C');
    assert.equal(res.supported, false);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

function scaffoldBifrost(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  fs.mkdirSync(path.join(dir, 'core'), { recursive: true });
  fs.writeFileSync(path.join(dir, 'core', 'go.mod'), 'module github.com/maximhq/bifrost/core\n');
  fs.mkdirSync(path.join(dir, 'transports', 'bifrost-http'), { recursive: true });
  fs.mkdirSync(path.join(dir, 'plugins', 'governance'), { recursive: true });
  return dir;
}

test('doctor: stock source checkout without go.work is installable (StillMark not yet present)', async () => {
  const tmpDir = scaffoldBifrost('stillmark-clean-src-');
  try {
    const res = await inspectBifrostCheckout(tmpDir);
    assert.equal(res.scenario, 'A');
    assert.equal(res.supported, true);
    assert.equal(res.details.stillmarkPresent, false);
    assert.equal(res.details.hasGoWork, false);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('doctor: detects StillMark source and go.work registration already present', async () => {
  const tmpDir = scaffoldBifrost('stillmark-compat-src-');
  try {
    fs.writeFileSync(path.join(tmpDir, 'go.work'), 'go 1.27.3\nuse ( ./core ./transports ./plugins/stillmark )\n');
    fs.mkdirSync(path.join(tmpDir, 'plugins', 'stillmark'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'plugins', 'stillmark', 'main.go'), 'package main\n');

    const res = await inspectBifrostCheckout(tmpDir);
    assert.equal(res.supported, true);
    assert.equal(res.details.stillmarkPresent, true);
    assert.equal(res.details.goWorkHasStillmark, true);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});
