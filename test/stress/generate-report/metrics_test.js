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

// Run with: node --test test/stress/generate-report/metrics_test.js
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

// Only the DOM operations used by the explorer are needed to exercise its
// pagination and Chart.js configuration without a browser dependency.
class Element {
    constructor() {
        this.children = [];
        this.dataset = {};
        this.html = '';
    }
    set innerHTML(value) {
        this.html = value;
        this.children = [];
        this.canvas = value.includes('<canvas>') ? { getContext: () => this.canvas } : null;
        this.rate = { checked: value.includes('checked'), addEventListener: (name, fn) => { this.rate[name] = fn; } };
    }
    get innerHTML() { return this.html; }
    appendChild(child) { this.children.push(child); }
    replaceChild(child, old) { this.children[this.children.indexOf(old)] = child; }
    removeChild(child) { this.children.splice(this.children.indexOf(child), 1); }
    insertAdjacentHTML(position, html) { this.html += html; }
    addEventListener() {}
    querySelector(selector) {
        if (selector === 'canvas') return this.canvas;
        if (selector === '.mx-rate') return this.rate;
        // A single match hangs its detail off the one metric row, which the
        // stub does not parse out of the HTML; the container stands in.
        if (selector === '.watch-row') return this;
        throw new Error(`Unexpected selector: ${selector}`);
    }
    querySelectorAll(selector) {
        assert.equal(selector, 'canvas');
        return [...(this.canvas ? [this.canvas] : []), ...this.children.flatMap(child => child.querySelectorAll(selector))];
    }
    click() { this.onclick(); }
}

// fixture optionally replaces the default series: [{metric, t, v?}].
async function loadExplorer(query, fixture) {
    const elements = Object.fromEntries(['mx-status', 'mx-search', 'mx-results', 'mx-more'].map(id => [id, new Element()]));
    const charts = new Map();
    class Chart {
        constructor(canvas, config) { this.canvas = canvas; this.config = config; charts.set(canvas, this); this.update(); }
        static getChart(canvas) { return charts.get(canvas); }
        // Stands in for Chart.js layout: the y axis is as wide as the digits
        // of its largest value, then afterFit may adjust it. layouts counts
        // the passes.
        update() {
            const values = this.config.data.datasets.flatMap(ds => ds.data.map(pt => Math.abs(pt.y)));
            this.scales = { y: { chart: this, width: String(Math.round(Math.max(0, ...values))).length * 7 } };
            this.config.options.scales.y.afterFit?.(this.scales.y);
            this.layouts = (this.layouts || 0) + 1;
        }
        destroy() { charts.delete(this.canvas); }
    }
    const series = (fixture || [
        { metric: 'early_gauge', t: [0, 600, 1200, 1800] },
        { metric: 'late_total', t: [900, 1200, 1500, 1800] },
        ...Array.from({ length: 105 }, (_, i) => ({ metric: `node_metric_${String(i).padStart(3, '0')}`, t: [0, 1800] })),
    ]).map(s => ({ source: 'node-exporter', instance: 'node-1', labelsStr: '', hay: s.metric, v: s.t.map((_, i) => i), ...s }));
    const log = { series, families: new Map(series.map(s => [s.metric, { series: 1 }])) };
    const context = vm.createContext({
        document: { getElementById: id => elements[id], createElement: () => new Element(), addEventListener() {} },
        window: { getSelection: () => '' },
        location: { hash: '#q=' + encodeURIComponent(query) },
        history: { replaceState() {} },
        Chart, readLog: async () => log,
    });
    for (const name of ['report.js', 'metrics-explorer.js']) {
        vm.runInContext(fs.readFileSync(path.join(__dirname, 'static', name), 'utf8'), context);
    }
    vm.runInContext('loadMetricsLog = readLog', context);
    const template = fs.readFileSync(path.join(__dirname, 'templates', 'metrics.html'), 'utf8');
    const script = template.match(/<script>([\s\S]*?)<\/script>/)[1]
        .replace('{{ metrics_log | tojson }}', '"metrics.jsonl"')
        .replace('{{ phases | tojson }}', '[]')
        .replace('{{ summary.startTime | tojson }}', '"2026-10-15T00:00:00Z"')
        .replace('(async () => {', 'const loaded = (async () => {');
    vm.runInContext(script, context);
    await vm.runInContext('loaded', context);
    assert.notEqual(elements['mx-status'].textContent, 'load failed', elements['mx-results'].innerHTML);
    return { elements, charts, context };
}

