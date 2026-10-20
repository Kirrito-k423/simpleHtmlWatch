'use strict';
// The pinned xterm bundle calls this helper for its four generated stylesheets.
// The nonce is unique to this HTML response; it is unrelated to SSH credentials.
globalThis.__shwTerminalStyle = doc => {
  const style = doc.createElement('style');
  style.nonce = document.querySelector('meta[name="watch-style-nonce"]').content;
  return style;
};
(() => {
  const dialog = document.querySelector('#ssh-dialog');
  const machines = document.querySelector('#ssh-machine');
  const message = document.querySelector('#ssh-message');
  const container = document.querySelector('#ssh-terminal');
  const connect = document.querySelector('#ssh-connect');
  let terminal, fit, socket, generation = 0, runId = '';
  const status = text => { message.textContent = text; };
  function disconnect() {
    generation++;
    const old = socket; socket = null;
    if (old && old.readyState < WebSocket.CLOSING) old.close();
    terminal?.dispose(); terminal = null; fit = null;
    container.replaceChildren();
    connect.disabled = !machines.value;
  }
  function send(value) {
    if (socket?.readyState !== WebSocket.OPEN) return;
    if (socket.bufferedAmount > 65536) { status('输入繁忙，请稍后重试。'); return; }
    socket.send(JSON.stringify(value));
  }
  function resize() {
    if (!terminal || !fit || !dialog.open) return;
    fit.fit();
    const cols = Math.max(20, Math.min(320, terminal.cols));
    const rows = Math.max(2, Math.min(120, terminal.rows));
    if (cols !== terminal.cols || rows !== terminal.rows) terminal.resize(cols, rows);
    send({type:'resize', cols, rows});
  }
  async function openConnection() {
    disconnect();
    if (!machines.value || !dialog.open) return;
    const serial = generation;
    connect.disabled = true;
    status('正在连接…');
    terminal = new Terminal({cursorBlink:true, scrollback:1000, fontSize:14, fontFamily:'Menlo, Consolas, monospace', theme:{background:'#10151c', foreground:'#d8e2ef'}, allowProposedApi:false});
    fit = new FitAddon.FitAddon(); terminal.loadAddon(fit);
    terminal.open(container); fit.fit();
    terminal.resize(Math.max(20,Math.min(320,terminal.cols)), Math.max(2,Math.min(120,terminal.rows)));
    let health;
    try {
      const response = await fetch('/healthz');
      if (!response.ok) throw new Error('无法核对服务身份，请刷新页面。');
      health = await response.json();
      if (!health.capabilities?.includes('ssh-terminal-v1')) throw new Error('当前实例尚未提供 SSH 终端，请由维护者升级。');
    } catch (error) { if (serial === generation) { status(error.message); connect.disabled = false; } return; }
    if (serial !== generation || !dialog.open) return;
    runId = health.runId;
    const ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ssh');
    socket = ws; ws.binaryType = 'arraybuffer';
    ws.onopen = () => {
      if (serial !== generation) { ws.close(); return; }
      ws.send(JSON.stringify({type:'auth', token:document.querySelector('meta[name="watch-token"]').content, runId, machineId:machines.value, cols:terminal.cols, rows:terminal.rows}));
      terminal.focus();
    };
    ws.onmessage = event => {
      if (serial !== generation || !terminal) return;
      if (typeof event.data === 'string') {
        const update = JSON.parse(event.data);
        if (update.type === 'status') {
          status(update.message);
          if (update.state === 'connected') terminal.focus();
        }
      } else {
        const bytes = new Uint8Array(event.data);
        terminal.write(bytes, () => { if (serial === generation) send({type:'ack', bytes:bytes.byteLength}); });
      }
    };
    ws.onerror = () => { if (serial === generation) status('SSH 连接失败，请核对机器在线状态和主机指纹。'); };
    ws.onclose = () => { if (serial === generation) { socket = null; connect.disabled = false; status(message.textContent + ' · 已断开，可手动重新连接。'); } };
    terminal.onData(data => {
      if (new TextEncoder().encode(data).byteLength > 16384) { status('一次输入最多 16 KiB，请分段粘贴。'); return; }
      send({type:'input', data});
    });
  }
  async function open(machineId = '') {
    if (typeof demo !== 'undefined' && demo) { status('演示模式不连接 SSH。'); return; }
    if (!dialog.open) dialog.showModal();
    disconnect(); status('选择一台机器即可连接。');
    const serial = generation;
    try {
      const response = await fetch('/api/tasks/machines', {headers:{'X-Watch-Token':document.querySelector('meta[name="watch-token"]').content}});
      if (!response.ok) throw new Error('无法读取机器列表，请刷新页面。');
      const list = await response.json();
      if (serial !== generation || !dialog.open) return;
      machines.replaceChildren(new Option('选择机器…', ''));
      for (const machine of list.filter(item => item.enabled)) {
        machines.add(new Option(`${machine.name || machine.id} · ${machine.host}:${machine.port}`, machine.id));
      }
      machines.value = machineId;
      if (machineId && machines.value) openConnection();
      else connect.disabled = true;
    } catch (error) { if (serial === generation) status(error.message); }
  }
  window.openSSH = open;
  document.querySelector('#ssh-btn').onclick = () => open();
  document.querySelector('#ssh-close').onclick = () => dialog.close();
  document.querySelector('#ssh-disconnect').onclick = () => { disconnect(); status('已断开。'); };
  connect.onclick = openConnection;
  machines.onchange = openConnection;
  dialog.addEventListener('close', disconnect);
  window.addEventListener('beforeunload', disconnect);
  new ResizeObserver(resize).observe(container);
})();
