#!/usr/bin/env node

import process from 'node:process';
import { runCli } from '../src/cli.mjs';

const exitCode = await runCli(process.argv);
process.exit(exitCode);

