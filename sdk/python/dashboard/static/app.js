// ─── HiveKernel Dashboard ───

// State
let ws = null;
let treeData = null;       // raw nodes array from server
let selectedNode = null;    // currently selected node dict
let ctxNode = null;         // node for context menu

// Per-PID activity log (from WebSocket events).
const nodeActivity = {};     // pid -> [{ts, level, message}, ...]
const NODE_ACTIVITY_MAX = 100;

// Color maps
const ROLE_COLORS = {
    kernel:    '#e94560',
    daemon:    '#ff9f43',
    agent:     '#54a0ff',
    architect: '#a55eea',
    lead:      '#5f27cd',
    worker:    '#10ac84',
    task:      '#576574',
};

const STATE_COLORS = {
    idle:     '#576574',
    running:  '#10ac84',
    blocked:  '#ee5a24',
    sleeping: '#f9ca24',
    dead:     '#636e72',
    zombie:   '#d63031',
};

// ─── WebSocket ───

function connectWS() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}/ws`);

    ws.onopen = () => {
        setStatus(true);
    };

    ws.onmessage = (evt) => {
        const msg = JSON.parse(evt.data);
        if (msg.type === 'tree' && msg.data.ok) {
            treeData = msg.data.nodes;
            renderTree();
        } else if (msg.type === 'tree' && !msg.data.ok) {
            setStatus(false, msg.data.error);
        } else if (msg.type === 'delta') {
            applyDelta(msg.data);
        }
    };

    ws.onclose = () => {
        setStatus(false);
        setTimeout(connectWS, 3000);
    };

    ws.onerror = () => {
        ws.close();
    };
}

function requestRefresh() {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'refresh' }));
    }
}

function applyDelta(delta) {
    if (delta.action === 'log') {
        appendLogEntry(delta);
        // Store per-PID activity.
        if (delta.pid) {
            if (!nodeActivity[delta.pid]) nodeActivity[delta.pid] = [];
            nodeActivity[delta.pid].push({
                ts: delta.ts,
                level: delta.level || 'info',
                message: delta.message || '',
                name: delta.name || '',
            });
            // Trim.
            while (nodeActivity[delta.pid].length > NODE_ACTIVITY_MAX) {
                nodeActivity[delta.pid].shift();
            }
            // Live-update activity panel if this PID is selected.
            if (selectedNode && selectedNode.pid === delta.pid) {
                renderActivityPanel(delta.pid);
            }
        }
        return;
    }
    if (!treeData) return;
    if (delta.action === 'add' && delta.node) {
        treeData.push(delta.node);
    } else if (delta.action === 'update') {
        const n = treeData.find(n => n.pid === delta.pid);
        if (n) {
            if (delta.state !== undefined) n.state = delta.state;
        }
    } else if (delta.action === 'remove') {
        treeData = treeData.filter(n => n.pid !== delta.pid);
    }
    renderTree();
}

function setStatus(connected, detail) {
    const el = document.getElementById('status-indicator');
    if (connected) {
        el.textContent = 'Connected';
        el.className = 'status connected';
    } else {
        el.textContent = detail ? 'Error: ' + detail.slice(0, 30) : 'Disconnected';
        el.className = 'status disconnected';
    }
}

// ─── D3 Tree ───

let svg, g, treeSvg, zoom;
const margin = { top: 40, right: 120, bottom: 40, left: 80 };

function initTree() {
    svg = d3.select('#tree-svg');
    g = svg.append('g').attr('class', 'tree-group');

    zoom = d3.zoom()
        .scaleExtent([0.2, 3])
        .on('zoom', (event) => {
            g.attr('transform', event.transform);
        });

    svg.call(zoom);

    // Close context menu on click
    svg.on('click', () => {
        document.getElementById('context-menu').style.display = 'none';
    });
}

function buildHierarchy(nodes) {
    if (!nodes || nodes.length === 0) return null;

    const map = {};
    nodes.forEach(n => { map[n.pid] = { ...n, children: [] }; });

    let root = null;
    nodes.forEach(n => {
        if (n.pid === 1 || n.ppid === 0) {
            root = map[n.pid];
        } else if (map[n.ppid]) {
            map[n.ppid].children.push(map[n.pid]);
        }
    });

    return root || map[nodes[0].pid];
}

function renderTree() {
    if (!treeData || treeData.length === 0) return;

    const root = buildHierarchy(treeData);
    if (!root) return;

    const hierarchy = d3.hierarchy(root, d => d.children);

    const rect = document.getElementById('tree-svg').getBoundingClientRect();
    const width = rect.width - margin.left - margin.right;
    const height = rect.height - margin.top - margin.bottom;

    const treeLayout = d3.tree().size([height, width * 0.7]);
    treeLayout(hierarchy);

    // Center the tree
    const t = d3.zoomIdentity.translate(margin.left, margin.top);

    // ─── Links ───
    const links = g.selectAll('.link')
        .data(hierarchy.links(), d => `${d.source.data.pid}-${d.target.data.pid}`);

    links.enter()
        .append('path')
        .attr('class', 'link')
        .merge(links)
        .transition().duration(500)
        .attr('d', d3.linkHorizontal()
            .x(d => d.y)
            .y(d => d.x));

    links.exit().remove();

    // ─── Nodes ───
    const nodeData = hierarchy.descendants();
    const nodes = g.selectAll('.node')
        .data(nodeData, d => d.data.pid);

    const nodeEnter = nodes.enter()
        .append('g')
        .attr('class', 'node')
        .attr('transform', d => `translate(${d.y},${d.x})`)
        .on('click', (event, d) => {
            event.stopPropagation();
            selectNode(d.data);
        })
        .on('contextmenu', (event, d) => {
            event.preventDefault();
            event.stopPropagation();
            showContextMenu(event, d.data);
        });

    nodeEnter.append('circle')
        .attr('r', 0)
        .transition().duration(500)
        .attr('r', 7);

    nodeEnter.append('text')
        .attr('class', 'node-name')
        .attr('dy', -12)
        .attr('text-anchor', 'middle');

    nodeEnter.append('text')
        .attr('class', 'node-pid')
        .attr('dy', 20)
        .attr('text-anchor', 'middle');

    // Update
    const nodeUpdate = nodeEnter.merge(nodes);

    nodeUpdate.transition().duration(500)
        .attr('transform', d => `translate(${d.y},${d.x})`);

    nodeUpdate.select('circle')
        .attr('fill', d => {
            const isZombie = d.data.state === 'zombie' || d.data.state === 'dead';
            return isZombie ? '#3a3a4a' : (ROLE_COLORS[d.data.role] || '#576574');
        })
        .attr('stroke', d => STATE_COLORS[d.data.state] || '#576574')
        .attr('opacity', d => {
            const isZombie = d.data.state === 'zombie' || d.data.state === 'dead';
            return isZombie ? 0.4 : 1;
        });

    nodeUpdate.select('.node-name')
        .text(d => d.data.name || '?')
        .attr('opacity', d => (d.data.state === 'zombie' || d.data.state === 'dead') ? 0.4 : 1);

    nodeUpdate.select('.node-pid')
        .text(d => 'PID ' + d.data.pid)
        .attr('opacity', d => (d.data.state === 'zombie' || d.data.state === 'dead') ? 0.4 : 1);

    // Highlight selected
    nodeUpdate.classed('selected', d => selectedNode && d.data.pid === selectedNode.pid);

    nodes.exit()
        .transition().duration(300)
        .style('opacity', 0)
        .remove();

    // If selected node is in tree, update detail panel
    if (selectedNode) {
        const updated = treeData.find(n => n.pid === selectedNode.pid);
        if (updated) {
            selectedNode = updated;
            fillDetail(updated);
        }
    }
}

// ─── Node Selection ───

function selectNode(node) {
    selectedNode = node;
    document.getElementById('detail-placeholder').style.display = 'none';
    document.getElementById('detail-content').style.display = 'block';
    fillDetail(node);
    loadTaskHistory(node.pid);
    renderActivityPanel(node.pid);

    // Re-render to update selection highlight
    renderTree();
}

function fillDetail(n) {
    document.getElementById('d-pid').textContent = n.pid;
    document.getElementById('d-ppid').textContent = n.ppid;
    document.getElementById('d-name').textContent = n.name;
    document.getElementById('d-user').textContent = n.user || '-';
    document.getElementById('d-role').textContent = n.role;
    document.getElementById('d-tier').textContent = n.cognitive_tier;
    document.getElementById('d-model').textContent = n.model || '-';
    document.getElementById('d-state').textContent = n.state;
    document.getElementById('d-vps').textContent = n.vps || '-';
    document.getElementById('d-children').textContent = n.child_count;
    document.getElementById('d-tokens').textContent = n.tokens_consumed.toLocaleString();
    document.getElementById('d-context').textContent = n.context_usage_percent + '%';
    document.getElementById('d-task').textContent = n.current_task_id || '-';
    document.getElementById('d-runtime').textContent = n.runtime_addr || '-';

    // Color the state cell
    const stateEl = document.getElementById('d-state');
    stateEl.style.color = STATE_COLORS[n.state] || '#e0e0e0';

    // Color the role cell
    const roleEl = document.getElementById('d-role');
    roleEl.style.color = ROLE_COLORS[n.role] || '#e0e0e0';
}

// ─── Spawn ───

function openSpawnModal() {
    if (selectedNode) {
        document.getElementById('spawn-parent').value = selectedNode.pid;
    }
    document.getElementById('spawn-modal').style.display = 'flex';
}

async function doSpawn() {
    const body = {
        parent_pid: parseInt(document.getElementById('spawn-parent').value),
        name: document.getElementById('spawn-name').value || 'new-agent',
        role: document.getElementById('spawn-role').value,
        tier: document.getElementById('spawn-tier').value,
        runtime_image: document.getElementById('spawn-runtime').value,
        system_prompt: document.getElementById('spawn-prompt').value,
    };

    closeModal('spawn-modal');

    try {
        const resp = await fetch('/api/spawn', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
        });
        const data = await resp.json();
        if (data.ok) {
            toast('success', `Spawned PID ${data.child_pid}: ${body.name}`);
            requestRefresh();
        } else {
            toast('error', `Spawn failed: ${data.error}`);
        }
    } catch (e) {
        toast('error', `Spawn error: ${e.message}`);
    }
}

// ─── Kill ───

async function killSelected() {
    if (!selectedNode) return;
    if (selectedNode.pid === 1) {
        toast('error', 'Cannot kill PID 1 (kernel)');
        return;
    }
    if (!confirm(`Kill PID ${selectedNode.pid} (${selectedNode.name})?`)) return;

    try {
        const resp = await fetch(`/api/kill/${selectedNode.pid}`, { method: 'POST' });
        const data = await resp.json();
        if (data.ok) {
            toast('success', `Killed PIDs: ${data.killed_pids.join(', ')}`);
            selectedNode = null;
            document.getElementById('detail-content').style.display = 'none';
            document.getElementById('detail-placeholder').style.display = 'block';
            requestRefresh();
        } else {
            toast('error', `Kill failed: ${data.error}`);
        }
    } catch (e) {
        toast('error', `Kill error: ${e.message}`);
    }
}

// ─── Execute (Send Task) ───

function openExecuteModal() {
    if (!selectedNode) return;
    document.getElementById('exec-target-pid').textContent = selectedNode.pid;
    document.getElementById('exec-target-name').textContent = selectedNode.name || '?';
    document.getElementById('exec-description').value = '';
    document.getElementById('exec-params').value = '{}';
    document.getElementById('exec-progress').style.display = 'none';
    document.getElementById('exec-result').style.display = 'none';
    document.getElementById('exec-result').innerHTML = '';
    document.getElementById('exec-params-section').style.display = 'none';
    document.getElementById('exec-submit').disabled = false;
    document.getElementById('execute-modal').style.display = 'flex';
    document.getElementById('exec-description').focus();
}

function toggleExecParams() {
    const section = document.getElementById('exec-params-section');
    section.style.display = section.style.display === 'none' ? 'block' : 'none';
}

async function doExecute() {
    if (!selectedNode) return;
    const pid = selectedNode.pid;
    const description = document.getElementById('exec-description').value.trim();
    if (!description) {
        toast('error', 'Please describe the task');
        return;
    }
    let params = {};
    try {
        params = JSON.parse(document.getElementById('exec-params').value || '{}');
    } catch (e) {
        toast('error', 'Invalid JSON in params');
        return;
    }
    const timeout = parseInt(document.getElementById('exec-timeout').value) || 120;

    // Show progress.
    document.getElementById('exec-submit').disabled = true;
    document.getElementById('exec-progress').style.display = 'block';
    document.getElementById('exec-progress-fill').style.width = '30%';
    document.getElementById('exec-progress-text').textContent = 'Sending task...';
    document.getElementById('exec-result').style.display = 'none';

    let progressPct = 30;
    const progressInterval = setInterval(() => {
        if (progressPct < 90) {
            progressPct += Math.random() * 5;
            document.getElementById('exec-progress-fill').style.width = progressPct + '%';
        }
    }, 1500);

    const startTime = Date.now();

    try {
        const resp = await fetch(`/api/execute/${pid}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ description, params, timeout }),
        });
        const data = await resp.json();
        clearInterval(progressInterval);
        const durationMs = Date.now() - startTime;

        document.getElementById('exec-progress-fill').style.width = '100%';

        if (data.ok) {
            const r = data.result;
            document.getElementById('exec-progress-text').textContent = `Done in ${(durationMs / 1000).toFixed(1)}s`;
            showTaskResult(r, description);
            // Show inline result in modal.
            const resultEl = document.getElementById('exec-result');
            resultEl.style.display = 'block';
            const exitClass = r.exit_code === 0 ? 'exec-result-ok' : 'exec-result-err';
            resultEl.innerHTML = `<div class="${exitClass}">Exit code: ${r.exit_code}</div>`
                + `<pre class="exec-result-output">${escapeHtml(r.output || '(no output)')}</pre>`;
            toast('success', 'Task completed!');
            requestRefresh();
        } else {
            document.getElementById('exec-progress-text').textContent = 'Failed';
            const resultEl = document.getElementById('exec-result');
            resultEl.style.display = 'block';
            resultEl.innerHTML = `<div class="exec-result-err">${escapeHtml(data.error)}</div>`;
            toast('error', `Task failed: ${data.error}`);
        }
    } catch (e) {
        clearInterval(progressInterval);
        document.getElementById('exec-progress-text').textContent = 'Error';
        toast('error', `Execute error: ${e.message}`);
    }

    document.getElementById('exec-submit').disabled = false;
}

