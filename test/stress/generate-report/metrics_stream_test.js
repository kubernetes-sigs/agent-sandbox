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

const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const { test } = require('node:test');
const { runInNewContext } = require('node:vm');
const { gzipSync } = require('node:zlib');

const source = readFileSync(join(__dirname, 'static/metrics-explorer.js'), 'utf8');
const startMs = Date.parse('2026-10-16T00:00:00Z');
const records = [
    { ts: '2026-10-16T00:00:00Z', metric: 'cpu_total', source: 'node', instance: 'café🙂', labels: { mode: 'idle' }, value: 2 },
    { ts: '2026-10-16T00:00:01Z', metric: 'cpu_total', source: 'node', instance: 'café🙂', labels: { mode: 'idle' }, value: 4 },
];
const bytes = Buffer.from(records.map(record => JSON.stringify(record)).join('\n'));

function streamOf(chunks, cancel) {
    let index = 0;
    return new ReadableStream({
        pull(controller) {
            if (index === chunks.length) controller.close();
            else controller.enqueue(chunks[index++]);
        },
        cancel,
    }, { highWaterMark: 0 });
}

function response(body) {
    return { ok: true, body, arrayBuffer() { throw new Error('whole-response buffering'); } };
}

function createExplorerContext(overrides = {}) {
    const context = { ReadableStream, DecompressionStream, TextDecoder, setTimeout, ...overrides };
    runInNewContext(source, context);
    return context;
}

function assertFixtureLog(log) {
    assert.equal(log.samples, 2);
    assert.equal(log.series.length, 1);
    assert.equal(log.series[0].instance, 'café🙂');
    assert.deepEqual([...log.series[0].t], [0, 1]);
    assert.deepEqual([...log.series[0].v], [2, 4]);
    assert.equal(log.families.get('cpu_total').samples, 2);
}

for (const compressed of [false, true]) {
    for (const layout of ['whole', 'bytewise', 'split header']) {
        test(`${compressed ? 'gzip' : 'raw'}: ${layout} chunks and no final newline`, async () => {
            const data = compressed ? gzipSync(bytes) : bytes;
            const chunks = layout === 'whole' ? [data]
                : layout === 'bytewise' ? [...data].map(byte => Uint8Array.of(byte))
                : [new Uint8Array(0), data.subarray(0, 1), new Uint8Array(0), data.subarray(1)];
            const body = streamOf(chunks);
            const app = createExplorerContext({ fetch: async () => response(body) });
            assertFixtureLog(await app.loadMetricsLog('metrics.jsonl', startMs));
            assert.equal(body.locked, false);
        });
    }
}

test('retries the alternate suffix and accepts raw bytes under either filename', async () => {
    for (const url of ['metrics.jsonl', 'metrics.jsonl.gz']) {
        const requested = [];
        const app = createExplorerContext({ fetch: async path => {
            requested.push(path);
            return requested.length === 1 ? { ok: false, status: 404 }
                : response(streamOf([bytes]));
        } });
        assertFixtureLog(await app.loadMetricsLog(url, startMs));
        assert.deepEqual(requested, [url, url.endsWith('.gz') ? 'metrics.jsonl' : 'metrics.jsonl.gz']);
    }
});

test('failed HTTP responses stay errors', async () => {
    const app = createExplorerContext({ fetch: async () => ({ ok: false, status: 403 }) });
    await assert.rejects(app.loadMetricsLog('metrics.jsonl', startMs), /HTTP 403/);
});

test('empty, bodyless, and gzip-empty responses produce empty indexes', async () => {
    for (const body of [null, streamOf([]), streamOf([new Uint8Array(0)]), streamOf([gzipSync('')])]) {
        const app = createExplorerContext({ fetch: async () => response(body) });
        const log = await app.loadMetricsLog('metrics.jsonl', startMs);
        assert.equal(log.samples, 0);
        assert.equal(log.series.length, 0);
    }
});

test('blank, malformed, and truncated records do not discard valid samples', async () => {
    const data = Buffer.from([
        JSON.stringify(records[0]),
        '', 'not json', '{}', '{"metric":"bad","value":"2"}',
        JSON.stringify(records[1]),
        '{"ts":',
    ].join('\n'));
    const app = createExplorerContext({ fetch: async () => response(streamOf([data])) });
    assertFixtureLog(await app.loadMetricsLog('metrics.jsonl', startMs));
});

