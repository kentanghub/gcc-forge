// main.ts — spawns the statically-linked Go forge binary and waits for it
// The Go binary handles: HTTP server, register with suite, poll /done, exit.

import { spawn } from 'node:child_process';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const binary = join(__dirname, 'gcc-server');

const child = spawn(binary, [], {
  stdio: ['ignore', 'inherit', 'inherit'],
  env: { ...process.env },
});

child.on('error', (err: Error) => {
  process.stderr.write(`[gcc-forge] failed to start Go binary: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code: number | null) => {
  process.exit(code ?? 0);
});

// Forward SIGTERM/SIGINT to the Go binary
process.on('SIGTERM', () => child.kill('SIGTERM'));
process.on('SIGINT', () => child.kill('SIGINT'));