// ─── Logs ───

const MAX_LOG_LINES = 200;

function appendLogEntry(data) {
    const container = document.getElementById('log-entries');
    if (!container) return;

    const ts = data.ts ? new Date(data.ts).toLocaleTimeString() : '';
    const level = (data.level || 'info').toUpperCase();
    const pid = data.pid || 0;
    const name = data.name || '';
    const msg = data.message || '';

    const div = document.createElement('div');
    div.className = `log-line log-${(data.level || 'info').toLowerCase()}`;
    div.innerHTML =
        `<span class="log-ts">${ts}</span>` +
        `<span class="log-pid">PID ${pid}</span>` +
        (name ? `<span class="log-name">${name}</span>` : '') +
        `<span class="log-level">[${level}]</span> ` +
        `<span class="log-msg">${escapeHtml(msg)}</span>`;

    container.appendChild(div);

    // Trim old entries.
    while (container.children.length > MAX_LOG_LINES) {
        container.removeChild(container.firstChild);
    }

    // Auto-scroll to bottom.
    container.scrollTop = container.scrollHeight;
}

function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

function clearLogs() {
    const container = document.getElementById('log-entries');
    if (container) container.innerHTML = '';
}

function toggleLogPanel() {
    const content = document.getElementById('log-content');
    const toggle = document.getElementById('log-toggle');
    if (content.style.display === 'none') {
        content.style.display = 'flex';
        toggle.textContent = '-';
    } else {
        content.style.display = 'none';
        toggle.textContent = '+';
    }
}

