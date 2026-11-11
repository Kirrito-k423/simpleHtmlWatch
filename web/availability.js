(() => {
  const el=id=>document.querySelector('#availability-'+id),dialog=el('dialog');
  const token=document.querySelector('meta[name="watch-token"]').content;
  const escape=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const stamp=value=>new Date(Date.parse(value)+8*3600000).toISOString().slice(0,19).replace('T',' ');
  const decimal=value=>Number(value).toFixed(1),scopes={work:'工作时段',off:'非工作时段',all:'全天'};
  let snapshot=null,controller=null,generation=0,timer=null,svg='';
  const colors=['#269b68','#d1a047','#d46d62','#d9dfe7'];
  function message(text,error=false){el('message').textContent=text;el('message').classList.toggle('error',error);}
  function clear(){snapshot=null;svg='';for(const id of ['table','chart','summary'])el(id).replaceChildren();el('meta').textContent='';for(const id of ['png','svg','csv'])el(id).disabled=true;}
  function chart(data){
    const width=1100,height=195+data.machines.length*34,left=315,plot=600,elapsed=data.expectedSeconds,maxUnits=elapsed/1800;
    const t=(x,y,text,size=12,extra='')=>'<text x="'+x+'" y="'+y+'" font-size="'+size+'" fill="#253047" '+extra+'>'+escape(text)+'</text>';
    let body='<rect width="1100" height="'+height+'" fill="#ffffff"/>'+t(30,34,'今日机器紧张情况 · '+scopes[data.scope],24)+t(30,57,stamp(data.after)+' 至 '+stamp(data.before)+'（北京时间）')+t(30,81,'资源 '+data.machineCount+' 个 · 可用半小时窗 '+data.counts.minutes30+' 个 · 观测覆盖 '+decimal(data.coveragePercent)+'%');
    ['可用整窗','碎片空闲','已观测占用','未观测'].forEach((name,i)=>{const x=30+i*160;body+='<rect x="'+x+'" y="100" width="12" height="12" rx="2" fill="'+colors[i]+'"/>'+t(x+19,111,name);});
    body+=t(30,143,'机器 / 配置身份')+t(left,143,'累计容量（每单位 = 30 分钟）')+t(948,143,'可用窗 / 覆盖');
    for(let tick=0;tick<=4;tick++){
      const x=left+plot*tick/4;
      body+='<line x1="'+x+'" y1="151" x2="'+x+'" y2="'+(height-35)+'" stroke="#e9edf3"/>'+t(x,height-18,decimal(maxUnits*tick/4),10,'text-anchor="middle"');
    }
    data.machines.forEach((row,i)=>{
      const y=165+i*34,name=Array.from(row.name).slice(0,31).join('');
      body+=t(30,y+13,name,12);
      let x=left;
      [row.counts.minutes30*1800,row.fragmentSeconds,row.busySeconds,row.unknownSeconds].forEach((value,k)=>{const w=elapsed?Math.max(0,value)/elapsed*plot:0;body+='<rect x="'+x.toFixed(3)+'" y="'+y+'" width="'+w.toFixed(3)+'" height="18" fill="'+colors[k]+'"><title>'+escape(['可用整窗','碎片空闲','已观测占用','未观测'][k]+'：'+decimal(value/1800)+' 个半小时单位')+'</title></rect>';x+=w;});
      body+=t(948,y+13,row.counts.minutes30+' 个 / '+decimal(row.coveragePercent)+'%');
    });
    body+=t(30,height-2,'绿色 = 实际形成的独立整窗；其余颜色为时长折合单位。未知不计占用。按配置物理标识去重。',10);
    return '<svg xmlns="http://www.w3.org/2000/svg" width="'+width+'" height="'+height+'" viewBox="0 0 '+width+' '+height+'" role="img" aria-label="今日机器半小时窗口容量图" font-family="system-ui, -apple-system, Segoe UI, sans-serif">'+body+'</svg>';
  }
  function render(){
    const data=snapshot;svg=chart(data);el('chart').innerHTML=svg;
    const cards=[['2 小时','minutes120'],['1 小时','minutes60'],['半小时','minutes30'],['15 分钟','minutes15'],['5 分钟','minutes5']];
    el('summary').innerHTML=cards.map(([label,key])=>'<div><span>'+label+'独立空闲窗</span><strong>'+data.counts[key]+'<small> 个</small></strong></div>').join('');
    el('table').innerHTML='<table><thead><tr><th>机器</th><th>2 小时</th><th>1 小时</th><th>半小时</th><th>15 分钟</th><th>5 分钟</th><th>最长连续空闲</th><th>观测覆盖</th><th>未观测</th></tr></thead><tbody>'+data.machines.map(row=>'<tr><th>'+escape(row.name)+'<small>'+escape(row.group)+' · '+row.machineIds.length+' 个入口</small></th>'+cards.map(([,key])=>'<td>'+row.counts[key]+'</td>').join('')+'<td>'+decimal(row.longestIdleSeconds/60)+' 分钟</td><td>'+decimal(row.coveragePercent)+'%</td><td>'+decimal(row.unknownSeconds/60)+' 分钟</td></tr>').join('')+'</tbody></table>';
    el('meta').textContent=scopes[data.scope]+' · '+stamp(data.after)+' — '+stamp(data.before)+' · '+data.configuredEntries+' 个配置入口 / '+data.machineCount+' 个资源 · 已经过的所选时段 '+decimal(data.expectedSeconds/3600)+' 小时 · 覆盖 '+decimal(data.coveragePercent)+'%';
    const problems=[data.storageError&&'历史保存异常：'+data.storageError,data.historyTruncated&&'历史因容量上限发生裁剪，缺失部分按未观测处理'].filter(Boolean);
    message(problems.length?problems.join('；'):!data.expectedSeconds?'今天尚未进入所选时段。':data.machines.length?'五档分别计数，同一档内互不重叠；不同档位之间不相加。未观测时段不计空闲，也不计占用。':'当前分组没有机器。',!!problems.length);
    for(const id of ['png','svg','csv'])el(id).disabled=!data.machines.length;
  }
  async function load(){
    const request=++generation;controller?.abort();controller=new AbortController();clear();message('读取'+scopes[el('scope').value]+'窗口统计…');
    if(typeof demo!=='undefined'&&demo){clear();message('演示模式没有真实窗口历史；请退出演示。');return;}
    try{
      const query=new URLSearchParams({scope:el('scope').value});if(el('group').value)query.set('group',el('group').value);
      const response=await fetch('/api/availability?'+query,{headers:{'X-Watch-Token':token},signal:controller.signal});
      const data=await response.json();if(!response.ok)throw new Error(data.error||'读取窗口统计失败');
      if(request!==generation||!dialog.open)return;
      if(data.schema!=='simplehtmlwatch.daily-idle-windows.v1'||!Array.isArray(data.machines)||!data.counts||!scopes[data.scope])throw new Error('窗口统计格式无效');
      snapshot=data;render();
    }catch(error){if(error.name!=='AbortError'&&request===generation){clear();message(error.message,true);}}
  }
  el('btn').onclick=async()=>{
    clearInterval(timer);dialog.showModal();clear();message('读取全天空闲机会…');const opening=generation;
    try{
      if(typeof demo!=='undefined'&&!demo)await loadConfig();
      if(!dialog.open||opening!==generation)return;
      const groups=[...new Set((config.machines||[]).map(m=>m.group).filter(Boolean))].sort();
      el('group').innerHTML='<option value="">所有分组</option>'+groups.map(g=>'<option value="'+escape(g)+'">'+escape(g)+'</option>').join('');
      await load();if(dialog.open)timer=setInterval(load,30000);
    }catch(error){message(error.message,true);}
  };
  el('close').onclick=()=>dialog.close();
  dialog.addEventListener('close',()=>{generation++;controller?.abort();clearInterval(timer);timer=null;clear();});
  el('refresh').onclick=load;el('group').onchange=load;el('scope').onchange=load;
  function download(blob,ext,day,scope){const url=URL.createObjectURL(blob),a=document.createElement('a');a.href=url;a.download='machine-idle-windows-'+scope+'-'+day+'.'+ext;a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);}
  el('svg').onclick=()=>{if(snapshot)download(new Blob([svg],{type:'image/svg+xml;charset=utf-8'}),'svg',stamp(snapshot.after).slice(0,10),snapshot.scope);};
  el('png').onclick=async()=>{
    if(!snapshot)return;const source=svg,day=stamp(snapshot.after).slice(0,10),scope=snapshot.scope;el('png').disabled=true;
    try{
      const image=new Image();image.src='data:image/svg+xml;charset=utf-8,'+encodeURIComponent(source);await image.decode();
      const scale=Math.max(1,Math.min(2,Math.sqrt(15000000/(image.width*image.height))));
      const canvas=document.createElement('canvas');canvas.width=Math.floor(image.width*scale);canvas.height=Math.floor(image.height*scale);
      const ctx=canvas.getContext('2d');ctx.scale(scale,scale);ctx.drawImage(image,0,0);
      const blob=await new Promise(resolve=>canvas.toBlob(resolve,'image/png'));if(!blob)throw new Error('图片导出失败');download(blob,'png',day,scope);
    }catch(error){message('图片导出失败：'+error.message,true);}finally{if(snapshot)el('png').disabled=false;}
  };
  el('csv').onclick=()=>{
    if(!snapshot)return;const quote=value=>'"'+String(value??'').replace(/"/g,'""').replace(/^[=+@-]/,"'$&")+'"';
    const rows=[['日期','范围','资源标识','机器','分组','2小时独立窗','1小时独立窗','半小时独立窗','15分钟独立窗','5分钟独立窗','最长空闲秒数','空闲秒数','占用秒数','未观测秒数','碎片空闲秒数','覆盖率(%)']],day=stamp(snapshot.after).slice(0,10);
    for(const r of snapshot.machines)rows.push([day,scopes[snapshot.scope],r.resourceKey,r.name,r.group,r.counts.minutes120,r.counts.minutes60,r.counts.minutes30,r.counts.minutes15,r.counts.minutes5,r.longestIdleSeconds,r.idleSeconds,r.busySeconds,r.unknownSeconds,r.fragmentSeconds,r.coveragePercent]);
    download(new Blob(['\uFEFF'+rows.map(r=>r.map(quote).join(',')).join('\r\n')],{type:'text/csv;charset=utf-8'}),'csv',day,snapshot.scope);
  };
  (async()=>{if(await window.watchReady){const params=new URLSearchParams(location.search);if(!params.has('tasks')&&params.get('demo')!=='1')el('btn').click();}})();
})();
