/*
 Copyright 2026 The Kubernetes Authors.

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

// Client-side metrics.jsonl explorer: loader, series index and query engine
// for the Prometheus scrapes captured by the stress tool.
//
// The file is one sample per line, written by promscrape.go:
//   {"ts": "...", "source": "...", "instance": "...", "metric": "...",
//    "labels": {...}?, "value": N}
// and is large (millions of samples), so the design mirrors watch.js: fetch
// the whole file (gunzipping if needed), scan it once, and keep only compact
// per-series records — a series is one unique (source, instance, metric,
// labels) stream, holding parallel arrays of (offset-seconds, value). The
// raw buffer is discarded after indexing; the series arrays are the working
// set.

// loadMetricsLog fetches, gunzips if the bytes are gzip, and indexes the
// metrics log. startMs anchors each sample's offset-seconds. onProgress
// (stage, detail) is called as loading advances.
async function loadMetricsLog(url, startMs, onProgress) {
    const progress = onProgress || (() => {});
    progress('downloading', '');
    let resp = await fetch(url);
    if (!resp.ok) {
        // The same bytes may sit under either name: prow's GCS uploader
        // strips a .gz suffix on upload, while download_results.py restores
        // it locally. Retry with the suffix toggled before giving up.
        const alt = url.endsWith('.gz') ? url.slice(0, -3) : url + '.gz';
        const altResp = await fetch(alt);
        if (!altResp.ok) {
            throw new Error(`fetching ${url}: HTTP ${resp.status}`);
        }
        resp = altResp;
    }
    let buf = new Uint8Array(await resp.arrayBuffer());
    if (buf[0] === 0x1f && buf[1] === 0x8b) {
        progress('decompressing', '');
        const gunzip = new Response(new Blob([buf]).stream().pipeThrough(new DecompressionStream('gzip')));
        buf = new Uint8Array(await gunzip.arrayBuffer());
    }
    return indexMetricsLog(buf, startMs, progress);
}

// indexMetricsLog scans the buffer once, folding each sample into its
// series. Yields to the event loop periodically so the page stays live.
// tMin/tMax on the result are the earliest and latest sample offset-seconds.
async function indexMetricsLog(buf, startMs, onProgress) {
    const progress = onProgress || (() => {});
    const decoder = new TextDecoder();
    const series = [];
    const byKey = new Map();   // series key -> index into series
    // Scrape timestamps repeat for every metric of one scrape pass, so
    // memoize the string -> epoch-ms parse, and track the time range there
    // rather than rescan every sample.
    const tsCache = new Map();
    let minMs = Infinity, maxMs = -Infinity;
    let samples = 0;

    let start = 0;
    while (start < buf.length) {
        let end = buf.indexOf(0x0a, start); // '\n'
        if (end === -1) end = buf.length;
        if (end > start) {
            let rec;
            try {
                rec = JSON.parse(decoder.decode(buf.subarray(start, end)));
            } catch (err) {
                rec = null; // tolerate a truncated final line
            }
            if (rec && rec.metric && typeof rec.value === 'number') {
                let tMs = tsCache.get(rec.ts);
                if (tMs === undefined) {
                    tMs = Date.parse(rec.ts);
                    tsCache.set(rec.ts, tMs);
                    if (tMs < minMs) minMs = tMs;
                    if (tMs > maxMs) maxMs = tMs;
                }
                addSample(series, byKey, rec, (tMs - startMs) / 1000);
                samples++;
                if (samples % 100000 === 0) {
                    progress('indexing', `${samples.toLocaleString()} samples`);
                    await new Promise(resolve => setTimeout(resolve, 0));
                }
            }
        }
        start = end + 1;
    }

    // Families: one entry per metric name, spanning its series.
    const families = new Map();
    for (const s of series) {
        let fam = families.get(s.metric);
        if (!fam) {
            fam = { metric: s.metric, series: 0, samples: 0, counter: isCounterName(s.metric) };
            families.set(s.metric, fam);
        }
        fam.series++;
        fam.samples += s.t.length;
    }

    progress('done', `${samples.toLocaleString()} samples, ${series.length.toLocaleString()} series`);
    return { series, families, samples, tMin: (minMs - startMs) / 1000, tMax: (maxMs - startMs) / 1000 };
}

function addSample(series, byKey, rec, tSec) {
    const source = rec.source || '';
    const instance = rec.instance || '';
    const labels = rec.labels || null;
    let labelsStr = '';
    if (labels) {
        labelsStr = Object.keys(labels).sort().map(k => `${k}=${labels[k]}`).join(' ');
    }
    const key = `${rec.metric}\0${source}\0${instance}\0${labelsStr}`;
    let idx = byKey.get(key);
    if (idx === undefined) {
        idx = series.length;
        byKey.set(key, idx);
        series.push({
            metric: rec.metric,
            source, instance, labels, labelsStr,
            t: [], v: [],
            hay: `${rec.metric} ${source} ${instance} ${labelsStr}`.toLowerCase(),
        });
    }
    const s = series[idx];
    s.t.push(tSec);
    s.v.push(rec.value);
}

// isCounterName guesses whether a metric is a cumulative counter from its
// Prometheus naming convention; used only to pick the default raw-vs-rate
// view, never to hide data.
function isCounterName(name) {
    return /(_total|_count|_sum|_bucket)$/.test(name);
}

// rateSeries converts a cumulative series to per-second deltas at interval
// midpoints. Counter resets (value decreasing, e.g. a restarted process)
// yield a null point so the plotted line breaks instead of showing a huge
// negative rate.
function rateSeries(s) {
    const t = [], v = [];
    for (let i = 1; i < s.t.length; i++) {
        const dt = s.t[i] - s.t[i - 1];
        if (dt <= 0) continue;
        const dv = s.v[i] - s.v[i - 1];
        t.push((s.t[i] + s.t[i - 1]) / 2);
        v.push(dv < 0 ? null : dv / dt);
    }
    return { t, v };
}

// ---- Query engine ----
//
// A query is whitespace-separated terms, ANDed together:
//   metric:X source:X instance:X  -- substring match on that field
//   anything-else:X               -- label match: the series has a label
//                                    whose name equals the key and whose
//                                    value contains X
//   bare text                     -- substring match over metric, source,
//                                    instance and labels
// A value starting with "=" matches exactly instead of by substring
// (metric:=foo_seconds does not also match foo_seconds_bucket) — it's what
// the click-to-lock drill-down inserts. Matching is case-insensitive.

const METRIC_QUERY_KEYS = ['metric', 'source', 'instance'];

function parseMetricsQuery(text) {
    const terms = [];
    for (const token of (text || '').trim().split(/\s+/)) {
        if (!token) continue;
        const sep = token.indexOf(':');
        if (sep > 0 && sep < token.length - 1) {
            const key = token.slice(0, sep).toLowerCase();
            let value = token.slice(sep + 1).toLowerCase();
            const exact = value.startsWith('=');
            if (exact) value = value.slice(1);
            const term = { value, exact };
            if (METRIC_QUERY_KEYS.includes(key)) {
                term.key = key;
            } else {
                term.key = 'label';
                term.name = key;
            }
            terms.push(term);
        } else {
            terms.push({ key: 'text', value: token.toLowerCase(), exact: false });
        }
    }
    return terms;
}

function valueMatches(actual, term) {
    const v = actual.toLowerCase();
    return term.exact ? v === term.value : v.includes(term.value);
}

function matchesSeries(s, terms) {
    for (const term of terms) {
        switch (term.key) {
            case 'metric':
                if (!valueMatches(s.metric, term)) return false;
                break;
            case 'source':
                if (!valueMatches(s.source, term)) return false;
                break;
            case 'instance':
                if (!valueMatches(s.instance, term)) return false;
                break;
            case 'label': {
                if (!s.labels) return false;
                let hit = false;
                for (const k of Object.keys(s.labels)) {
                    if (k.toLowerCase() === term.name && valueMatches(String(s.labels[k]), term)) {
                        hit = true;
                        break;
                    }
                }
                if (!hit) return false;
                break;
            }
            default:
                if (!s.hay.includes(term.value)) return false;
        }
    }
    return true;
}

// lockTerms builds the query that pins exactly one series: its metric,
// source, instance and every label, all as exact matches. Terms split on
// whitespace, so a label value holding any cannot be written as one; it is
// left out, and the lock may then also match series that differ only there,
// rather than match nothing.
function lockTerms(s) {
    const terms = [`metric:=${s.metric}`];
    if (s.source) terms.push(`source:=${s.source}`);
    if (s.instance) terms.push(`instance:=${s.instance}`);
    if (s.labels) {
        for (const k of Object.keys(s.labels).sort()) {
            const value = String(s.labels[k]);
            if (/\s/.test(value)) continue;
            terms.push(`${k}:=${value}`);
        }
    }
    return terms.join(' ');
}

// searchMetricSeries returns the matching series indices grouped per metric
// family, ordered by metric name: [{metric, counter, indices: [...]}].
function searchMetricSeries(log, queryText) {
    const terms = parseMetricsQuery(queryText);
    const byMetric = new Map();
    for (let i = 0; i < log.series.length; i++) {
        const s = log.series[i];
        if (!matchesSeries(s, terms)) continue;
        let group = byMetric.get(s.metric);
        if (!group) {
            group = { metric: s.metric, counter: isCounterName(s.metric), indices: [] };
            byMetric.set(s.metric, group);
        }
        group.indices.push(i);
    }
    return [...byMetric.values()].sort((a, b) => a.metric.localeCompare(b.metric));
}
