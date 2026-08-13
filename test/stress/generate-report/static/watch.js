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

// Client-side watch.jsonl viewer: loader, index, query engine and object
// differ for the Kubernetes watch stream captured by the stress tool.
//
// The file is one JSON object per line:
//   {"timestamp": "...", "resource": "pods", "type": "MODIFIED", "object": {...}}
// and can be large (tens of thousands of events, ~100MB decompressed), so
// the design mirrors flamegraph.js: fetch the whole file (gunzipping if
// needed), keep the decompressed bytes as a single Uint8Array, and retain
// only a small index record per event. Full objects are decoded and parsed
// on demand (detail view, diff computation), never held all at once.

// loadWatchLog fetches the watch log, gunzips it if the bytes are gzip
// (the file may be stored compressed with or without a .gz name, or the
// server may have already transparently decoded it), and indexes it.
// onProgress(stage, detail) is called as loading advances.
async function loadWatchLog(url, onProgress) {
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
    return indexWatchLog(buf, progress);
}

// indexWatchLog scans the decompressed buffer once, JSON-parsing each line
// to extract a small per-event index record, then discards the parsed
// object. Yields to the event loop periodically so the page stays live.
//
// For Event objects the searchable identity (kind/namespace/name/uid) is
// taken from involvedObject, so "kind:pod name:foo" surfaces both the pod's
// own watch events and the Events emitted about it.
async function indexWatchLog(buf, onProgress) {
    const progress = onProgress || (() => {});
    const decoder = new TextDecoder();
    const events = [];
    // Last event index per stored object (resource + object uid), linking
    // each event to the previous state of the same object for diffing.
    const lastByObject = new Map();

    let start = 0;
    while (start < buf.length) {
        let end = buf.indexOf(0x0a, start); // '\n'
        if (end === -1) end = buf.length;
        if (end > start) {
            let entry;
            try {
                entry = JSON.parse(decoder.decode(buf.subarray(start, end)));
            } catch (err) {
                entry = null; // tolerate a truncated final line
            }
            if (entry && entry.object) {
                events.push(makeIndexRecord(entry, start, end, events.length, lastByObject));
            }
        }
        start = end + 1;
        if (events.length % 5000 === 0) {
            progress('indexing', `${events.length.toLocaleString()} events`);
            await new Promise(resolve => setTimeout(resolve, 0));
        }
    }
    progress('done', `${events.length.toLocaleString()} events`);
    return { buf, events };
}

function makeIndexRecord(entry, start, end, index, lastByObject) {
    const obj = entry.object;
    const meta = obj.metadata || {};
    const isEvent = obj.kind === 'Event';
    // Events are indexed under the object they describe.
    const involved = isEvent ? (obj.involvedObject || {}) : null;

    const kind = obj.kind || entry.resource || '?';
    const targetKind = involved ? (involved.kind || '') : '';
    const name = involved ? (involved.name || meta.name || '') : (meta.name || '');
    const namespace = (involved ? involved.namespace : meta.namespace) || meta.namespace || '';
    const reason = isEvent ? (obj.reason || '') : '';
    const message = isEvent ? (obj.message || '') : '';

    const objectKey = `${entry.resource}:${meta.uid || meta.name || ''}`;
    const prev = lastByObject.has(objectKey) ? lastByObject.get(objectKey) : -1;
    lastByObject.set(objectKey, index);

    return {
        start, end,
        t: Date.parse(entry.timestamp),
        resource: entry.resource || '',
        type: entry.type || '',
        kind,
        kindLower: kind.toLowerCase(),
        targetKind,
        targetKindLower: targetKind.toLowerCase(),
        namespace,
        name,
        nameLower: name.toLowerCase(),
        namespaceLower: namespace.toLowerCase(),
        uid: meta.uid || '',
        targetUid: involved ? (involved.uid || '') : '',
        reason,
        reasonLower: reason.toLowerCase(),
        message,
        prev,
        hay: `${kind} ${targetKind} ${namespace}/${name} ${reason} ${message} ${entry.type} ${entry.resource}`.toLowerCase(),
    };
}

// getEntry re-decodes and parses one line on demand, returning the full
// {timestamp, resource, type, object} record.
function getEntry(log, index) {
    const ev = log.events[index];
    return JSON.parse(new TextDecoder().decode(log.buf.subarray(ev.start, ev.end)));
}

// ---- Query engine ----
//
// A query is whitespace-separated terms, ANDed together:
//   key:value  -- keys: kind, name, namespace/ns, type, resource, uid, reason
//   bare text  -- substring match over kind, namespace/name, reason, message
// Matching is case-insensitive; kind tolerates singular/plural (pod == pods);
// name/namespace/reason/text are substring matches; type/resource are prefix
// matches (type:mod == MODIFIED); uid is a prefix match.

const QUERY_KEYS = ['kind', 'name', 'namespace', 'ns', 'type', 'resource', 'uid', 'reason'];

function parseQuery(text) {
    const terms = [];
    for (const token of (text || '').trim().split(/\s+/)) {
        if (!token) continue;
        const sep = token.indexOf(':');
        const key = sep > 0 ? token.slice(0, sep).toLowerCase() : '';
        if (QUERY_KEYS.includes(key) && sep < token.length - 1) {
            terms.push({
                key: key === 'ns' ? 'namespace' : key,
                value: token.slice(sep + 1).toLowerCase(),
            });
        } else {
            terms.push({ key: 'text', value: token.toLowerCase() });
        }
    }
    return terms;
}