// ─── Run Task ───

function openRunTaskModal() {
    if (!selectedNode) return;
    document.getElementById('rt-target-pid').textContent = selectedNode.pid;
    document.getElementById('rt-task').value = '';
    document.getElementById('rt-progress').style.display = 'none';
    document.getElementById('rt-submit').disabled = false;
    document.getElementById('run-task-modal').style.display = 'flex';
    document.getElementById('rt-task').focus();
}

async function doRunTask() {
    if (!selectedNode) return;
    const parentPid = selectedNode.pid;
    const task = document.getElementById('rt-task').value.trim();
    if (!task) {
        toast('error', 'Please describe a task');
        return;
    }

    const model = document.getElementById('rt-model').value;
    const maxWorkers = parseInt(document.getElementById('rt-workers').value);
    const timeout = parseInt(document.getElementById('rt-timeout').value) || 300;

    // Show progress.
    document.getElementById('rt-submit').disabled = true;
    document.getElementById('rt-progress').style.display = 'block';
    document.getElementById('rt-progress-fill').style.width = '20%';
    document.getElementById('rt-progress-text').textContent = 'Spawning orchestrator...';

    // Animate progress bar.
    let progressPct = 20;
    const progressInterval = setInterval(() => {
        if (progressPct < 90) {
            progressPct += Math.random() * 3;
            document.getElementById('rt-progress-fill').style.width = progressPct + '%';
        }
    }, 2000);

    const phases = [
        { at: 5000, text: 'Decomposing task...' },
        { at: 15000, text: 'Spawning workers...' },
        { at: 25000, text: 'Workers processing...' },
        { at: 60000, text: 'Synthesizing results...' },
    ];
    const phaseTimers = phases.map(p =>
        setTimeout(() => {
            const el = document.getElementById('rt-progress-text');
            if (el) el.textContent = p.text;
        }, p.at)
    );

    try {
        const resp = await fetch('/api/run-task', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                parent_pid: parentPid,
                task: task,
                model: model,
                max_workers: maxWorkers,
                timeout: timeout,
            }),
        });
        const data = await resp.json();

        clearInterval(progressInterval);
        phaseTimers.forEach(t => clearTimeout(t));

        if (data.ok) {
            document.getElementById('rt-progress-fill').style.width = '100%';
            document.getElementById('rt-progress-text').textContent = 'Done!';

            // Show result panel.
            showTaskResult(data.result, task);
            toast('success', 'Task completed!');
            requestRefresh();

            setTimeout(() => closeModal('run-task-modal'), 1500);
        } else {
            document.getElementById('rt-progress-text').textContent = 'Failed: ' + data.error;
            toast('error', 'Task failed: ' + data.error);
        }
    } catch (e) {
        clearInterval(progressInterval);
        phaseTimers.forEach(t => clearTimeout(t));
        document.getElementById('rt-progress-text').textContent = 'Error: ' + e.message;
        toast('error', 'Request error: ' + e.message);
    }

    document.getElementById('rt-submit').disabled = false;
}

