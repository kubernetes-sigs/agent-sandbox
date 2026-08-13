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

// Minimal pprof (profile.proto) parser and flame graph tree builder.
//
// pprof files are gzip-compressed protobuf messages; the page gunzips with
// DecompressionStream and this file decodes the protobuf wire format
// directly, so no protobuf runtime or server-side conversion is needed.
// Only the fields needed for a CPU flame graph are decoded; everything else
// (mappings, labels, comments) is skipped.
//
// Field numbers below are from
// https://github.com/google/pprof/blob/main/proto/profile.proto

// readVarint decodes a base-128 varint at pos, returning [value, nextPos].
// Values are accumulated as doubles: exact up to 2^53, which covers every
// quantity we care about (ids, counts, nanoseconds); 64-bit addresses may
// lose low-bit precision but are only used as fallback frame names.
function readVarint(buf, pos) {
    let value = 0;
    let factor = 1;
    for (;;) {
        const b = buf[pos++];
        value += (b & 0x7f) * factor;
        if ((b & 0x80) === 0) return [value, pos];
        factor *= 128;
    }
}

// forEachField walks one protobuf message, invoking
// visit(fieldNumber, wireType, value) where value is a number for varint /
// fixed fields and a Uint8Array subarray for length-delimited fields.
function forEachField(buf, visit) {
    let pos = 0;
    while (pos < buf.length) {
        let key;
        [key, pos] = readVarint(buf, pos);
        const fieldNumber = Math.floor(key / 8);
        const wireType = key % 8;
        switch (wireType) {
            case 0: { // varint
                let v;
                [v, pos] = readVarint(buf, pos);
                visit(fieldNumber, wireType, v);
                break;
            }
            case 1: // fixed64
                visit(fieldNumber, wireType, 0);
                pos += 8;
                break;
            case 2: { // length-delimited
                let len;
                [len, pos] = readVarint(buf, pos);
                visit(fieldNumber, wireType, buf.subarray(pos, pos + len));
                pos += len;
                break;
            }
            case 5: // fixed32
                visit(fieldNumber, wireType, 0);
                pos += 4;
                break;
            default:
                throw new Error(`unsupported protobuf wire type ${wireType}`);
        }
    }
}

// readPackedVarints decodes a packed repeated varint field into out.
function readPackedVarints(buf, out) {
    let pos = 0;
    while (pos < buf.length) {
        let v;
        [v, pos] = readVarint(buf, pos);
        out.push(v);
    }
}

// parsePprof decodes an UNCOMPRESSED profile.proto buffer into
// {sampleTypes, samples, locations, functions, strings, durationNanos}.
function parsePprof(buf) {
    const decoder = new TextDecoder();
    const profile = {
        sampleTypes: [],   // [{type, unit}] as string-table indices
        samples: [],       // [{locationIds, values}]
        locations: new Map(), // id -> {address, lines: [{functionId}]}
        functions: new Map(), // id -> {nameIdx}
        strings: [],
        durationNanos: 0,
        defaultSampleType: 0, // string-table index
    };

    forEachField(buf, (field, wire, value) => {
        switch (field) {
            case 1: { // sample_type: ValueType
                const vt = { type: 0, unit: 0 };
                forEachField(value, (f, w, v) => {
                    if (f === 1) vt.type = v;
                    if (f === 2) vt.unit = v;
                });
                profile.sampleTypes.push(vt);
                break;
            }
            case 2: { // sample: Sample
                const sample = { locationIds: [], values: [] };
                forEachField(value, (f, w, v) => {
                    if (f === 1) w === 2 ? readPackedVarints(v, sample.locationIds) : sample.locationIds.push(v);
                    if (f === 2) w === 2 ? readPackedVarints(v, sample.values) : sample.values.push(v);
                });
                profile.samples.push(sample);
                break;
            }
            case 4: { // location: Location
                const loc = { id: 0, address: 0, lines: [] };
                forEachField(value, (f, w, v) => {
                    if (f === 1) loc.id = v;
                    if (f === 3) loc.address = v;
                    if (f === 4) { // line: Line
                        const line = { functionId: 0 };
                        forEachField(v, (lf, lw, lv) => {
                            if (lf === 1) line.functionId = lv;
                        });
                        loc.lines.push(line);
                    }
                });
                profile.locations.set(loc.id, loc);
                break;
            }
            case 5: { // function: Function
                const fn = { id: 0, nameIdx: 0 };
                forEachField(value, (f, w, v) => {
                    if (f === 1) fn.id = v;
                    if (f === 2) fn.nameIdx = v;
                });
                profile.functions.set(fn.id, fn);
                break;
            }
            case 6: // string_table
                profile.strings.push(decoder.decode(value));
                break;
            case 10: // duration_nanos
                profile.durationNanos = value;
                break;
            case 13: // default_sample_type
                profile.defaultSampleType = value;
                break;
        }
    });
    return profile;
}

