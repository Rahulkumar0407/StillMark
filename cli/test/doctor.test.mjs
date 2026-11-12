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

test('doctor: inspectBifrostCheckout detects Scenario B (Clean source without StillMark)', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-clean-src-'));
  try {
    // Scaffold minimal clean Bifrost source
    fs.writeFileSync(path.join(tmpDir, 'go.work'), 'go 1.27.3\nuse ( ./core ./transports )\n');
    fs.mkdirSync(path.join(tmpDir, 'core'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'core', 'go.mod'), 'module github.com/maximhq/bifrost/core\n');
    fs.mkdirSync(path.join(tmpDir, 'transports', 'bifrost-http', 'server'), { recursive: true });
    fs.writeFileSync(
      path.join(tmpDir, 'transports', 'bifrost-http', 'server', 'plugins.go'),
      'package server\nfunc loadBuiltinPlugins() {}\n'
    );

    const res = await inspectBifrostCheckout(tmpDir);
    assert.equal(res.scenario, 'B');
    assert.equal(res.supported, false);
    assert.ok(res.reasons.some((r) => r.includes('Native StillMark is not registered')));
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('doctor: inspectBifrostCheckout detects Scenario A (Compatible source with StillMark)', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-compat-src-'));
  try {
    // Scaffold compatible Bifrost source
    fs.writeFileSync(path.join(tmpDir, 'go.work'), 'go 1.27.3\nuse ( ./core ./transports ./plugins/stillmark )\n');
    fs.mkdirSync(path.join(tmpDir, 'core'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'core', 'go.mod'), 'module github.com/maximhq/bifrost/core\n');
    fs.mkdirSync(path.join(tmpDir, 'transports', 'bifrost-http', 'server'), { recursive: true });
    fs.writeFileSync(
      path.join(tmpDir, 'transports', 'bifrost-http', 'server', 'plugins.go'),
      'package server\nimport "github.com/maximhq/bifrost/plugins/stillmark"\nfunc load() { _ = stillmark.PluginName }\n'
    );
    fs.mkdirSync(path.join(tmpDir, 'plugins', 'stillmark'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'plugins', 'stillmark', 'stillmark.go'), 'package stillmark\n');

    const res = await inspectBifrostCheckout(tmpDir);
    assert.equal(res.scenario, 'A');
    assert.equal(res.supported, true);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('doctor: inspectBifrostCheckout detects Scenario D (Legacy CacheGuard)', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'stillmark-legacy-src-'));
  try {
    fs.writeFileSync(path.join(tmpDir, 'go.work'), 'go 1.27.3\nuse ( ./core ./transports )\n');
    fs.mkdirSync(path.join(tmpDir, 'core'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'core', 'go.mod'), 'module github.com/maximhq/bifrost/core\n');
    fs.mkdirSync(path.join(tmpDir, 'transports', 'bifrost-http', 'server'), { recursive: true });
    fs.writeFileSync(
      path.join(tmpDir, 'transports', 'bifrost-http', 'server', 'plugins.go'),
      'package server\nfunc loadBuiltinPlugins() {}\n'
    );
    fs.mkdirSync(path.join(tmpDir, 'plugins', 'cacheguard'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'plugins', 'cacheguard', 'main.go'), 'package main\n');

    const res = await inspectBifrostCheckout(tmpDir);
    assert.equal(res.scenario, 'D');
    assert.equal(res.legacySupported, true);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