function showTaskResult(result, taskDesc) {
    const panel = document.getElementById('result-panel');
    panel.style.display = 'block';

    const tokens = result.artifacts.total_tokens || '?';
    const subtasks = result.metadata.subtasks_count || '?';
    document.getElementById('result-status').textContent =
        `Task: "${taskDesc.slice(0, 60)}..." | ${subtasks} subtasks | ${tokens} tokens`;

    document.getElementById('result-output').textContent = result.output;
}

function toggleResultPanel() {
    const content = document.getElementById('result-content');
    const toggle = document.getElementById('result-toggle');
    if (content.style.display === 'none') {
        content.style.display = 'block';
        toggle.textContent = '-';
    } else {
        content.style.display = 'none';
        toggle.textContent = '+';
    }
}

// ─── Artifacts ───

function toggleArtifacts() {
    const content = document.getElementById('artifacts-content');
    const toggle = document.getElementById('artifacts-toggle');
    if (content.style.display === 'none') {
        content.style.display = 'block';
        toggle.textContent = '-';
        loadArtifacts();
    } else {
        content.style.display = 'none';
        toggle.textContent = '+';
    }
}

async function loadArtifacts() {
    try {
        const resp = await fetch('/api/artifacts');
        const data = await resp.json();
        const tbody = document.getElementById('artifacts-body');
        tbody.innerHTML = '';
        if (data.ok && data.artifacts.length > 0) {
            data.artifacts.forEach(a => {
                const size = a.size_bytes < 1024
                    ? a.size_bytes + 'B'
                    : Math.round(a.size_bytes / 1024) + 'KB';
                const tr = document.createElement('tr');
                tr.innerHTML = `<td title="${a.key}">${a.key}</td>`
                    + `<td>${a.content_type}</td>`
                    + `<td>${size}</td>`
                    + `<td>PID ${a.stored_by_pid}</td>`;
                tbody.appendChild(tr);
            });
        } else if (data.ok) {
            tbody.innerHTML = '<tr><td colspan="4" style="color:#666">No artifacts</td></tr>';
        } else {
            tbody.innerHTML = `<tr><td colspan="4" style="color:#e94560">${data.error}</td></tr>`;
        }
    } catch (e) {
        toast('error', `Artifacts error: ${e.message}`);
    }
}

