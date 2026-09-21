'use strict';
// THROWAWAY: three layouts answer whether a recorded NPU PID can lead to the
// same-sample process + working-directory evidence. No real recording or SSH.
window.startHistoryPrototype = function () {
  const params = new URLSearchParams(location.search);
  if (document.querySelector('meta[name="history-prototype"]').content !== 'true' || params.get('prototype') !== 'history') return;
  const h = value => String(value ?? '').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const names = {A:'时间轴 · 双栏证据', B:'事件列表 · 按次排查', C:'多机矩阵 · 先找异常'};
  const hosts = Array.from({length:16},(_,i)=>`ascend-${String(i+1).padStart(2,'0')}`);
  let variant = Object.hasOwn(names,params.get('variant')) ? params.get('variant') : 'A';
  let host = 0, cursor = 10, selectedPID = 7702, recording = false, playing = false, speed = 1, follow = false;
  const base = Date.parse('2026-09-21T14:00:00+08:00');
  const frames = [], observations = [];
  const at = ms => new Date(ms).toLocaleTimeString('zh-CN',{hour12:false,timeZone:'Asia/Shanghai'});
  function frame(n, timestamp=base+n*4000) {
    return hosts.map((machine,m)=>{
      const overlap = m===0 ? n>=8 && n<=15 : m===3 ? n>=14 && n<=19 : m===8 ? n>=4 && n<=9 : false;
      const gap = m===1 && n>=11 && n<=13;
      const pid = 4101+m*100, rival = 7702+m*100;
      const processes = [{pid,ppid:4000+m*100,user:'root',start:'13:42:10',ticks:830000+m,boot:'demo-boot-'+m,cmd:'python train.py --config configs/base.yaml',cwd:'/home/team-a/training',npu:0,memory:28000,new:false},
        ...(overlap?[{pid:rival,ppid:7600+m*100,user:'root',start:m===0?'14:00:30':m===3?'14:00:54':'14:00:14',ticks:940000+m,boot:'demo-boot-'+m,cmd:'python benchmark.py --device 0 --suite matmul',cwd:n===15&&m===0?null:'/home/team-b/tilexr/benchmarks',cwdError:'进程在读取 cwd 前退出（模拟）',npu:0,memory:12000,new:true}]:[]),
        {pid:9900+m,ppid:1,user:'root',start:'12:10:00',ticks:800000+m,boot:'demo-boot-'+m,cmd:'python data_prepare.py',cwd:'/home/shared/datasets',npu:null,memory:0,new:false}];
      const npu = gap ? '[SSH 采集超时：这一帧没有 NPU 输出，不能解释为没有任务]' : [
        '+---------------------------------------------------------------+',
        '| NPU   Name          Health   AICore(%)   Memory-Usage(MB)       |',
        `| 0     Ascend 910B    OK       ${overlap?'99':'68'}          ${overlap?'40000':'28000'} / 65536        |`,
        '+---------------------------------------------------------------+',
        '| NPU   PID     Process name                    Memory(MB)      |',
        ...processes.filter(p=>p.npu!==null).map(p=>`| 0     ${p.pid}    python                          ${p.memory}           |`),
        '+---------------------------------------------------------------+'].join('\n');
      return {id:`sample-${n}-${m}`,machine,machineID:`demo-${m+1}`,index:n,startedAt:timestamp,endedAt:timestamp+360,npuAt:timestamp,psAt:timestamp+170,cwdAt:timestamp+300,gap,overlap:overlap&&!gap,processes:gap?[]:processes,npu,ps:gap?'[未采集，保留缺失标记]':['UID        PID  PPID  C STIME    TTY          TIME CMD',...processes.map(p=>`${p.user.padEnd(8)} ${p.pid} ${p.ppid} 90 ${p.start} ?       00:12:31 ${p.cmd}`)].join('\n')};
    });
  }
  for(let n=0;n<24;n++) frames.push(frame(n));
  document.body.classList.add('history-prototype');
  const root = document.createElement('section'); root.id = 'history-prototype'; document.querySelector('main').append(root);
  const switcher = document.createElement('nav'); switcher.className='prototype-switcher'; switcher.setAttribute('aria-label','原型方案切换'); document.body.append(switcher);
  function current() {return frames[cursor][host];}
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
  function VariantC() {return `<div class="hp-c"><section class="hp-matrix"><h3>16 台机器 × 历史时刻</h3><p>每格 4 秒。点击任意格，下面的 NPU、进程和目录同步定位。</p><div class="hp-matrix-scroll"><table><thead><tr><th>机器 / 时间</th>${frames.map((r,n)=>`<th>${n%4===0?at(r[0].startedAt).slice(3):'·'}</th>`).join('')}</tr></thead><tbody>${hosts.map((machine,m)=>`<tr><th>${machine}</th>${frames.map((row,n)=>`<td><button data-cell="${m},${n}" class="${row[m].gap?'gap':row[m].overlap?'suspect':'normal'} ${m===host&&n===cursor?'selected':''}" title="${machine} ${at(row[m].startedAt)}" aria-label="${machine} ${at(row[m].startedAt)}">${row[m].gap?'?':row[m].overlap?'2':'1'}</button></td>`).join('')}</tr>`).join('')}</tbody></table></div></section>${evidence(true)}</div>`;}
  function draw() {
    const s=current();
    root.innerHTML=`<div class="hp-notice"><b>PROTOTYPE / 模拟数据</b><span>验证问题：能否从历史 NPU 占用，追溯同轮进程与工作目录？记录只在内存，刷新丢失。</span><a href="/?demo=1">返回实时演示</a></div>
      <div class="hp-heading"><div><div class="eyebrow">WATCH HISTORY / RECORD & REPLAY</div><h2>回到任务开始竞争的那一刻</h2><p>2026-09-21 · Asia/Shanghai · 同一采样关联 NPU / ps / cwd</p></div><div><button data-action="record" class="${recording?'hp-recording':'primary'}">${recording?'■ 停止模拟记录':'● 开始模拟记录'}</button><small>${frames.length} 轮 · ${frames.length*16} 份机器快照 · 本机内存</small></div></div>
      <div class="hp-controls"><label>机器 <select id="hp-machine" aria-label="回放机器">${hosts.map((name,m)=>`<option value="${m}" ${m===host?'selected':''}>${name}</option>`).join('')}</select></label><button data-action="prev" ${cursor===0?'disabled':''}>上一帧</button><button data-action="play">${playing?'暂停回放':'播放回放'}</button><button data-action="next" ${cursor===frames.length-1?'disabled':''}>下一帧</button><select id="hp-speed" aria-label="回放速度">${[1,2,4].map(n=>`<option value="${n}" ${speed===n?'selected':''}>${n}×</option>`).join('')}</select><input id="hp-time" aria-label="历史时间轴" type="range" min="0" max="${frames.length-1}" value="${cursor}"><b>${at(s.startedAt)}</b><button data-action="latest">${follow?'跟随最新 ✓':'跟随最新'}</button></div>
      <div class="hp-legend"><span>绿色：单任务观察</span><span class="hp-amber">橙色：同卡多进程，疑似竞争</span><span>灰色：采集缺失</span><span>回放位置固定；新增记录不会把你拉回最新</span></div>
      ${{A:VariantA,B:VariantB,C:VariantC}[variant]()}
      <details class="hp-state"><summary>查看原型状态 / 数据模型</summary><pre>${h(JSON.stringify({variant,mode:playing?'replay-playing':'replay-paused',recording,followLatest:follow,frames:frames.length,selectedMachine:s.machine,selectedSample:s.id,selectedPID,timestamps:{npu:s.npuAt,ps:s.psAt,cwd:s.cwdAt,start:s.startedAt,end:s.endedAt},fields:['machineId','sampleId','rawNpu','rawPs','pid','bootId','starttime','cwd','cwdError'],persistence:'none / mock only',decisions:'同轮关联；保留缺失；用 bootId + PID + starttime 防止 PID 重用误关联'},null,2))}</pre></details>`;
    switcher.innerHTML=`<button data-variant="-1" aria-label="上一个原型方案">←</button><span>原型 ${variant} — ${names[variant]}</span><button data-variant="1" aria-label="下一个原型方案">→</button>`;
    observations.push({variant,sample:s.id,pid:selectedPID,recording});
    console.info('[history prototype]',observations.at(-1));
  }
  function changeVariant(delta) {const list=['A','B','C'];variant=list[(list.indexOf(variant)+delta+3)%3];params.set('variant',variant);history.replaceState(null,'','?'+params);draw();}
  switcher.onclick=e=>{const b=e.target.closest('[data-variant]');if(b) changeVariant(Number(b.dataset.variant));};
  document.addEventListener('keydown',e=>{if(e.target.closest('input,textarea,select,[contenteditable],button')||!['ArrowLeft','ArrowRight'].includes(e.key))return;e.preventDefault();changeVariant(e.key==='ArrowRight'?1:-1);});
  root.onchange=e=>{if(e.target.id==='hp-machine')choose(Number(e.target.value),cursor);if(e.target.id==='hp-time')choose(host,Number(e.target.value));if(e.target.id==='hp-speed'){speed=Number(e.target.value);draw();}};
  root.onclick=e=>{
    const t=e.target.closest('button,[data-pid]');if(!t)return;
    if(t.dataset.frame!==undefined)choose(host,Number(t.dataset.frame));
    if(t.dataset.host!==undefined)choose(Number(t.dataset.host),cursor);
    if(t.dataset.pid!==undefined){selectedPID=Number(t.dataset.pid);draw();}
    if(t.dataset.cell){const [m,n]=t.dataset.cell.split(',').map(Number);choose(m,n);}
    if(t.dataset.event){const [m,n,pid]=t.dataset.event.split(',').map(Number);choose(m,n,pid);}
    switch(t.dataset.action){
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
