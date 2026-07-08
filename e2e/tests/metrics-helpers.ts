// Small Prometheus text-format helpers for e2e metrics assertions. Shared by
// the API and runner metrics specs so the parsing lives in one place.

// parseGauge returns the value of a gauge sample with the given metric name.
export function parseGauge(body: string, name: string): number {
  for (const raw of body.split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    if (!line.startsWith(name)) continue;
    const value = Number(line.split(/\s+/).pop());
    if (Number.isFinite(value)) return value;
  }
  return 0;
}

// parseCounter returns the value of a single counter sample matching every
// label in `labels`. Returns 0 if no series matches — the metric family may
// not have been observed yet (Prometheus only emits a family once at least
// one series exists).
export function parseCounter(
  body: string,
  name: string,
  labels: Record<string, string>,
): number {
  for (const raw of body.split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    if (!line.startsWith(name)) continue;
    const open = line.indexOf('{');
    const close = line.indexOf('}');
    if (open < 0 || close < 0) continue;
    const seriesLabels = parseLabels(line.slice(open + 1, close));
    if (!matchesLabels(seriesLabels, labels)) continue;
    const value = Number(line.slice(close + 1).trim().split(/\s+/)[0]);
    if (Number.isFinite(value)) return value;
  }
  return 0;
}

// parseLabels parses a Prometheus label set like `role="api",route="/x"`.
// Sufficient for prom-client output — does not handle escaped quotes in values.
export function parseLabels(s: string): Record<string, string> {
  const out: Record<string, string> = {};
  const re = /(\w+)="([^"]*)"/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    out[m[1]] = m[2];
  }
  return out;
}

// matchesLabels reports whether `series` contains every key/value in `want`.
// Extra labels in `series` are allowed.
export function matchesLabels(
  series: Record<string, string>,
  want: Record<string, string>,
): boolean {
  for (const [k, v] of Object.entries(want)) {
    if (series[k] !== v) return false;
  }
  return true;
}