// ─── Context Menu ───

function showContextMenu(event, node) {
    ctxNode = node;
    const menu = document.getElementById('context-menu');
    menu.style.display = 'block';
    menu.style.left = event.clientX + 'px';
    menu.style.top = event.clientY + 'px';
}

function ctxViewDetails() {
    document.getElementById('context-menu').style.display = 'none';
    if (ctxNode) selectNode(ctxNode);
}

function ctxSpawnChild() {
    document.getElementById('context-menu').style.display = 'none';
    if (ctxNode) {
        selectedNode = ctxNode;
        openSpawnModal();
    }
}

function ctxExecuteTask() {
    document.getElementById('context-menu').style.display = 'none';
    if (ctxNode) {
        selectedNode = ctxNode;
        openExecuteModal();
    }
}

function ctxRunTask() {
    document.getElementById('context-menu').style.display = 'none';
    if (ctxNode) {
        selectedNode = ctxNode;
        openRunTaskModal();
    }
}

function ctxKill() {
    document.getElementById('context-menu').style.display = 'none';
    if (ctxNode) {
        selectedNode = ctxNode;
        killSelected();
    }
}

// Hide context menu on outside click
document.addEventListener('click', () => {
    document.getElementById('context-menu').style.display = 'none';
});

// ─── Modal helpers ───