// pickSampleType chooses which sample value column to visualize: the
// profile's default if set, else cpu/nanoseconds, else the last column
// (the pprof CLI's default). Returns {index, label}.
function pickSampleType(profile) {
    const label = i => {
        const vt = profile.sampleTypes[i];
        return `${profile.strings[vt.type]}/${profile.strings[vt.unit]}`;
    };
    if (profile.defaultSampleType) {
        const idx = profile.sampleTypes.findIndex(vt => vt.type === profile.defaultSampleType);
        if (idx >= 0) return { index: idx, label: label(idx) };
    }
    const cpu = profile.sampleTypes.findIndex(
        vt => profile.strings[vt.type] === 'cpu' && profile.strings[vt.unit] === 'nanoseconds');
    if (cpu >= 0) return { index: cpu, label: label(cpu) };
    const last = profile.sampleTypes.length - 1;
    return { index: last, label: label(last) };
}

// buildFlameTree folds samples into a root->leaf tree of
// {name, value, children} nodes, the shape d3-flame-graph consumes. Each
// node's value is the total for its whole subtree (samples are added along
// the entire path). Sample stacks are leaf-first in pprof, and each
// location's inline frames are leaf-first too, so both are walked backwards.
function buildFlameTree(profile, valueIndex) {
    const root = { name: 'root', value: 0, children: [] };
    const childByName = new Map(); // node -> Map(name -> child node)

    for (const sample of profile.samples) {
        const v = sample.values[valueIndex] || 0;
        if (v <= 0) continue;

        const frames = [];
        for (let i = sample.locationIds.length - 1; i >= 0; i--) {
            const loc = profile.locations.get(sample.locationIds[i]);
            if (!loc) continue;
            if (loc.lines.length === 0) {
                frames.push('0x' + loc.address.toString(16));
                continue;
            }
            for (let j = loc.lines.length - 1; j >= 0; j--) {
                const fn = profile.functions.get(loc.lines[j].functionId);
                frames.push(fn ? profile.strings[fn.nameIdx] : '??');
            }
        }

        root.value += v;
        let node = root;
        for (const name of frames) {
            let index = childByName.get(node);
            if (!index) {
                index = new Map();
                childByName.set(node, index);
            }
            let child = index.get(name);
            if (!child) {
                child = { name, value: 0, children: [] };
                index.set(name, child);
                node.children.push(child);
            }
            child.value += v;
            node = child;
        }
    }
    return root;
}

// searchSelfTime sums the self ("flat" in pprof terms) time of matched
// frames: each frame's value minus its children's, over every matched
// occurrence including nested and recursive ones. Complements the
// inclusive total that d3-flame-graph's search handler reports, which
// counts each matched subtree once (frames under an already-matched
// ancestor are excluded there to avoid double counting).
function searchSelfTime(nodes) {
    let self = 0;
    for (const node of nodes) {
        let childSum = 0;
        for (const child of node.children || []) {
            childSum += child.value;
        }
        self += node.value - childSum;
    }
    return self;
}

// loadPprof fetches a pprof file, gunzips it if needed (pprof files are
// gzip-compressed by convention), and parses it.
async function loadPprof(url) {
    const resp = await fetch(url);
    if (!resp.ok) {
        throw new Error(`fetching ${url}: HTTP ${resp.status}`);
    }
    let buf = new Uint8Array(await resp.arrayBuffer());
    if (buf[0] === 0x1f && buf[1] === 0x8b) {
        const gunzip = new Response(new Blob([buf]).stream().pipeThrough(new DecompressionStream('gzip')));
        buf = new Uint8Array(await gunzip.arrayBuffer());
    }
    return parsePprof(buf);
}