// activate only walks up from the click target with closest(), so a target
// that answers for the chosen panel, metric row or series row stands in for a
// real click without a browser DOM. Returns the query the click left behind.
function click({ elements, context }, { panel, row, series }) {
    const head = {
        closest: () => ({ dataset: { g: String(row) } }),
    };
    const ancestors = {
        '.mx-panel': panel === undefined ? null : { dataset: { p: String(panel) } },
        'tr[data-s]': series === undefined ? null : { dataset: { s: String(series) } },
        '.watch-row-head': row === undefined ? null : head,
    };
    context.clickEvent = {
        target: {
            closest: selector => {
                assert.ok(selector in ancestors, `Unexpected selector: ${selector}`);
                return ancestors[selector];
            },
        },
    };
    vm.runInContext('activate(clickEvent)', context);
    return elements['mx-search'].value;
}

test('each panel can reveal every metric without changing the other panel', async () => {
    const { elements } = await loadExplorer('metric:node_metric ; metric:node_metric_0');
    const [first, second] = elements['mx-results'].children;
    const detail = first.children[0];
    const more = first.children.find(child => child.className === 'watch-more');
    assert.equal((detail.innerHTML.match(/data-g=/g) || []).length, 100);
    assert.ok(more, 'a truncated panel needs its own Show more button');
    assert.equal(more.hidden, false);
    const otherHTML = second.children[0].innerHTML;
    more.click();
    assert.equal((detail.innerHTML.match(/data-g=/g) || []).length, 105);
    assert.match(detail.innerHTML, /data-g="104"/);
    assert.match(detail.innerHTML, /node_metric_104/);
    assert.equal(more.hidden, true);
    assert.equal(second.children[0].innerHTML, otherHTML);
});

test('single-query pagination still reveals every matching metric', async () => {
    const { elements } = await loadExplorer('metric:node_metric');
    const results = elements['mx-results'];
    assert.equal((results.innerHTML.match(/data-g=/g) || []).length, 100);
    elements['mx-more'].click();
    assert.equal((results.innerHTML.match(/data-g=/g) || []).length, 105);
    assert.equal(elements['mx-more'].hidden, true);
});

test('overlapping panels count each matching series once in the status', async () => {
    const { elements } = await loadExplorer('metric:node_metric ; metric:node_metric_0');
    assert.equal(elements['mx-status'].textContent, '2 panels, 105 / 107 series');
});

test('panels keep the full log time range when rate mode changes or a query narrows', async () => {
    const { elements, charts, context } = await loadExplorer('metric:=early_gauge ; metric:=late_total');
    assert.equal(charts.size, 2);
    const assertBounds = () => {
        for (const chart of charts.values()) {
            assert.equal(chart.config.options.scales.x.min, 0);
            assert.equal(chart.config.options.scales.x.max, 1800);
        }
    };
    assertBounds();
    const lateDetail = elements['mx-results'].children[1].children[0];
    assert.equal(lateDetail.rate.checked, true);
    lateDetail.rate.checked = false;
    lateDetail.rate.change();
    assertBounds();
    vm.runInContext('setSubQuery(0, "metric:=late_total")', context);
    assert.equal(charts.size, 2);
    assertBounds();
});

