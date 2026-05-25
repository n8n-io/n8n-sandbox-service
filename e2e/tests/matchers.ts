import { expect } from '@playwright/test';
import type { ExecResult } from './helpers';

declare module '@playwright/test' {
  interface Matchers<R, T = unknown> {
    toHaveSucceeded(): R;
  }
}

function formatExecResult(result: ExecResult): string {
  const lines = [
    `  exitCode:  ${result.exitCode}`,
    `  success:   ${result.success}`,
    `  timedOut:  ${result.timedOut}`,
    `  killed:    ${result.killed}`,
    `  execTime:  ${result.executionTimeMs}ms`,
  ];

  if (result.stdout) {
    lines.push(`\n  --- stdout ---\n${result.stdout}`);
  }
  if (result.stderr) {
    lines.push(`\n  --- stderr ---\n${result.stderr}`);
  }

  return lines.join('\n');
}

expect.extend({
  toHaveSucceeded(received: ExecResult) {
    const pass = received.exitCode === 0;
    const message = pass
      ? () => `Expected command to have failed, but it succeeded.\n\n${formatExecResult(received)}`
      : () => `Expected command to have succeeded (exitCode === 0), but it failed.\n\n${formatExecResult(received)}`;
    return { pass, message };
  },
});