test('network failures and corrupt gzip reject instead of returning a partial index', async () => {
    const failed = new ReadableStream({ start(controller) { controller.error(new Error('network failed')); } });
    for (const body of [failed, streamOf([Uint8Array.of(0x1f, 0x8b, 0xff)]), streamOf([gzipSync(bytes).subarray(0, -4)])]) {
        const app = createExplorerContext({ fetch: async () => response(body) });
        await assert.rejects(app.loadMetricsLog('metrics.jsonl', startMs));
        assert.equal(body.locked, false);
    }
});

test('a network failure after samples are indexed still rejects', async () => {
    // The failure above happens before any sample; fail only once a chunk has
    // been decoded, so a partial index exists when the error arrives.
    let decoded;
    const decoding = new Promise(resolve => { decoded = resolve; });
    let sent = false;
    const body = new ReadableStream({ async pull(controller) {
        if (!sent) {
            sent = true;
            controller.enqueue(Buffer.concat([bytes, Buffer.from('\n')]));
            return;
        }
        await decoding;
        controller.error(new Error('network failed'));
    } }, { highWaterMark: 0 });
    class TrackingDecoder extends TextDecoder {
        decode(value, options) { decoded(); return super.decode(value, options); }
    }
    const app = createExplorerContext({ TextDecoder: TrackingDecoder, fetch: async () => response(body) });
    await assert.rejects(app.loadMetricsLog('metrics.jsonl', startMs), /network failed/);
    assert.equal(body.locked, false);
});

test('large chunks are decoded in bounded pieces for loading and direct indexing', async () => {
    const data = Buffer.concat([Buffer.alloc(9 << 20, 0x0a), bytes]);
    for (const entry of ['load', 'stream']) {
        let largestDecode = 0;
        class BoundedDecoder extends TextDecoder {
            decode(value, options) {
                largestDecode = Math.max(largestDecode, value?.byteLength || 0);
                assert.ok(largestDecode <= 1 << 20, 'decoder received more than 1 MiB');
                return super.decode(value, options);
            }
        }
        const app = createExplorerContext({ TextDecoder: BoundedDecoder, fetch: async () => response(streamOf([data])) });
        const log = entry === 'load' ? await app.loadMetricsLog('metrics.jsonl', startMs)
            : await app.indexMetricsStream(streamOf([data]), startMs);
        assertFixtureLog(log);
    }
});

test('compressed input reaches the decompressor in bounded chunks', async () => {
    let largestInput = 0;
    class InspectDecompression {
        constructor(format) {
            const decompressor = new DecompressionStream(format);
            const inspect = new TransformStream({ transform(chunk, controller) {
                largestInput = Math.max(largestInput, chunk.byteLength);
                assert.ok(chunk.byteLength <= 64 << 10, 'decompressor input exceeded 64 KiB');
                controller.enqueue(chunk);
            } });
            this.writable = inspect.writable;
            this.readable = inspect.readable.pipeThrough(decompressor);
        }
    }
    // A large, poorly compressible label forces multiple input slices.
    const { randomBytes } = require('node:crypto');
    const record = { ...records[0], labels: { payload: randomBytes(160000).toString('hex') } };
    const data = gzipSync(JSON.stringify(record));
    assert.ok(data.length > 64 << 10);
    const app = createExplorerContext({ DecompressionStream: InspectDecompression, fetch: async () => response(streamOf([data])) });
    const log = await app.loadMetricsLog('metrics.jsonl', startMs);
    assert.equal(log.samples, 1);
    assert.equal(log.series[0].labels.payload, record.labels.payload);
    assert.ok(largestInput > 0);
});

test('logs below the progress interval report indexing before decoding', async () => {
    for (const data of [bytes, gzipSync(bytes)]) {
        const events = [];
        class TrackingDecoder extends TextDecoder {
            decode(value, options) {
                if (!events.includes('decode')) events.push('decode');
                return super.decode(value, options);
            }
        }
        const app = createExplorerContext({ TextDecoder: TrackingDecoder, fetch: async () => response(streamOf([data])) });
        assertFixtureLog(await app.loadMetricsLog('metrics.jsonl', startMs, stage => events.push(stage)));
        assert.deepEqual(events, ['downloading', 'indexing', 'decode', 'done']);
    }
});

class BrokenDecoder extends TextDecoder {
    decode() { throw new Error('decoder failed'); }
}