function closeModal(id) {
    document.getElementById(id).style.display = 'none';
}

// Close modal on backdrop click
document.querySelectorAll('.modal').forEach(modal => {
    modal.addEventListener('click', (e) => {
        if (e.target === modal) {
            modal.style.display = 'none';
        }
    });
});

// ─── Toast ───

function toast(type, message) {
    const container = document.getElementById('toast-container');
    const el = document.createElement('div');
    el.className = `toast ${type}`;
    el.textContent = message;
    container.appendChild(el);
    setTimeout(() => {
        el.style.opacity = '0';
        el.style.transition = 'opacity 0.3s';
        setTimeout(() => el.remove(), 300);
    }, 3000);
}

// ─── Activity Log (per-node live events) ───

function renderActivityPanel(pid) {
    const panel = document.getElementById('activity-panel');
    const container = document.getElementById('activity-entries');
    const entries = nodeActivity[pid] || [];

    if (entries.length === 0) {
        panel.style.display = 'none';
        return;
    }

    panel.style.display = 'block';
    document.getElementById('activity-count').textContent = entries.length;
    container.innerHTML = '';

    // Show most recent first.
    const reversed = [...entries].reverse();
    reversed.forEach(entry => {
        const div = document.createElement('div');
        div.className = `activity-line activity-${entry.level}`;
        const ts = entry.ts ? new Date(entry.ts).toLocaleTimeString() : '';
        div.innerHTML =
            `<span class="activity-ts">${ts}</span>`
            + `<span class="activity-level">[${(entry.level || 'info').toUpperCase()}]</span> `
            + `<span class="activity-msg">${escapeHtml(entry.message)}</span>`;
        container.appendChild(div);
    });

    // Scroll to top (most recent).
    container.scrollTop = 0;
}

