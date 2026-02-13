// ─── HiveKernel Dashboard ───

// State
let ws = null;
let treeData = null;       // raw nodes array from server
let selectedNode = null;    // currently selected node dict
let ctxNode = null;         // node for context menu

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
        .attr('fill', d => ROLE_COLORS[d.data.role] || '#576574')
        .attr('stroke', d => STATE_COLORS[d.data.state] || '#576574');

    nodeUpdate.select('.node-name')
        .text(d => d.data.name || '?');

    nodeUpdate.select('.node-pid')
        .text(d => 'PID ' + d.data.pid);

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

// ─── Execute ───

function openExecuteModal() {
    if (!selectedNode) return;
    document.getElementById('exec-target-pid').textContent = selectedNode.pid;
    document.getElementById('execute-modal').style.display = 'flex';
}

async function doExecute() {
    if (!selectedNode) return;
    const pid = selectedNode.pid;
    const description = document.getElementById('exec-description').value || 'Dashboard task';
    let params = {};
    try {
        params = JSON.parse(document.getElementById('exec-params').value || '{}');
    } catch (e) {
        toast('error', 'Invalid JSON in params');
        return;
    }
    const timeout = parseInt(document.getElementById('exec-timeout').value) || 60;

    closeModal('execute-modal');
    toast('success', `Executing task on PID ${pid}...`);

    try {
        const resp = await fetch(`/api/execute/${pid}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ description, params, timeout }),
        });
        const data = await resp.json();
        if (data.ok) {
            const r = data.result;
            toast('success', `Task done (exit ${r.exit_code}): ${r.output.slice(0, 80)}`);
            requestRefresh();
        } else {
            toast('error', `Execute failed: ${data.error}`);
        }
    } catch (e) {
        toast('error', `Execute error: ${e.message}`);
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

// ─── Resize ───

window.addEventListener('resize', () => {
    if (treeData) renderTree();
});

// ─── Init ───

initTree();
connectWS();