// kindMatches reports whether a query token names the given kind (already
// lower-cased and singular, e.g. "pod", "sandbox"). Plural queries match by
// appending "s"/"es" to the kind rather than stripping suffixes from the
// query: singularizing "sandboxes" naively yields "sandboxe", which would
// never equal "sandbox". An empty kind (no involvedObject) matches nothing.
function kindMatches(kindLower, query) {
    return kindLower !== '' &&
        (kindLower === query || kindLower + 's' === query || kindLower + 'es' === query);
}

function matchesEvent(ev, terms) {
    for (const term of terms) {
        const v = term.value;
        switch (term.key) {
            case 'kind':
                if (!kindMatches(ev.kindLower, v) && !kindMatches(ev.targetKindLower, v)) return false;
                break;
            case 'name':
                if (!ev.nameLower.includes(v)) return false;
                break;
            case 'namespace':
                if (!ev.namespaceLower.includes(v)) return false;
                break;
            case 'type':
                if (!ev.type.toLowerCase().startsWith(v)) return false;
                break;
            case 'resource':
                // Resource names are plural ("pods"); a singular query is a
                // prefix of the plural, so prefix matching covers both.
                if (!ev.resource.toLowerCase().startsWith(v)) return false;
                break;
            case 'uid':
                if (!ev.uid.startsWith(v) && !ev.targetUid.startsWith(v)) return false;
                break;
            case 'reason':
                if (!ev.reasonLower.includes(v)) return false;
                break;
            default:
                if (!ev.hay.includes(v)) return false;
        }
    }
    return true;
}

function searchEvents(log, queryText, limit) {
    const terms = parseQuery(queryText);
    const matches = [];
    for (let i = 0; i < log.events.length; i++) {
        if (matchesEvent(log.events[i], terms)) {
            matches.push(i);
            if (limit && matches.length >= limit) break;
        }
    }
    return matches;
}

// ---- Object diffing ----

// Paths whose churn is noise, never worth showing.
const DIFF_SKIP_PATHS = new Set(['metadata.managedFields']);

// Candidate identity fields for aligning array elements, in preference
// order — the same fields Kubernetes strategic-merge-patch keys lists by
// (conditions by type, containers/statuses by name, ...).
const ARRAY_MERGE_KEYS = ['type', 'name', 'key', 'ip'];

// arrayMergeKey returns a field name that uniquely identifies every element
// of both arrays, or null to fall back to index alignment.
function arrayMergeKey(a, b) {
    for (const key of ARRAY_MERGE_KEYS) {
        const ok = arr => {
            const seen = new Set();
            for (const el of arr) {
                if (el === null || typeof el !== 'object' || Array.isArray(el)) return false;
                const v = el[key];
                if (typeof v !== 'string' || seen.has(v)) return false;
                seen.add(v);
            }
            return true;
        };
        if ((a.length || b.length) && ok(a) && ok(b)) return key;
    }
    return null;
}

// diffObjects compares two plain JSON values and returns leaf-level changes
// as [{path, from, to}] where from === undefined means the field was added
// and to === undefined means it was removed. Arrays of keyed objects (e.g.
// status.conditions) are aligned by their identity field so an inserted
// element doesn't read as every later element changing; other arrays are
// compared by index.
function diffObjects(before, after) {
    const out = [];
    walk('', before, after);
    return out;

    function walk(path, a, b) {
        if (DIFF_SKIP_PATHS.has(path)) return;
        if (a === b) return;
        const aIsObj = a !== null && typeof a === 'object';
        const bIsObj = b !== null && typeof b === 'object';
        if (aIsObj && bIsObj && Array.isArray(a) && Array.isArray(b)) {
            const mergeKey = arrayMergeKey(a, b);
            if (mergeKey) {
                const byKey = arr => new Map(arr.map(el => [el[mergeKey], el]));
                const aMap = byKey(a), bMap = byKey(b);
                for (const key of new Set([...aMap.keys(), ...bMap.keys()])) {
                    walk(`${path}[${key}]`, aMap.get(key), bMap.get(key));
                }
            } else {
                for (let i = 0; i < Math.max(a.length, b.length); i++) {
                    walk(`${path}[${i}]`, a[i], b[i]);
                }
            }
            return;
        }
        if (aIsObj && bIsObj && !Array.isArray(a) && !Array.isArray(b)) {
            for (const key of new Set([...Object.keys(a), ...Object.keys(b)])) {
                walk(path ? `${path}.${key}` : String(key), a[key], b[key]);
            }
            return;
        }
        out.push({ path, from: a, to: b });
    }
}

// diffForEvent computes the diff between this MODIFIED/DELETED event's
// object and the previous captured state of the same object, or null when
// there is no earlier state in the log.
function diffForEvent(log, index) {
    const ev = log.events[index];
    if (ev.prev < 0) return null;
    const before = getEntry(log, ev.prev).object;
    const after = getEntry(log, index).object;
    return diffObjects(before, after);
}

// formatDiffValue renders one side of a change compactly.
function formatDiffValue(value, maxLen) {
    if (value === undefined) return '∅';
    let text = typeof value === 'string' ? value : JSON.stringify(value);
    if (maxLen && text.length > maxLen) text = text.slice(0, maxLen - 1) + '…';
    return text;
}

// Changes shown last in inline summaries: they accompany every write, so
// they only add signal when nothing else changed.
const DIFF_LOW_SIGNAL = new Set(['metadata.resourceVersion', 'metadata.generation']);

function sortDiffForDisplay(changes) {
    return changes.slice().sort((a, b) => {
        const aLow = DIFF_LOW_SIGNAL.has(a.path) ? 1 : 0;
        const bLow = DIFF_LOW_SIGNAL.has(b.path) ? 1 : 0;
        return aLow - bLow || a.path.localeCompare(b.path);
    });
}
