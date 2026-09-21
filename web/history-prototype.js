'use strict';
// THROWAWAY: three layouts answer whether a recorded NPU PID can lead to the
// same-sample process + working-directory evidence. No real recording or SSH.
window.startHistoryPrototype = function () {
  const params = new URLSearchParams(location.search);
  if (document.querySelector('meta[name="history-prototype"]').content !== 'true' || params.get('prototype') !== 'history') return;
  const h = value => String(value ?? '').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const names = {A:'时间轴 · 双栏证据', B:'事件列表 · 按次排查', C:'多机矩阵 · 先找异常'};
  const hosts = Array.from({length:16},(_,i)=>`ascend-${String(i+1).padStart(2,'0')}`);
  let variant = Object.hasOwn(names,params.get('variant')) ? params.get('variant') : 'C';
  let host = 0, cursor = 10, selectedPID = 7702, recording = false, playing = false, speed = 1, follow = false;
  let source = 'npu', rule = 'npu', keyword = 'tilexr', draftName = '', draftShell = '', commandMessage = '';
  const recordedCommands = [
    {id:'npu',name:'NPU 状态',original:'watch npu-smi info',shell:'npu-smi info',enabled:true},
    {id:'ps',name:'完整进程',original:'ps -ef',shell:'ps -ef',enabled:true},
    {id:'tilexr',name:'tilexr 任务',original:"watch -n 4 'ps -ef | grep tilexr'",shell:'ps -ef | grep tilexr',enabled:true}
  ];
  // UI-only preview of a deliberately limited watch syntax, not a shell parser.
  function previewCommand(input) {
    let shell=input.trim(), watchInterval=null;
    if(!shell) return {error:'请填写单次命令或常见 watch 命令。'};
    if(/^(?:\/[^\s]+\/)?watch(?:\s|$)/.test(shell)) {
      shell=shell.replace(/^(?:\/[^\s]+\/)?watch\s*/, '');
      while(shell.startsWith('-')) {
        let match=shell.match(/^(?:-n\s*|--interval(?:=|\s+))(\d+(?:\.\d+)?)(?:\s+|$)/);
        if(match) {watchInterval=Number(match[1]);shell=shell.slice(match[0].length).trim();continue;}
        match=shell.match(/^(?:-d|-t|-c|-p|--differences|--no-title|--color|--precise)(?:\s+|$)/);
        if(match){shell=shell.slice(match[0].length).trim();continue;}
        if(shell.startsWith('-- ')){shell=shell.slice(3).trim();break;}
        return {error:'这个 watch 选项暂不支持自动转换，请填写内部的单次命令。'};
      }
      if((shell.startsWith("'")&&shell.endsWith("'"))||(shell.startsWith('"')&&shell.endsWith('"'))) {
        const quote=shell[0], inner=shell.slice(1,-1);
        if(inner.includes(quote)||inner.includes('\\')) return {error:'复杂引号暂不自动转换，请填写单次命令。'};
        shell=inner;
      } else if(shell.startsWith("'")||shell.startsWith('"')) return {error:'请检查外层引号，或填写单次命令。'};
      if(!shell) return {error:'watch 后面还需要一条命令。'};
    }
    return {shell,watchInterval};
  }
  const base = Date.parse('2026-09-21T14:00:00+08:00');
  const frames = [], observations = [];
  const at = ms => new Date(ms).toLocaleTimeString('zh-CN',{hour12:false,timeZone:'Asia/Shanghai'});
  function frame(n, timestamp=base+n*4000) {
    return hosts.map((machine,m)=>{
      const overlap = m===0 ? n>=8 && n<=15 : m===3 ? n>=14 && n<=19 : m===8 ? n>=4 && n<=9 : false;
      const gap = m===1 && n>=11 && n<=13;
      const pid = 4101+m*100, rival = 7702+m*100;
      const processes = [{pid,ppid:4000+m*100,user:'root',start:'13:42:10',ticks:830000+m,boot:'demo-boot-'+m,cmd:'python train.py --config configs/base.yaml',cwd:'/home/team-a/training',npu:0,memory:28000,new:false},
        ...(overlap?[{pid:rival,ppid:7600+m*100,user:'root',start:m===0?'14:00:30':m===3?'14:00:54':'14:00:14',ticks:940000+m,boot:'demo-boot-'+m,cmd:'python /home/team-b/tilexr/benchmarks/benchmark.py --device 0 --suite matmul',cwd:n===15&&m===0?null:'/home/team-b/tilexr/benchmarks',cwdError:'进程在读取 cwd 前退出（模拟）',npu:0,memory:12000,new:true}]:[]),
        {pid:9900+m,ppid:1,user:'root',start:'12:10:00',ticks:800000+m,boot:'demo-boot-'+m,cmd:'python data_prepare.py',cwd:'/home/shared/datasets',npu:null,memory:0,new:false}];
      const npu = gap ? '[SSH 采集超时：这一帧没有 NPU 输出，不能解释为没有任务]' : [
        '+---------------------------------------------------------------+',
        '| NPU   Name          Health   AICore(%)   Memory-Usage(MB)       |',
        `| 0     Ascend 910B    OK       ${overlap?'99':'68'}          ${overlap?'40000':'28000'} / 65536        |`,
        '+---------------------------------------------------------------+',
        '| NPU   PID     Process name                    Memory(MB)      |',
        ...processes.filter(p=>p.npu!==null).map(p=>`| 0     ${p.pid}    python                          ${p.memory}           |`),
        '+---------------------------------------------------------------+'].join('\n');
      const snapshot = {id:`sample-${n}-${m}`,machine,machineID:`demo-${m+1}`,index:n,startedAt:timestamp,endedAt:timestamp+360,npuAt:timestamp,psAt:timestamp+170,cwdAt:timestamp+300,gap,overlap:overlap&&!gap,processes:gap?[]:processes,npu,ps:gap?'[未采集，保留缺失标记]':['UID        PID  PPID  C STIME    TTY          TIME CMD',...processes.map(p=>`${p.user.padEnd(8)} ${p.pid} ${p.ppid} 90 ${p.start} ?       00:12:31 ${p.cmd}`)].join('\n')};
      snapshot.outputs = Object.fromEntries(recordedCommands.filter(c=>c.enabled).map(c=>[c.id,{
        name:c.name,original:c.original,shell:c.shell,at:timestamp,
        stdout:gap?'':c.id==='npu'?npu:c.id==='ps'?snapshot.ps:c.id==='tilexr'?(overlap?`root ${rival} 7600 90 14:00 ? 00:12:31 python /home/team-b/tilexr/benchmarks/benchmark.py`:''):`[模拟输出，不执行输入命令]\n机器 ${machine}\n命令 ${c.shell}\n模拟阶段 ${n<8?'等待':n<16?'运行':'完成'}`,
        stderr:gap?'SSH 采集超时':'',exitCode:gap?null:c.id==='tilexr'&&!overlap?1:0,missing:gap
      }]));
      return snapshot;
    });
  }
  for(let n=0;n<24;n++) frames.push(frame(n));
  document.body.classList.add('history-prototype');
  const root = document.createElement('section'); root.id = 'history-prototype'; document.querySelector('main').append(root);
  const switcher = document.createElement('nav'); switcher.className='prototype-switcher'; switcher.setAttribute('aria-label','原型方案切换'); document.body.append(switcher);
  function current() {return frames[cursor][host];}
  function mark(s) {
    const output=s.outputs[source];
    if(!output) return {kind:'gap',text:'—',reason:'当时未记录此指令'};
    if(output.missing) return {kind:'gap',text:'?',reason:'采集失败，无法判断'};
    if(rule==='none') return {kind:'normal',text:'·',reason:'已记录；未启用判断规则'};
    if(rule==='contains') return !keyword?{kind:'gap',text:'?',reason:'请填写匹配文本'}:{kind:output.stdout.includes(keyword)?'suspect':'normal',text:output.stdout.includes(keyword)?'!':'·',reason:output.stdout.includes(keyword)?'命中文本规则；不等同于任务竞争':'未命中文本规则'};
    if(rule==='change') {
      const previous=frames[s.index-1]?.[hostIndex(s)]?.outputs[source];
      if(!previous||previous.missing||previous.shell!==output.shell) return {kind:'gap',text:'?',reason:'没有可比较的相邻记录'};
      const changed=previous.stdout!==output.stdout||previous.stderr!==output.stderr||previous.exitCode!==output.exitCode;
      return {kind:changed?'suspect':'normal',text:changed?'Δ':'·',reason:changed?'相邻两帧原始输出或退出状态不同；不等同于竞争':'相邻两帧输出与退出状态相同'};
    }
    if(source!=='npu') return {kind:'gap',text:'?',reason:'此指令未配置 NPU 解析器；不推断任务竞争'};
    const pids=[...new Set([...output.stdout.matchAll(/^\|\s+0\s+(\d+)\s+python\s+\d+\s*\|$/gm)].map(m=>m[1]))];
    if(!pids.length) return {kind:'gap',text:'?',reason:'原型格式解析失败；无法判断'};
    return {kind:pids.length>1?'suspect':'normal',text:String(pids.length),reason:`本地规则：NPU 0 有 ${pids.length} 个不同 PID${pids.length>1?'，提示同卡多进程；不认定竞争':''}`};
  }
  function hostIndex(s) {return hosts.indexOf(s.machine);}
  function commandPanel() {
    return `<details class="hp-command-settings"><summary>记录指令：内置与自定义（${recordedCommands.filter(c=>c.enabled).length} 条已勾选） · 展开配置</summary>
      <p>勾选项每 4 秒模拟记录一次，修改只影响后续采样；新指令不会补造旧记录。NPU / 完整 ps / cwd 是任务溯源的辅助证据。</p>
      ${recordedCommands.map(c=>`<label class="hp-command-option"><input type="checkbox" data-record-command="${c.id}" ${['npu','ps'].includes(c.id)?'disabled title="溯源辅助证据，原型固定采集"':''} ${c.enabled?'checked':''}><b>${h(c.name)}</b><code>${h(c.original)}</code><span>→ ${h(c.shell)}</span></label>`).join('')}
      <div class="hp-command-add"><input id="hp-command-name" aria-label="记录指令名称" placeholder="名称，例如日志末尾" value="${h(draftName)}"><input id="hp-command-shell" aria-label="记录自定义指令" placeholder="watch -n 4 'ps -ef | grep tilexr'" value="${h(draftShell)}"><button data-action="add-command">＋ 添加记录指令</button></div>
      <p role="status">${h(commandMessage||'支持常见 watch / watch -n 4 / watch -d 写法预览；复杂选项或引号不静默猜测。')}</p>
      <p>统一采样间隔 4 秒；watch 的刷新间隔只作提示，显示选项不进入采集命令。所有输入均为模拟，不执行 SSH。</p></details>`;
  }
  function recordedEvidence() {
    const s=current(),output=s.outputs[source],result=mark(s);
    return `<section class="hp-recorded-output"><h3>选中指令的历史原始输出 · ${h(s.machine)} / ${at(s.startedAt)}</h3>
      <p>${h(result.reason)} · 无 AI / 不联网分析</p>
      ${output?`<p>当时指令：<code>${h(output.original)}</code> → 单次执行：<code>${h(output.shell)}</code></p><small>${h(s.id)} · 退出码 ${output.exitCode??'未知'} · ${at(output.at)}</small><pre>${h(output.missing?'[本次采集失败]':output.stdout||'[stdout 为空]')}</pre>${output.stderr?`<pre>stderr: ${h(output.stderr)}</pre>`:''}`:'<p>这个时刻没有保存该指令。开始记录后，新采样才会出现结果。</p>'}
      <p>下方是同一轮的 NPU / ps / cwd 辅助证据；任意文本不会自动被解释成 PID 或目录。</p></section>`;
  }
  function choose(m,n,pid) {host=m;cursor=n;follow=false;selectedPID=pid ?? (current().processes.find(p=>p.new)?.pid || current().processes[0]?.pid);draw();}
  function events() {
    const list=[];
    hosts.forEach((machine,m)=>{
      let active=null;
      frames.forEach((row,n)=>{
        if(row[m].overlap) {if(!active) {active={machine,m,first:n,last:n,pid:row[m].processes.find(p=>p.new)?.pid};list.push(active);} active.last=n;}
        else active=null;
      });
    });
    return list.sort((a,b)=>b.first-a.first);
  }
  function timeline() {
    return `<div class="hp-timeline">${frames.map((row,n)=>`<button class="${row[host].gap?'gap':row[host].overlap?'suspect':'normal'} ${cursor===n?'selected':''}" data-frame="${n}" title="${at(row[host].startedAt)} · ${row[host].gap?'采集缺失':row[host].overlap?'同卡多进程，疑似竞争':'单任务'}" aria-label="跳至 ${at(row[host].startedAt)}">${n%4===0?at(row[host].startedAt).slice(3):'·'}</button>`).join('')}</div>`;
  }
  function evidence(compact=false) {
    const s=current(), p=s.processes.find(p=>p.pid===selectedPID);
    return `<div class="hp-evidence ${compact?'compact':''}">
      <div class="hp-evidence-title"><strong>${s.machine} / NPU 0 / ${at(s.startedAt)}</strong><span class="${s.overlap?'hp-amber':''}">${s.gap?'采集缺失':s.overlap?'疑似竞争 · 两个 PID':'单任务'}</span></div>
      <div class="hp-raw"><section><h3>① 当时的 npu-smi info</h3><small>采样 ${at(s.npuAt)} · ${h(s.id)}</small><pre>${h(s.npu)}</pre><div class="hp-pids">${s.processes.filter(p=>p.npu!==null).map(p=>`<button data-pid="${p.pid}" class="${p.pid===selectedPID?'primary':''}">PID ${p.pid}${p.new?' · 新出现':''}</button>`).join('')}</div></section>
      <section><h3>② 同轮 ps -ef</h3><small>NPU 后 170 ms 采集 · ${h(s.id)}</small><pre>${h(s.ps)}</pre></section></div>
      <section class="hp-process"><h3>③ PID → 任务目录</h3><p>点击 NPU PID 联动这里；工作目录来自当时的 /proc/PID/cwd，回放不查询当前机器。</p>
      <div class="hp-table-wrap"><table><thead><tr><th>PID / 用户</th><th>NPU</th><th>工作目录（采样时）</th><th>启动命令</th></tr></thead><tbody>${s.processes.map(p=>`<tr data-pid="${p.pid}" class="${p.pid===selectedPID?'chosen':''}"><td><button data-pid="${p.pid}">${p.pid}</button><small>${p.user} · ${p.start}</small></td><td>${p.npu===null?'未出现在 NPU 表':p.npu}</td><td><code>${h(p.cwd || '目录未取得')}</code>${!p.cwd?`<small class="hp-amber">${h(p.cwdError)}</small>`:''}</td><td><code>${h(p.cmd)}</code></td></tr>`).join('')}</tbody></table></div>
      <div class="hp-verdict">${s.gap?'这一帧缺失，不推断任务占用。':p?`已选 PID <b>${p.pid}</b> · ${p.npu===null?'该进程未出现在 NPU 表中。':p.cwd?`任务工作目录：<b>${h(p.cwd)}</b>`:'目录采集失败，不能拿当前目录或上一帧补齐。'}<br><small>进程身份：${h(p.boot)} / PID ${p.pid} / starttime ${p.ticks}；同轮采集跨度 360 ms，非原子快照。</small>`:'选择一个 PID 查看任务来源。'}</div></section></div>`;
  }
  function VariantA() {return `<div class="hp-a"><aside class="hp-hosts"><h3>机器历史</h3>${hosts.map((name,m)=>`<button data-host="${m}" class="${m===host?'active':''}"><span>${name}</span><small>${frames.some(r=>r[m].overlap)?'有疑似竞争':m===1?'有缺帧':'无标记'}</small></button>`).join('')}</aside><div>${timeline()}${evidence()}</div></div>`;}
  function VariantB() {return `<div class="hp-b"><aside class="hp-events"><h3>疑似竞争事件</h3><p>按时间倒序，先选事件，再对照前后帧。</p>${events().map(e=>`<button data-event="${e.m},${e.first},${e.pid}" class="${e.m===host&&cursor>=e.first&&cursor<=e.last?'active':''}"><small>${at(frames[e.first][e.m].startedAt)} — ${at(frames[e.last][e.m].startedAt)}</small><strong>${e.machine} · NPU 0</strong><span>同卡新增 PID ${e.pid}</span><small>首末观察点；真实开始/结束可能在采样间隔内</small></button>`).join('')}<div class="hp-tip">多 PID 也可能是合法协同任务。先查看目录和命令，再判断是否竞争。</div></aside><div><div class="hp-context"><button data-action="before">看事件前一帧</button><span>当前 ${current().machine} · ${at(current().startedAt)}</span></div>${evidence(true)}</div></div>`;}
  function VariantC() {return `<div class="hp-c">${commandPanel()}<div class="hp-rule-controls"><label>矩阵展示指令 <select id="hp-source" aria-label="矩阵展示指令">${recordedCommands.map(c=>`<option value="${c.id}" ${source===c.id?'selected':''}>${h(c.name)}</option>`).join('')}</select></label><label>本地标记规则 <select id="hp-rule" aria-label="本地标记规则"><option value="none" ${rule==='none'?'selected':''}>不判断，仅记录</option><option value="contains" ${rule==='contains'?'selected':''}>包含指定文本</option><option value="change" ${rule==='change'?'selected':''}>相比上一帧变化</option><option value="npu" ${rule==='npu'?'selected':''} ${source!=='npu'?'disabled':''}>NPU 同卡多个 PID（原型格式）</option></select></label>${rule==='contains'?`<label>匹配文本 <input id="hp-keyword" aria-label="匹配文本" value="${h(keyword)}"></label>`:''}<span>本机确定性规则 · 无 AI</span></div><section class="hp-matrix"><h3>16 台机器 × 历史时刻</h3><p>每格 4 秒。橙色是所选规则命中；绿色是已记录 / 未命中；灰色是缺失 / 无法判断。点击格子查看当时的指令、输出及辅助证据。</p><div class="hp-matrix-scroll"><table><thead><tr><th>机器 / 时间</th>${frames.map((r,n)=>`<th>${n%4===0?at(r[0].startedAt).slice(3):'·'}</th>`).join('')}</tr></thead><tbody>${hosts.map((machine,m)=>`<tr><th>${machine}</th>${frames.map((row,n)=>{const status=mark(row[m]);return `<td><button data-cell="${m},${n}" class="${status.kind} ${m===host&&n===cursor?'selected':''}" title="${h(status.reason)}" aria-label="${machine} ${at(row[m].startedAt)}">${status.text}</button></td>`;}).join('')}</tr>`).join('')}</tbody></table></div></section>${recordedEvidence()}${evidence(true)}</div>`;}
  function draw() {
    const s=current();
    root.innerHTML=`<div class="hp-notice"><b>PROTOTYPE / 模拟数据</b><span>验证问题：能否从历史 NPU 占用，追溯同轮进程与工作目录？记录只在内存，刷新丢失。</span><a href="/?demo=1">返回实时演示</a></div>
      <div class="hp-heading"><div><div class="eyebrow">WATCH HISTORY / RECORD & REPLAY</div><h2>回到任务开始竞争的那一刻</h2><p>2026-09-21 · Asia/Shanghai · 同一采样关联 NPU / ps / cwd</p></div><div><button data-action="record" class="${recording?'hp-recording':'primary'}">${recording?'■ 停止模拟记录':'● 开始模拟记录'}</button><small>${frames.length} 轮 · ${frames.length*16} 份机器快照 · 本机内存</small></div></div>
      <div class="hp-controls"><label>机器 <select id="hp-machine" aria-label="回放机器">${hosts.map((name,m)=>`<option value="${m}" ${m===host?'selected':''}>${name}</option>`).join('')}</select></label><button data-action="prev" ${cursor===0?'disabled':''}>上一帧</button><button data-action="play">${playing?'暂停回放':'播放回放'}</button><button data-action="next" ${cursor===frames.length-1?'disabled':''}>下一帧</button><select id="hp-speed" aria-label="回放速度">${[1,2,4].map(n=>`<option value="${n}" ${speed===n?'selected':''}>${n}×</option>`).join('')}</select><input id="hp-time" aria-label="历史时间轴" type="range" min="0" max="${frames.length-1}" value="${cursor}"><b>${at(s.startedAt)}</b><button data-action="latest">${follow?'跟随最新 ✓':'跟随最新'}</button></div>
      <div class="hp-legend"><span>颜色解释见所选方案</span><span class="hp-amber">标记仅辅助排查，不认定任务挤占</span><span>灰色：采集缺失</span><span>回放位置固定；新增记录不会把你拉回最新</span></div>
      ${{A:VariantA,B:VariantB,C:VariantC}[variant]()}
      <details class="hp-state"><summary>查看原型状态 / 数据模型</summary><pre>${h(JSON.stringify({variant,source,rule,keyword,recordedCommands,mode:playing?'replay-playing':'replay-paused',recording,followLatest:follow,frames:frames.length,selectedMachine:s.machine,selectedSample:s.id,selectedPID,timestamps:{npu:s.npuAt,ps:s.psAt,cwd:s.cwdAt,start:s.startedAt,end:s.endedAt},fields:['machineId','sampleId','rawNpu','rawPs','pid','bootId','starttime','cwd','cwdError'],persistence:'none / mock only',decisions:'同轮关联；保留缺失；用 bootId + PID + starttime 防止 PID 重用误关联'},null,2))}</pre></details>`;
    switcher.innerHTML=`<button data-variant="-1" aria-label="上一个原型方案">←</button><span>原型 ${variant} — ${names[variant]}</span><button data-variant="1" aria-label="下一个原型方案">→</button>`;
    observations.push({variant,sample:s.id,pid:selectedPID,recording});
    console.info('[history prototype]',observations.at(-1));
  }
  function changeVariant(delta) {const list=['A','B','C'];variant=list[(list.indexOf(variant)+delta+3)%3];params.set('variant',variant);history.replaceState(null,'','?'+params);draw();}
  switcher.onclick=e=>{const b=e.target.closest('[data-variant]');if(b) changeVariant(Number(b.dataset.variant));};
  document.addEventListener('keydown',e=>{if(e.target.closest('input,textarea,select,[contenteditable],button')||!['ArrowLeft','ArrowRight'].includes(e.key))return;e.preventDefault();changeVariant(e.key==='ArrowRight'?1:-1);});
  root.oninput=e=>{if(e.target.id==='hp-command-name')draftName=e.target.value;if(e.target.id==='hp-command-shell')draftShell=e.target.value;};
  root.onchange=e=>{
    if(e.target.dataset.recordCommand){recordedCommands.find(c=>c.id===e.target.dataset.recordCommand).enabled=e.target.checked;}
    if(e.target.id==='hp-source'){source=e.target.value;rule='none';draw();}
    if(e.target.id==='hp-rule'){rule=e.target.value;draw();}
    if(e.target.id==='hp-keyword'){keyword=e.target.value;draw();}
if(e.target.id==='hp-machine')choose(Number(e.target.value),cursor);if(e.target.id==='hp-time')choose(host,Number(e.target.value));if(e.target.id==='hp-speed'){speed=Number(e.target.value);draw();}};
  root.onclick=e=>{
    const t=e.target.closest('button,[data-pid]');if(!t)return;
    if(t.dataset.frame!==undefined)choose(host,Number(t.dataset.frame));
    if(t.dataset.host!==undefined)choose(Number(t.dataset.host),cursor);
    if(t.dataset.pid!==undefined){selectedPID=Number(t.dataset.pid);draw();}
    if(t.dataset.cell){const [m,n]=t.dataset.cell.split(',').map(Number);choose(m,n);}
    if(t.dataset.event){const [m,n,pid]=t.dataset.event.split(',').map(Number);choose(m,n,pid);}
    switch(t.dataset.action){
      case 'add-command': {
        const preview=previewCommand(draftShell);
        if(preview.error) commandMessage=preview.error;
        else {
          const command={id:'custom-'+recordedCommands.length,name:draftName.trim()||'自定义 '+recordedCommands.length,original:draftShell.trim(),shell:preview.shell,enabled:true};
          recordedCommands.push(command);source=command.id;rule='none';
          commandMessage=`已添加：${preview.shell}。${preview.watchInterval!==null?`输入 watch 间隔 ${preview.watchInterval}s；`:''}记录统一每 4s 采样。旧帧不补录。`;
          draftName='';draftShell='';
        }
        draw();root.querySelector('.hp-command-settings').open=true;break;
      }
      case 'record':recording=!recording;draw();break;
      case 'play':playing=!playing;follow=false;if(playing&&cursor===frames.length-1)cursor=0;draw();break;
      case 'prev':playing=false;choose(host,Math.max(0,cursor-1));break;
      case 'next':playing=false;choose(host,Math.min(frames.length-1,cursor+1));break;
      case 'latest':playing=false;follow=true;cursor=frames.length-1;draw();break;
      case 'before':{const event=events().find(e=>e.m===host&&cursor>=e.first&&cursor<=e.last);playing=false;choose(host,Math.max(0,(event?.first??cursor)-1));break;}
    }
  };
  let playElapsed=0;
  setInterval(()=>{if(!playing){playElapsed=0;return;}playElapsed+=250*speed;if(playElapsed<4000)return;playElapsed=0;if(cursor<frames.length-1){cursor++;selectedPID=current().processes.find(p=>p.new)?.pid||current().processes[0]?.pid;}else playing=false;draw();},250);
  setInterval(()=>{if(!recording)return;frames.push(frame(frames.length,frames.at(-1)[0].startedAt+4000));if(follow)cursor=frames.length-1;draw();},4000);
  draw();
};
