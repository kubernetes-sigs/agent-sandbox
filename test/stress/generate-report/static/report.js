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

// Shared client-side code for the stress test report pages.
//
// Tables: pages pass raw row objects and column specs, so sorting always
// compares raw values, never formatted text.

function escapeHtml(value) {
    return String(value).replace(/[&<>"']/g, c => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[c]));
}

function badge(kind, text) {
    return `<span class="badge badge-${kind}">${escapeHtml(text)}</span>`;
}

// Threshold coloring for numeric cells: above `bad` -> red, above
// `warn` -> orange, otherwise green.
function numClass(value, warn, bad) {
    return value > bad ? 'num-bad' : value > warn ? 'num-warn' : 'num-good';
}

// columns: [{key, label, render?}] where render(value, row) returns
// trusted HTML. Without render, the value is escaped and shown as-is
// (null/undefined -> "-").
function renderTable(selector, columns, rows) {
    const table = document.querySelector(selector);
    const thead = document.createElement('thead');
    const headRow = document.createElement('tr');
    const tbody = document.createElement('tbody');
    let sorted = rows.slice();

    const renderBody = () => {
        tbody.innerHTML = sorted.map(row => '<tr>' + columns.map(col => {
            const value = row[col.key];
            if (col.render) return `<td>${col.render(value, row)}</td>`;
            if (value === null || value === undefined) return '<td>-</td>';
            return `<td>${escapeHtml(value)}</td>`;
        }).join('') + '</tr>').join('');
    };

    columns.forEach(col => {
        const th = document.createElement('th');
        th.className = 'sortable';
        th.textContent = col.label;
        th.addEventListener('click', () => {
            const ascending = th.dataset.order !== 'asc';
            headRow.querySelectorAll('th').forEach(sibling => {
                if (sibling !== th) delete sibling.dataset.order;
            });
            th.dataset.order = ascending ? 'asc' : 'desc';
            sorted = rows.slice().sort((a, b) => {
                let va = a[col.key], vb = b[col.key];
                // Numeric columns: missing values sort below all numbers.
                if (typeof va === 'number' || typeof vb === 'number') {
                    va = typeof va === 'number' ? va : -Infinity;
                    vb = typeof vb === 'number' ? vb : -Infinity;
                    return ascending ? va - vb : vb - va;
                }
                va = va == null ? '' : String(va);
                vb = vb == null ? '' : String(vb);
                return ascending ? va.localeCompare(vb) : vb.localeCompare(va);
            });
            renderBody();
        });
        headRow.appendChild(th);
    });

    thead.appendChild(headRow);
    renderBody();
    table.replaceChildren(thead, tbody);
}

// ---- Chart helpers ----

const CHART_COLORS = [
    '#6366f1', // indigo
    '#10b981', // emerald
    '#f59e0b', // amber
    '#ef4444', // rose
    '#06b6d4', // cyan
    '#ec4899', // pink
    '#8b5cf6', // violet
    '#14b8a6'  // teal
];

// Tick labels for a list of ISO timestamps.
function timeLabels(timestamps) {
    return timestamps.map(ts => new Date(ts).toLocaleTimeString());
}

// Standard styling for one time-series line dataset.
function lineDataset(label, data, idx) {
    const color = CHART_COLORS[idx % CHART_COLORS.length];
    return {
        label: label,
        data: data,
        borderColor: color,
        backgroundColor: color + '20',
        borderWidth: 2,
        tension: 0.1,
        spanGaps: true
    };
}

// Standard options for the time-series line charts.
function lineChartOptions(yTitle) {
    const axis = title => ({
        grid: { color: '#e5e5e0' },
        ticks: { color: '#575551' },
        title: { display: true, text: title, color: '#575551' }
    });
    return {
        animation: false,
        responsive: true,
        maintainAspectRatio: false,
        scales: { x: axis('Time'), y: axis(yTitle) },
        plugins: {
            legend: {
                labels: {
                    color: '#1c1a17',
                    usePointStyle: true,
                    pointStyle: 'line',
                    boxWidth: 20
                }
            }
        }
    };
}

// Chart.js plugin drawing a dashed vertical line and label at each phase
// start. pointTimes holds epoch ms for each data point; labels holds the
// matching tick label (getPixelForValue needs the label on category scales).
function phaseBands(phases, pointTimes, labels) {
    return {
        id: 'phaseBands',
        beforeDraw: (chart) => {
            const { ctx, chartArea: { top, bottom }, scales: { x } } = chart;

            phases.forEach((phase) => {
                const phaseStartMs = new Date(phase.start_ts).getTime();
                const startIdx = pointTimes.findIndex(t => t >= phaseStartMs);
                if (startIdx === -1) return;

                const startX = x.getPixelForValue(labels[startIdx]);

                ctx.strokeStyle = '#1d4ed8';
                ctx.lineWidth = 1.5;
                ctx.setLineDash([5, 5]);
                ctx.beginPath();
                ctx.moveTo(startX, top);
                ctx.lineTo(startX, bottom);
                ctx.stroke();
                ctx.setLineDash([]);

                ctx.fillStyle = '#1d4ed8';
                ctx.font = 'bold 11px ' + getComputedStyle(document.documentElement).getPropertyValue('--font-sans').trim();
                ctx.textAlign = 'left';
                ctx.fillText(phase.name, startX + 6, top + 18);
            });
        }
    };
}