test('clicks inside a panel narrow only that panel', async () => {
    const explorer = await loadExplorer('metric:=early_gauge instance:node-1 ; metric:node_metric_00 source:node-exporter');
    assert.equal(click(explorer, { panel: 0 }),
        'metric:=early_gauge instance:node-1 ; metric:node_metric_00 source:node-exporter');
    // click() supplies data-p and data-g itself, so check the rendered panel carries them.
    const panel = explorer.elements['mx-results'].children[1];
    assert.equal(panel.dataset.p, '1');
    assert.equal(panel.children[0].innerHTML.match(/data-g="3"[\s\S]*?mx-metric">([^<]+)/)[1], 'node_metric_003');
    // Metric rows index the clicked panel's own matches and keep only its filters.
    assert.equal(click(explorer, { panel: 1, row: 3 }),
        'metric:=early_gauge instance:node-1 ; metric:=node_metric_003 source:node-exporter');
    // Series 5 is node_metric_003, after early_gauge and late_total.
    assert.equal(click(explorer, { panel: 1, series: 5 }),
        'metric:=early_gauge instance:node-1 ; metric:=node_metric_003 source:=node-exporter instance:=node-1');
    assert.equal(explorer.elements['mx-status'].textContent, '2 panels, 2 / 107 series');
});

test('clicks without panels still rewrite the whole query', async () => {
    const explorer = await loadExplorer('metric:node_metric_00 source:node-exporter');
    assert.equal(click(explorer, { row: 3 }), 'metric:=node_metric_003 source:node-exporter');
    assert.equal(click(explorer, { series: 5 }), 'metric:=node_metric_003 source:=node-exporter instance:=node-1');
});

test('a semicolon inside a label value does not start a panel', async () => {
    const explorer = await loadExplorer('metric:node_metric_00 source:node-exporter');
    vm.runInContext("log.series[5].labels = { path: '/api;v=1' }", explorer.context);
    click(explorer, { series: 5 });
    assert.equal(explorer.elements['mx-status'].textContent, '1 metrics, 1 / 107 series');
});

test('a panel click ignores an edit the debounce has not rendered', async () => {
    const explorer = await loadExplorer('metric:=early_gauge ; metric:node_metric_00 ; metric:=late_total');
    explorer.elements['mx-search'].value = 'metric:node_metric_00 ; metric:=late_total';
    assert.equal(click(explorer, { panel: 1, row: 3 }),
        'metric:=early_gauge ; metric:=node_metric_003 ; metric:=late_total');
});

test('a single-query click ignores an edit the debounce has not rendered', async () => {
    const explorer = await loadExplorer('metric:node_metric_00 source:node-exporter');
    explorer.elements['mx-search'].value = 'metric:node_metric_00 instance:node-1';
    assert.equal(click(explorer, { row: 3 }), 'metric:=node_metric_003 source:node-exporter');
});

test('stacked charts share the widest y axis so equal times line up', async () => {
    const { elements, charts, context } = await loadExplorer('metric:=big_gauge ; metric:=late_total', [
        { metric: 'big_gauge', t: [0, 600, 1200, 1800], v: [0, 100000, 200000, 300000] },
        { metric: 'late_total', t: [900, 1200, 1500, 1800] },
        { metric: 'small_gauge', t: [0, 1800] },
    ]);
    const widths = () => [...charts.values()].map(chart => chart.scales.y.width);
    assert.deepEqual(widths(), [42, 42]);
    const lateDetail = elements['mx-results'].children[1].children[0];
    lateDetail.rate.checked = false;
    lateDetail.rate.change();
    assert.deepEqual(widths(), [42, 42]);
    vm.runInContext('setSubQuery(1, "metric:=small_gauge")', context);
    assert.deepEqual(widths(), [42, 42]);
});

test('narrowing one panel keeps the other panels as they were', async () => {
    const explorer = await loadExplorer('metric:=late_total ; metric:node_metric');
    const results = explorer.elements['mx-results'];
    const [kept] = results.children;
    kept.children[0].rate.checked = false;
    kept.children[0].rate.change();
    click(explorer, { panel: 1, row: 3 });
    assert.equal(results.children[0], kept);
    assert.equal(kept.children[0].rate.checked, false);
    assert.equal(explorer.charts.size, 2);
});

test('switching from one query to panels renders every panel afresh', async () => {
    const explorer = await loadExplorer('metric:=early_gauge');
    vm.runInContext('setQuery("metric:=early_gauge ; metric:=late_total")', explorer.context);
    const results = explorer.elements['mx-results'];
    assert.deepEqual(results.children.map(child => child.className), ['mx-panel', 'mx-panel']);
    assert.doesNotMatch(results.innerHTML, /data-g=/);
    assert.equal(explorer.charts.size, 2);
});

test('a single-query chart fits its own data', async () => {
    const { charts } = await loadExplorer('metric:=late_total');
    const [chart] = charts.values();
    assert.equal(chart.config.options.scales.x.min, undefined);
    assert.equal(chart.config.options.scales.x.max, undefined);
});

test('a log with a single scrape time still gets a usable shared x range', async () => {
    const { charts } = await loadExplorer('metric:=a ; metric:=b', [{ metric: 'a', t: [900] }, { metric: 'b', t: [900] }]);
    assert.equal(charts.size, 2);
    for (const chart of charts.values()) {
        assert.equal(chart.config.options.scales.x.min, 899);
        assert.equal(chart.config.options.scales.x.max, 901);
    }
});

test('the shared y axis shrinks when the widest stacked panel is replaced', async () => {
    const { charts, context } = await loadExplorer('metric:=big_gauge ; metric:=small_gauge', [
        { metric: 'big_gauge', t: [0, 1800], v: [0, 300000] },
        { metric: 'small_gauge', t: [0, 1800] },
        { metric: 'tiny_gauge', t: [0, 1800] },
    ]);
    const widths = () => [...charts.values()].map(chart => chart.scales.y.width);
    assert.deepEqual(widths(), [42, 42]);
    vm.runInContext('setSubQuery(0, "metric:=tiny_gauge")', context);
    assert.deepEqual(widths(), [7, 7]);
});

test('a single-query chart is laid out once and not aligned', async () => {
    const { charts } = await loadExplorer('metric:=early_gauge');
    const [chart] = charts.values();
    assert.equal(chart.layouts, 1);
    assert.equal(chart.config.options.scales.y.afterFit, undefined);
});

test('stacked panels collapse a lone series\' raw samples so charts stay close', async () => {
    const stacked = await loadExplorer('metric:=early_gauge ; metric:=late_total');
    for (const panel of stacked.elements['mx-results'].children) {
        assert.match(panel.children[0].innerHTML, /<details style=/);
    }
    const single = await loadExplorer('metric:=early_gauge');
    assert.match(single.elements['mx-results'].children[0].innerHTML, /<details open style=/);
});
