(() => {
  const el=id=>document.querySelector('#availability-'+id),dialog=el('dialog'),detail=el('detail-dialog'),charts=AvailabilityCharts;
  const token=document.querySelector('meta[name="watch-token"]').content;
  const {escape,stamp,durations,scopeNames}=charts;
  let snapshot=null,controller=null,generation=0,timer=null,frame=null,detailSelection=null;
  const reasonNames={pool_unconfirmed:'未确认唯一 B 组',historical_pool_changed:'当天历史记录与当前 B 组不符，不能借用旧组历史',physical_identity_unconfirmed:'未确认物理资源身份',pool_over_eight:'同组超过 8 个物理节点，需核对分组',not_enough_members:'可确认的组内节点不足'};
  function message(value,error=false){el('message').textContent=value;el('message').title=value;el('message').classList.toggle('error',error);}
  function clear(){snapshot=null;el('chart').replaceChildren();el('meta').textContent='';for(const id of ['png','svg','csv'])el(id).disabled=true;if(detail.open)detail.close();detailSelection=null;}
  function draw(){
    if(!snapshot||!dialog.open)return;
    const rect=el('chart').getBoundingClientRect();el('chart').innerHTML=charts.render(snapshot,{width:rect.width,height:rect.height});
  }
  function queueDraw(){if(frame!=null)cancelAnimationFrame(frame);frame=requestAnimationFrame(()=>{frame=null;draw();});}
  const observer=new ResizeObserver(queueDraw);observer.observe(el('chart'));
  function render(){
    const data=snapshot,multi=(data.nodes||1)>1;
    el('meta').textContent=scopeNames[data.scope]+' · '+stamp(data.before)+' · '+data.machineCount+' 节点 / '+data.configuredEntries+' 入口 · 覆盖 '+Number(data.coveragePercent).toFixed(1)+'% · 所选时段已过 '+Number(data.expectedSeconds/3600).toFixed(2)+' 小时';
    const problems=[data.storageError&&'历史保存异常：'+data.storageError,data.historyTruncated&&'历史因容量限制裁剪，缺失按未观测处理'].filter(Boolean);
    const excluded=multi?data.machines.filter(row=>row.multiReason).length:0;
    message(problems.length?problems.join('；'):!data.expectedSeconds?'今天尚未进入所选时段。':!data.machines.length?'当前分组没有节点。':multi?'多机为保守可行排布，非最大值；图头是独立实验次数，圆环中心是节点参与次数。'+(excluded?' '+excluded+' 个节点未参与组网，点击查看原因。':''):'五档分别统计，同一档内互不重叠；每张图中心为对应档位窗口个数。',!!problems.length);
    for(const id of ['png','svg','csv'])el(id).disabled=!data.machines.length;
    draw();if(detail.open&&detailSelection)renderDetail();
  }
  async function load(){
    const request=++generation;controller?.abort();controller=new AbortController();clear();message('读取'+scopeNames[el('scope').value]+'窗口统计…');
    if(typeof demo!=='undefined'&&demo){message('演示模式没有真实窗口历史；请退出演示。');return;}
    try{
      const query=new URLSearchParams({scope:el('scope').value,nodes:el('nodes').value});if(el('group').value)query.set('group',el('group').value);
      const response=await fetch('/api/availability?'+query,{headers:{'X-Watch-Token':token},signal:controller.signal});
      const data=await response.json();if(!response.ok)throw new Error(data.error||'读取窗口统计失败');
      if(request!==generation||!dialog.open)return;
      if(data.schema!=='simplehtmlwatch.daily-idle-windows.v1'||!Array.isArray(data.machines)||!data.counts||!scopeNames[data.scope]||Number(data.nodes||1)!==Number(el('nodes').value))throw new Error('窗口统计格式或组网规模无效，请更新服务');
      snapshot=data;render();
    }catch(error){if(error.name!=='AbortError'&&request===generation){clear();message(error.message,true);}}
  }
  el('btn').onclick=async()=>{
    clearInterval(timer);dialog.showModal();clear();message('读取空闲窗口…');const opening=generation;
    try{
      if(typeof demo!=='undefined'&&!demo)await loadConfig();
      if(!dialog.open||opening!==generation)return;
      const groups=[...new Set((config.machines||[]).map(m=>m.group).filter(Boolean))].sort();
      el('group').innerHTML='<option value="">所有分组</option>'+groups.map(g=>'<option value="'+escape(g)+'">'+escape(g)+'</option>').join('');
      await load();if(dialog.open)timer=setInterval(()=>{if(!detail.open)load();},30000);
    }catch(error){message(error.message,true);}
  };
  el('close').onclick=()=>dialog.close();
  dialog.addEventListener('close',()=>{generation++;controller?.abort();clearInterval(timer);timer=null;clear();});
  el('refresh').onclick=load;el('group').onchange=load;el('scope').onchange=load;el('nodes').onchange=load;
  function openDetail(target){
    const tile=target.closest('[data-node-resource]');if(!tile||!snapshot)return;
    detailSelection={resource:tile.dataset.nodeResource,minutes:Number(tile.dataset.nodeDuration)};renderDetail();detail.showModal();
  }
  function renderDetail(){
    if(!snapshot||!detailSelection)return;
    const row=snapshot.machines.find(r=>r.resourceKey===detailSelection.resource);if(!row)return;
    const duration=durations.find(d=>d.minutes===detailSelection.minutes),multi=(snapshot.nodes||1)>1;
    el('detail-title').textContent=row.name+' · '+duration.label;
    let html='<p>'+escape(row.group)+' · '+escape(row.resourceKey)+' · 覆盖 '+Number(row.coveragePercent).toFixed(1)+'%</p>';
    if(row.multiReason)html+='<p class="availability-detail-warning">'+escape(reasonNames[row.multiReason]||row.multiReason)+'</p>';
    html+='<table><thead><tr><th>时间档位</th><th>'+(multi?'本节点参与次数':'独立空闲窗')+'</th></tr></thead><tbody>'+durations.map(d=>'<tr><td>'+d.label+'</td><td>'+row.counts[d.key]+'</td></tr>').join('')+'</tbody></table>';
    html+='<p>空闲 '+(row.idleSeconds/60).toFixed(1)+' 分钟 · 占用 '+(row.busySeconds/60).toFixed(1)+' 分钟 · 未观测 '+(row.unknownSeconds/60).toFixed(1)+' 分钟</p>';
    if(multi){
      const plans=(snapshot.plans?.[duration.key]||[]).filter(p=>p.resources.includes(row.resourceKey)),names=new Map(snapshot.machines.map(m=>[m.resourceKey,charts.shortName(m.name)]));
      html+='<h3>'+snapshot.nodes+' 机同组排布 · '+duration.label+'</h3><p>每条记录全程使用同一批节点，连续重复的相邻窗口合并显示。保守可行排布不代表全局最大值。</p>';
      html+=plans.length?'<table><thead><tr><th>北京时间 / 次数</th><th>组内固定节点</th></tr></thead><tbody>'+plans.map(p=>'<tr><td>'+stamp(p.after)+'<br>'+stamp(p.before)+'<br>'+p.count+' × '+duration.label+'</td><td>'+escape(p.pool)+'<br>'+p.resources.map(key=>escape(names.get(key)||key)).join('、')+'</td></tr>').join('')+'</tbody></table>':'<p>该档位没有已列出的参与窗口。</p>';
      if(snapshot.omittedPlans?.[duration.key])html+='<p>明细受容量上限限制，另有 '+snapshot.omittedPlans[duration.key]+' 个窗口未列出；总计数包含全部可行排布。</p>';
    }
    el('detail-body').innerHTML=html;
  }
  el('chart').onclick=event=>openDetail(event.target);
  el('chart').onkeydown=event=>{if(event.key==='Enter'||event.key===' '){event.preventDefault();openDetail(event.target);}};
  el('detail-close').onclick=()=>detail.close();detail.addEventListener('close',()=>{detailSelection=null;el('detail-body').replaceChildren();});
  function download(blob,ext,day,scope,nodes){const url=URL.createObjectURL(blob),a=document.createElement('a');a.href=url;a.download='machine-idle-windows-'+nodes+'nodes-'+scope+'-'+day+'.'+ext;a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);}
  el('svg').onclick=()=>{if(snapshot)download(new Blob([charts.render(snapshot)],{type:'image/svg+xml;charset=utf-8'}),'svg',stamp(snapshot.after).slice(0,10),snapshot.scope,snapshot.nodes||1);};
  el('png').onclick=async()=>{
    if(!snapshot)return;const data=snapshot,source=charts.render(data);el('png').disabled=true;
    try{
      const image=new Image();image.src='data:image/svg+xml;charset=utf-8,'+encodeURIComponent(source);await image.decode();
      const canvas=document.createElement('canvas');canvas.width=image.width;canvas.height=image.height;canvas.getContext('2d').drawImage(image,0,0);
      const blob=await new Promise(resolve=>canvas.toBlob(resolve,'image/png'));if(!blob)throw new Error('图片导出失败');download(blob,'png',stamp(data.after).slice(0,10),data.scope,data.nodes||1);
    }catch(error){message('图片导出失败：'+error.message,true);}finally{if(snapshot)el('png').disabled=false;}
  };
  el('csv').onclick=()=>{
    if(!snapshot)return;const quote=value=>'"'+String(value??'').replace(/"/g,'""').replace(/^[=+@-]/,"'$&")+'"';
    const rows=[['日期','范围','组网规模','计数口径','资源标识','机器','B组','2小时','1小时','半小时','15分钟','5分钟','最长空闲秒数','空闲秒数','占用秒数','未观测秒数','覆盖率(%)']],day=stamp(snapshot.after).slice(0,10),multi=(snapshot.nodes||1)>1;
    for(const r of snapshot.machines)rows.push([day,scopeNames[snapshot.scope],snapshot.nodes||1,multi?'节点参与次数':'独立空闲窗',r.resourceKey,r.name,r.pool||r.group,r.counts.minutes120,r.counts.minutes60,r.counts.minutes30,r.counts.minutes15,r.counts.minutes5,r.longestIdleSeconds,r.idleSeconds,r.busySeconds,r.unknownSeconds,r.coveragePercent]);
    rows.push([day,scopeNames[snapshot.scope],snapshot.nodes||1,multi?'独立组网实验窗':'独立空闲窗','TOTAL','全体汇总','',...durations.map(d=>snapshot.counts[d.key])]);
    download(new Blob(['\uFEFF'+rows.map(r=>r.map(quote).join(',')).join('\r\n')],{type:'text/csv;charset=utf-8'}),'csv',day,snapshot.scope,snapshot.nodes||1);
  };
  (async()=>{if(await window.watchReady){const params=new URLSearchParams(location.search);if(!params.has('tasks')&&params.get('demo')!=='1')el('btn').click();}})();
})();