function toggleActivityPanel() {
    const content = document.getElementById('activity-content');
    const toggle = document.getElementById('activity-toggle');
    if (content.style.display === 'none') {
        content.style.display = 'flex';
        toggle.textContent = '-';
    } else {
        content.style.display = 'none';
        toggle.textContent = '+';
    }
}

// ─── Task History ───

async function loadTaskHistory(pid) {
    const panel = document.getElementById('history-panel');
    const container = document.getElementById('history-entries');

    try {
        const resp = await fetch(`/api/task-history?pid=${pid}&limit=20`);
        const data = await resp.json();
        if (!data.ok || !data.entries.length) {
            panel.style.display = 'none';
            return;
        }

        panel.style.display = 'block';
        container.innerHTML = '';
        data.entries.forEach(entry => {
            const div = document.createElement('div');
            div.className = 'history-entry' + (entry.success ? '' : ' history-error');
            const ago = timeAgo(entry.timestamp);
            const duration = entry.duration_ms >= 1000
                ? (entry.duration_ms / 1000).toFixed(1) + 's'
                : entry.duration_ms + 'ms';
            const desc = entry.description.length > 80
                ? entry.description.slice(0, 80) + '...'
                : entry.description;
            const statusDot = entry.success
                ? '<span class="history-dot history-dot-ok"></span>'
                : '<span class="history-dot history-dot-err"></span>';

            div.innerHTML =
                `<div class="history-header">`
                + `${statusDot}`
                + `<span class="history-desc">${escapeHtml(desc)}</span>`
                + `<span class="history-time">${ago}</span>`
                + `</div>`
                + `<div class="history-meta">${duration}`
                + (entry.error ? ` | Error: ${escapeHtml(entry.error.slice(0, 60))}` : '')
                + `</div>`;

            // Expandable output.
            if (entry.result) {
                const toggle = document.createElement('div');
                toggle.className = 'history-output-toggle';
                toggle.textContent = 'Show output';
                const output = document.createElement('pre');
                output.className = 'history-output';
                output.style.display = 'none';
                output.textContent = entry.result.slice(0, 500);
                toggle.onclick = () => {
                    if (output.style.display === 'none') {
                        output.style.display = 'block';
                        toggle.textContent = 'Hide output';
                    } else {
                        output.style.display = 'none';
                        toggle.textContent = 'Show output';
                    }
                };
                div.appendChild(toggle);
                div.appendChild(output);
            }

            container.appendChild(div);
        });
    } catch (e) {
        panel.style.display = 'none';
    }
}

function timeAgo(timestampMs) {
    const diff = Date.now() - timestampMs;
    if (diff < 60000) return Math.floor(diff / 1000) + 's ago';
    if (diff < 3600000) return Math.floor(diff / 60000) + 'm ago';
    if (diff < 86400000) return Math.floor(diff / 3600000) + 'h ago';
    return Math.floor(diff / 86400000) + 'd ago';
}

function toggleHistoryPanel() {
    const content = document.getElementById('history-content');
    const toggle = document.getElementById('history-toggle');
    if (content.style.display === 'none') {
        content.style.display = 'block';
        toggle.textContent = '-';
    } else {
        content.style.display = 'none';
        toggle.textContent = '+';
    }
}

// ─── Resize ───

window.addEventListener('resize', () => {
    if (treeData) renderTree();
});

// ─── Init ───

initTree();
connectWS();
