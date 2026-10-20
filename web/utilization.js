(() => {
  const dialog=document.querySelector('#utilization-dialog');
  const el=id=>document.querySelector('#utilization-'+id);
  const token=document.querySelector('meta[name="watch-token"]').content;
  const escape=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const fmt=value=>value==null?'—':Number(value).toFixed(1)+'%';
  const date=at=>new Date(at*1000+8*3600000).toISOString().slice(0,10);
  const stamp=at=>new Date(at*1000+8*3600000).toISOString().slice(0,16).replace('T',' ');
  let snapshot=null,controller=null,generation=0;
  function message(value,error=false){el('message').textContent=value;el('message').classList.toggle('error',error);}
  function clear(){snapshot=null;el('table').replaceChildren();el('detail').replaceChildren();el('windows').replaceChildren();el('export').disabled=true;}
  function selectedHours(row){
    const selected=el('day').value;
    return selected==='pattern'?row.hourOfDay:row.hours.filter(h=>date(h.at)===selected);
  }
  function cell(h){
    const style=h.mean==null?'util-missing':h.mean<10?'util-low':h.mean<50?'util-mid':'util-high';
    const title='算力均值 '+fmt(h.mean)+'；峰值 '+fmt(h.peak)+'；低于 10% '+fmt(h.lowPercent)+'；算力覆盖 '+fmt(h.coveragePercent)+'；整机占用 '+fmt(h.occupiedPercent)+'；占用覆盖 '+fmt(h.occupancyCoveragePercent);
    const occupancy=h.occupiedPercent==null?'util-missing':h.occupiedPercent===0?'util-idle':'util-occupied';
    return '<td title="'+escape(title)+'"><span class="util-value '+style+(h.mean!=null&&h.coveragePercent<90?' util-partial':'')+'">算力 '+fmt(h.mean)+'</span><span class="util-value '+occupancy+(h.occupiedPercent!=null&&h.occupancyCoveragePercent<90?' util-partial':'')+'">占用 '+fmt(h.occupiedPercent)+'</span></td>';
  }
  function detail(index){
    const row=snapshot.machines[index];
    el('detail').innerHTML='<h3>'+escape(row.name||row.id)+' · '+escape(row.model||'尚无可识别数据')+' · '+escape(row.metric)+'</h3><table><thead><tr><th>北京时间</th><th>平均算力</th><th>占用时长比例</th><th>算力覆盖率</th><th>占用覆盖率</th><th>整机峰值</th><th>低于 10%</th><th>采样数</th></tr></thead><tbody>'+row.hours.map(h=>'<tr><td>'+stamp(h.at)+'</td><td>'+fmt(h.mean)+'</td><td>'+fmt(h.occupiedPercent)+'</td><td>'+fmt(h.coveragePercent)+'</td><td>'+fmt(h.occupancyCoveragePercent)+'</td><td>'+fmt(h.peak)+'</td><td>'+fmt(h.lowPercent)+'</td><td>'+h.samples+'</td></tr>').join('')+'</tbody></table>';
  }
  function render(){
    if(!snapshot)return;
    el('detail').replaceChildren();
    const rows=snapshot.machines;
    el('table').innerHTML='<table class="utilization-table"><thead><tr><th>机器 / 小时</th>'+Array.from({length:24},(_,i)=>'<th>'+String(i).padStart(2,'0')+'</th>').join('')+'<th>全窗口汇总</th><th>覆盖率</th></tr></thead><tbody>'+rows.map((row,i)=>'<tr><th><button type="button" data-util-machine="'+i+'">'+escape(row.name||row.id)+'</button></th>'+selectedHours(row).map(cell).join('')+'<td class="util-summary"><span class="util-value">算力 '+fmt(row.mean)+'</span><span class="util-value">占用 '+fmt(row.occupiedPercent)+'</span></td><td><span class="util-value">算力 '+fmt(row.coveragePercent)+'</span><span class="util-value">占用 '+fmt(row.occupancyCoveragePercent)+'</span></td></tr>').join('')+'</tbody></table>';
    const observed=rows.filter(row=>row.mean!=null).length;
    const occupiedObserved=rows.filter(row=>row.occupiedPercent!=null).length;
    message(snapshot.storageError?'历史写入异常：'+snapshot.storageError:observed?'算力历史 '+observed+' / '+rows.length+' 台；占用历史 '+occupiedObserved+' / '+rows.length+' 台。每格上行算力、下行占用；斜纹表示对应指标覆盖不足。':'所选范围尚无利用率历史。采集启用前的日期无法补回。',!!snapshot.storageError);
    el('storage').textContent='保留 '+snapshot.retentionDays+' 天 · 已落盘 '+(snapshot.bytes/1048576).toFixed(3)+' / '+(snapshot.limitBytes/1048576).toFixed(2)+' MiB · 只存机器级小时桶 · 数据更新 '+stamp(Math.floor(Date.parse(snapshot.generatedAt)/1000));
    const windows=rows.flatMap(row=>row.lowWindows.map(w=>'<p>'+escape(row.name||row.id)+'：'+stamp(w.after)+' — '+stamp(w.before)+'</p>'));
    el('windows').innerHTML=windows.length?windows.join(''):'<p>没有覆盖充足的完整低负载小时。候选需每小时覆盖 ≥90%，且已观测时间中 ≥90% 的整机均值低于 10%。</p>';
    el('export').disabled=false;
  }
  async function load(){
    const request=++generation;
    controller?.abort();controller=new AbortController();
    clear();el('storage').textContent='';message('读取小时汇总…');
    if(typeof demo!=='undefined'&&demo){message('演示模式没有真实利用率历史；请退出演示。');return;}
    try{
      const q=new URLSearchParams({period:el('period').value});
      if(el('group').value)q.set('group',el('group').value);
      const response=await fetch('/api/utilization?'+q,{headers:{'X-Watch-Token':token},signal:controller.signal});
      const data=await response.json();
      if(!response.ok)throw new Error(data.error||'读取失败');
      if(request!==generation||!dialog.open)return;
      if(!Array.isArray(data.machines))throw new Error('汇总格式无效');
      const previous=el('day').value;
      snapshot=data;
      const dates=[];for(let at=data.after;at<data.before;at+=86400)dates.push(date(at));
      el('day').innerHTML=(dates.length>1?'<option value="pattern">一周各小时规律</option>':'')+dates.map(d=>'<option value="'+d+'">'+d+'</option>').join('');
      if([...el('day').options].some(o=>o.value===previous))el('day').value=previous;
      render();
    }catch(error){if(error.name!=='AbortError'&&request===generation){clear();message(error.message,true);}}
  }
  el('btn').onclick=async()=>{
    dialog.showModal();
    try{
      if(typeof demo!=='undefined'&&!demo)await loadConfig();
      const groups=[...new Set((config.machines||[]).map(m=>m.group).filter(Boolean))].sort();
      el('group').innerHTML='<option value="">所有分组</option>'+groups.map(g=>'<option value="'+escape(g)+'">'+escape(g)+'</option>').join('');
      await load();
    }catch(error){message(error.message,true);}
  };
  el('close').onclick=()=>dialog.close();
  dialog.addEventListener('close',()=>{generation++;controller?.abort();clear();});
  el('period').onchange=load;el('group').onchange=load;el('refresh').onclick=load;el('day').onchange=render;
  el('table').onclick=event=>{const button=event.target.closest('[data-util-machine]');if(button&&snapshot)detail(Number(button.dataset.utilMachine));};
  el('export').onclick=()=>{
    if(!snapshot)return;
    const csvCell=value=>'"'+String(value??'').replace(/"/g,'""').replace(/^[=+@-]/,"'$&")+'"';
    const rows=[['机器ID','机器','分组','型号','指标','北京时间','平均算力利用率(%)','整机占用时长比例(%)','算力覆盖率(%)','占用覆盖率(%)','占用状态观测秒数','整机峰值(%)','低于10%的时间(%)','采样数']];
    for(const row of snapshot.machines)for(const h of row.hours)rows.push([row.id,row.name,row.group,row.model,row.metric,stamp(h.at),h.mean,h.occupiedPercent,h.coveragePercent,h.occupancyCoveragePercent,h.occupancyObservedSeconds,h.peak,h.lowPercent,h.samples]);
    const blob=new Blob(['\uFEFF'+rows.map(row=>row.map(csvCell).join(',')).join('\r\n')],{type:'text/csv;charset=utf-8'});
    const url=URL.createObjectURL(blob),link=document.createElement('a');link.href=url;link.download='npu-utilization-'+date(snapshot.after)+'.csv';link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
  };
})();
