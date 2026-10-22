(() => {
  const el=id=>document.querySelector('#idle-ranking-'+id);
  const dialog=el('dialog'),token=document.querySelector('meta[name="watch-token"]').content;
  const escape=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const pct=value=>value==null?'—':Number(value).toFixed(1)+'%';
  const stamp=value=>value?new Date(Date.parse(value)+8*3600000).toISOString().slice(0,19).replace('T',' '):'—';
  const reasons={ready:'当前资源检查通过',machine_disabled:'已停用',npu_collection_disabled:'未启用NPU监控',npu_sample_identity_changed:'采样入口或物理资源已变化',ssh_key_changed:'SSH指纹已变化',npu_sample_unknown:'NPU采样失败或不完整',npu_sample_stale:'NPU采样已过期',npu_sample_duplicate:'NPU采样重复',npu_process_unknown:'进程表不完整或无法识别',npu_process_busy:'NPU有进程占用',npu_devices_unknown:'设备信息未知',npu_devices_incomplete:'设备信息不完整',npu_device_count_mismatch:'设备数量不符',npu_model_mismatch:'型号不符',npu_utilization_busy:'NPU算力不为0',hbm_insufficient:'空闲HBM不足',history_insufficient:'5分钟历史不足',history_storage_error:'5分钟历史保存异常',recent_1h_at_least_25:'最近1小时占用≥25%'};
  let snapshot=null,controller=null,generation=0,timer=null,openEpoch=0;
  function message(value,error=false){el('message').textContent=value;el('message').classList.toggle('error',error);}
  function clear(){snapshot=null;el('table').replaceChildren();el('export').disabled=true;el('meta').textContent='';el('path').textContent='';}
  function render(){
    if(!snapshot)return;
    const rows=snapshot.machines.filter(row=>!el('available').checked||row.eligible);
    const windowCell=(row,key)=>{
      const w=row.windows[key];
      return '<td class="'+(!w.complete?'idle-history-partial':'')+'">'+pct(w.occupiedPercent)+'<small>覆盖 '+pct(w.coveragePercent)+'</small></td>';
    };
    el('table').innerHTML='<table><thead><tr><th>顺序 / 机器</th><th>历史档位</th><th>当前资源</th><th>近5分钟</th><th>近1小时</th><th>近2小时</th><th>近3小时</th><th>近4小时</th><th>设备 / 最小空闲HBM</th><th>NPU采样时间</th></tr></thead><tbody>'+rows.map(row=>'<tr class="'+(row.recommended?'idle-recommended':'')+'"><th><strong>'+(row.rank?'#'+row.rank+' · ':'')+escape(row.name||row.machineId)+'</strong><small>'+escape(row.group)+' · '+escape(row.model||'型号未知')+'</small></th><td><span class="idle-tier idle-tier-'+row.tier+'">'+escape(row.tier?row.tier+' · '+row.tierLabel:reasons[row.historyReason]||row.historyReason)+'</span></td><td class="'+(row.eligible?'idle-current-pass':'idle-current-fail')+'">'+escape(reasons[row.currentReason]||row.currentReason)+(row.eligible&&!row.recommended?'<small>历史偏好暂不可用</small>':'')+'</td>'+['5m','1h','2h','3h','4h'].map(key=>windowCell(row,key)).join('')+'<td>'+(row.devices||'—')+' / '+(row.minFreeHbmMB==null?'—':Number(row.minFreeHbmMB).toLocaleString()+' MB')+'</td><td>'+stamp(row.lastNpuSampleAt)+'</td></tr>').join('')+'</tbody></table>';
    if(!rows.length)el('table').textContent='当前筛选没有符合条件的机器。';
    const recommended=snapshot.machines.filter(row=>row.recommended).length;
    const available=snapshot.machines.filter(row=>row.eligible).length;
    const problems=[snapshot.exportError&&'JSON自动保存异常：'+snapshot.exportError,snapshot.historyStorageError&&'5分钟历史保存异常：'+snapshot.historyStorageError].filter(Boolean);
    message(problems.length?problems.join('；'):'可推荐 '+recommended+' 台 · 当前资源符合 '+available+' / '+snapshot.machines.length+' 台'+(recommended?'':' · 可能正在积累历史，或当前资源不满足条件'),!!problems.length);
    el('meta').textContent='历史截至 '+stamp(snapshot.historyThrough)+' · 资源检查 '+stamp(snapshot.generatedAt)+' · 文件上次保存 '+stamp(snapshot.exportedAt)+' · 下次自动保存 '+stamp(snapshot.nextRefreshAt)+' · 5分钟历史 '+(snapshot.historyBytes/1024).toFixed(1)+' / '+(snapshot.historyLimitBytes/1024).toFixed(0)+' KiB';
    el('path').textContent='本机自动导出：'+snapshot.exportPath+'（默认资源条件；当前筛选可单独下载）';
    el('export').disabled=false;
  }
  async function load(){
    const request=++generation;
    controller?.abort();controller=new AbortController();
    el('export').disabled=true;
    if(typeof demo!=='undefined'&&demo){clear();message('演示模式没有真实空闲排序；请退出演示。');return;}
    try{
      for(const id of ['hbm','devices'])if(!el(id).checkValidity())throw new Error('请检查HBM或设备数量的输入范围');
      const q=new URLSearchParams();
      if(el('group').value)q.set('group',el('group').value);
      if(el('model').value.trim())q.set('model',el('model').value.trim());
      if(el('devices').value)q.set('devicesPerMachine',el('devices').value);
      if(el('hbm').value)q.set('minFreeHbmMB',el('hbm').value);
      const response=await fetch('/api/idle-ranking?'+q,{headers:{'X-Watch-Token':token},signal:controller.signal});
      const data=await response.json();
      if(!response.ok)throw new Error(data.error||'读取空闲排序失败');
      if(request!==generation||!dialog.open)return;
      if(data.schema!=='simplehtmlwatch.idle-ranking.v1'||!Array.isArray(data.machines))throw new Error('空闲排序格式无效');
      snapshot=data;render();
    }catch(error){if(error.name!=='AbortError'&&request===generation){clear();message(error.message,true);}}
  }
  el('btn').onclick=async()=>{
    const opening=++openEpoch;clearInterval(timer);timer=null;
    dialog.showModal();clear();message('读取空闲排序与当前资源…');
    try{
      if(typeof demo!=='undefined'&&!demo)await loadConfig();
      if(!dialog.open||opening!==openEpoch)return;
      const groups=[...new Set((config.machines||[]).map(m=>m.group).filter(Boolean))].sort();
      el('group').innerHTML='<option value="">所有分组</option>'+groups.map(g=>'<option value="'+escape(g)+'">'+escape(g)+'</option>').join('');
      await load();
      if(dialog.open&&opening===openEpoch)timer=setInterval(load,10000);
    }catch(error){message(error.message,true);}
  };
  el('close').onclick=()=>dialog.close();
  dialog.addEventListener('close',()=>{openEpoch++;generation++;controller?.abort();clearInterval(timer);timer=null;clear();});
  for(const id of ['group','model','hbm','devices'])el(id).onchange=load;
  el('available').onchange=render;el('refresh').onclick=load;
  el('export').onclick=()=>{
    if(!snapshot)return;
    const url=URL.createObjectURL(new Blob([JSON.stringify(snapshot,null,2)+'\n'],{type:'application/json;charset=utf-8'}));
    const link=document.createElement('a');link.href=url;link.download='idle-ranking.json';link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
  };
})();