for (const [name, first, Decoder, error] of [
    ['raw indexing failure', bytes, BrokenDecoder, /decoder failed/],
    ['gzip indexing failure', gzipSync(bytes), BrokenDecoder, /decoder failed/],
    // An unknown compression method fails inside the decompressor, whose pipe
    // then cancels its source on its own, racing loadMetricsLog's cleanup.
    ['corrupt gzip', Uint8Array.of(0x1f, 0x8b, 0xff, 0, 0, 0, 0, 0, 0, 0), TextDecoder, TypeError],
]) {
    test(`${name} finishes canceling the response before rejecting`, async () => {
        let canceled = false;
        let sent = false;
        const body = new ReadableStream({
            pull(controller) {
                if (!sent) controller.enqueue(first);
                sent = true; // Keep the response open until the consumer cancels it.
            },
            // A cancellation that settles later must still finish before the load rejects.
            cancel: () => new Promise(resolve => setTimeout(() => { canceled = true; resolve(); }, 10)),
        }, { highWaterMark: 0 });
        const app = createExplorerContext({ TextDecoder: Decoder, fetch: async () => response(body) });
        await assert.rejects(app.loadMetricsLog('metrics.jsonl', startMs), error);
        assert.equal(canceled, true);
        assert.equal(body.locked, false);
    });
}

test('a record spanning many chunks preserves its labels and the following record', async () => {
    const record = { ...records[0], labels: { payload: 'x'.repeat(2 << 20) } };
    const data = Buffer.from(JSON.stringify(record) + '\n' + JSON.stringify(records[1]));
    const app = createExplorerContext({ fetch: async () => response(streamOf([data])) });
    const log = await app.loadMetricsLog('metrics.jsonl', startMs);
    assert.equal(log.samples, 2);
    assert.equal(log.series.length, 2);
    assert.equal(log.series[0].labels.payload, record.labels.payload);
    assert.equal(log.series[1].v[0], 4);
});

test('rechunking applies backpressure and forwards cancellation', async () => {
    let slices = 0;
    let canceled;
    const data = new Uint8Array(100);
    data.subarray = (...args) => { slices++; return Uint8Array.prototype.subarray.apply(data, args); };
    const body = streamOf([data], reason => { canceled = reason; });
    const app = createExplorerContext();
    const reader = app.rechunkStream(body, 4).getReader();
    await reader.read();
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.ok(slices <= 2, 'queued slices ahead of demand');
    await reader.cancel('stop');
    reader.releaseLock();
    assert.equal(canceled, 'stop');
    assert.equal(body.locked, false);
});

for (const compressed of [false, true]) {
    // A collect-all-then-index regression never lets the body close; the
    // runner's timeout then fails the test instead of hanging the suite.
    test(`${compressed ? 'gzip' : 'raw'} indexing progresses before EOF`, { timeout: 60000 }, async () => {
        const batch = Buffer.from((JSON.stringify(records[0]) + '\n').repeat(100001));
        const chunks = [compressed ? gzipSync(batch) : batch];
        let indexed = false;
        let finish;
        const ready = new Promise(resolve => { finish = resolve; });
        let index = 0;
        const body = new ReadableStream({ async pull(controller) {
            if (index < chunks.length) controller.enqueue(chunks[index++]);
            else {
                await ready;
                controller.close();
            }
        } }, { highWaterMark: 0 });
        const app = createExplorerContext({ fetch: async () => response(body) });
        const log = await app.loadMetricsLog('metrics.jsonl', startMs, (stage, detail) => {
            // The initial 'indexing' report has no detail; wait for a sample count.
            if (stage === 'indexing' && detail) {
                indexed = true;
                finish();
            } else if (stage === 'done') {
                assert.ok(indexed, 'indexing waited for EOF');
            }
        });
        assert.equal(log.samples, 100001);
    });
}

test('indexing yields to the event loop between progress reports', async () => {
    // In-memory reads resolve as microtasks, so a timer set at one report can
    // only run before the next if indexing itself yields. Raw only: gzip
    // inflation sometimes lets timers run without a yield, so a gzip variant
    // would pass intermittently.
    const data = Buffer.from((JSON.stringify(records[0]) + '\n').repeat(300000));
    let reports = 0;
    let timerRan = false;
    const app = createExplorerContext({ fetch: async () => response(streamOf([data])) });
    const log = await app.loadMetricsLog('metrics.jsonl', startMs, (stage, detail) => {
        if (stage !== 'done' && !(stage === 'indexing' && detail)) return;
        if (reports++) assert.ok(timerRan, `indexing did not yield before ${stage} (report ${reports})`);
        timerRan = false;
        setTimeout(() => { timerRan = true; }, 0);
    });
    // Two sample-count reports plus 'done', so at least two gaps are checked.
    assert.ok(reports >= 3, `only ${reports} progress reports`);
    assert.equal(log.samples, 300000);
});
