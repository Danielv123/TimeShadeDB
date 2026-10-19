// Nearest-rank percentiles, matching bench/internal/benchlib/stats.go.
export function percentile(sorted, p) {
  if (sorted.length === 0) return NaN;
  if (p <= 0) return sorted[0];
  const rank = Math.min(sorted.length, Math.max(1, Math.ceil(p * sorted.length))) - 1;
  return sorted[rank];
}

export function summarize(xs) {
  const s = xs.filter((x) => typeof x === "number" && !Number.isNaN(x)).sort((a, b) => a - b);
  if (s.length === 0) return { n: 0 };
  const mean = s.reduce((a, b) => a + b, 0) / s.length;
  return { n: s.length, mean, max: s[s.length - 1], p50: percentile(s, 0.5), p90: percentile(s, 0.9), p95: percentile(s, 0.95), p99: percentile(s, 0.99) };
}
