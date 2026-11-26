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
// and is large (millions of samples). Decompress and index it incrementally,
// keeping only per-series records — a series is one unique (source, instance,
// metric, labels) stream, holding parallel arrays of (offset-seconds, value).

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
    // Bound compressed input too: some browsers deliver a cached response
    // as one chunk, which the decompressor can inflate into a huge buffer.
    const reader = rechunkStream(resp.body || new ReadableStream({
        start(controller) { controller.close(); },
    }), 64 << 10).getReader();
    try {
        // Gzip's two magic bytes may arrive in separate (or empty) chunks.
        const prefix = [];
        let length = 0;
        while (length < 2) {
            const { done, value } = await reader.read();
            if (done) break;
            prefix.push(value);
            length += value.length;
        }
        const gzip = length >= 2 && prefix[0][0] === 0x1f
            && (prefix[0].length > 1 ? prefix[0][1] : prefix[1][0]) === 0x8b;
        // No cancel(): the finally below owns cleanup, so it still waits for
        // the body's cancellation when the decompressor cancels this stream first.
        let stream = new ReadableStream({
            async pull(controller) {
                if (prefix.length) {
                    controller.enqueue(prefix.shift());
                    return;
                }
                const { done, value } = await reader.read();
                if (done) controller.close(); else controller.enqueue(value);
            },
        });
        if (gzip) {
            stream = stream.pipeThrough(new DecompressionStream('gzip'));
        }
        return await indexMetricsStream(stream, startMs, progress);
    } finally {
        await reader.cancel().catch(() => {});
        reader.releaseLock();
    }
}

// Slice on demand rather than enqueueing every view of a large browser-owned
// buffer at once. The original buffer remains live until its slices are read.
function rechunkStream(stream, maxChunkBytes) {
    const reader = stream.getReader();
    let chunk;
    let offset = 0;
    return new ReadableStream({
        async pull(controller) {
            try {
                while (!chunk || offset === chunk.length) {
                    const { done, value } = await reader.read();
                    if (done) {
                        reader.releaseLock();
                        controller.close();
                        return;
                    }
                    chunk = value;
                    offset = 0;
                }
                controller.enqueue(chunk.subarray(offset, offset + maxChunkBytes));
                offset += Math.min(maxChunkBytes, chunk.length - offset);
                if (offset === chunk.length) chunk = undefined;
            } catch (err) {
                reader.releaseLock();
                throw err;
            }
        },
        async cancel(reason) {
            try { await reader.cancel(reason); }
            finally { reader.releaseLock(); }
        },
    });
}

// Keep only the trailing partial record between chunks. Decode bounds apply
// here so compressed and plain inputs get the same protection.
async function indexMetricsStream(stream, startMs, onProgress) {
    const progress = onProgress || (() => {});
    const decoder = new TextDecoder();
    const series = [];
    const byKey = new Map();   // series key -> index into series
    // Scrape timestamps repeat for every metric of one scrape pass, so
    // memoize the string -> epoch-ms parse.
    const tsCache = new Map();
    let samples = 0;

    let bytes = 0;
    let lastYield = 0;
    let carry = '';
    const indexLine = (line) => {
        if (!line) return;
        let rec;
        try {
            rec = JSON.parse(line);
        } catch (err) {
            return; // tolerate malformed records, including a truncated final line
        }
        if (!rec || !rec.metric || typeof rec.value !== 'number') return;
        let tMs = tsCache.get(rec.ts);
        if (tMs === undefined) {
            tMs = Date.parse(rec.ts);
            tsCache.set(rec.ts, tMs);
        }
        addSample(series, byKey, rec, (tMs - startMs) / 1000);
        samples++;
    };

    // Indexing overlaps the download, so report it before the first sample
    // rather than leaving the earlier stage on screen.
    progress('indexing', '');
    const reader = rechunkStream(stream, 1 << 20).getReader();
    try {
        for (;;) {
            const { done, value } = await reader.read();
            if (done) {
                indexLine(carry + decoder.decode());
                break;
            }
            bytes += value.byteLength;
            const text = decoder.decode(value, { stream: true });
            // Do not rescan a growing partial record on every chunk.
            const end = text.lastIndexOf('\n');
            if (end < 0) {
                carry += text;
            } else {
                for (const line of (carry + text.slice(0, end)).split('\n')) indexLine(line);
                carry = text.slice(end + 1);
            }
            if (samples - lastYield >= 100000) {
                lastYield = samples;
                progress('indexing', `${samples.toLocaleString()} samples, ${(bytes / 1048576).toFixed(0)} MB decoded`);
                await new Promise(resolve => setTimeout(resolve, 0));
            }
        }
    } finally {
        await reader.cancel().catch(() => {});
        reader.releaseLock();
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
    return { series, families, samples };
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
// source, instance and every label, all as exact matches.
function lockTerms(s) {
    const terms = [`metric:=${s.metric}`];
    if (s.source) terms.push(`source:=${s.source}`);
    if (s.instance) terms.push(`instance:=${s.instance}`);
    if (s.labels) {
        for (const k of Object.keys(s.labels).sort()) {
            terms.push(`${k}:=${s.labels[k]}`);
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
