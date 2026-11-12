(function(root){
  'use strict';
  const durations=[{minutes:120,label:'2 小时',key:'minutes120'},{minutes:60,label:'1 小时',key:'minutes60'},{minutes:30,label:'半小时',key:'minutes30'},{minutes:15,label:'15 分钟',key:'minutes15'},{minutes:5,label:'5 分钟',key:'minutes5'}];
  const colors=['#32b783','#d4a346','#d2676d','#596675'];
  const escape=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const stamp=value=>new Date(Date.parse(value)+28800000).toISOString().slice(0,19).replace('T',' ');
  const scopeNames={work:'工作时段',off:'非工作时段',all:'全天'};
  const percent=(count,limit)=>Number.isFinite(limit)&&limit>0?(count*100/limit).toFixed(1)+'%':'—';
  function shortName(name){const match=String(name).match(/B\d{2}[-_\s]\d+/i);return match?match[0].replace(/[_\s]/g,'-'):Array.from(String(name).split('·')[0].trim()).slice(0,11).join('');}
  function fit(count,width,height){
    let best={columns:1,rows:Math.max(1,count),size:0};
    for(let columns=1;columns<=Math.max(1,count);columns++){
      const rows=Math.max(1,Math.ceil(count/columns)),size=Math.min((width-(columns-1)*3)/columns,(height-(rows-1)*3)/rows);
      if(size>best.size)best={columns,rows,size};
    }
    return best;
  }
  function render(data,{width=2000,height=1200}={}){
    width=Math.max(320,Math.round(width));height=Math.max(260,Math.round(height));
    const nodes=data.nodes||1,multi=nodes>1,top=width<1000?68:52,padding=6,gap=6,panelWidth=(width-padding*2-gap*4)/5,panelHeight=height-top-padding;
    const rows=[...data.machines].sort((a,b)=>(a.pool||a.group||'').localeCompare(b.pool||b.group||'','zh-CN',{numeric:true})||shortName(a.name).localeCompare(shortName(b.name),'zh-CN',{numeric:true})||a.resourceKey.localeCompare(b.resourceKey));
    const num=value=>Number(value).toFixed(2),text=(x,y,value,size=12,fill='#dce6f2',extra='')=>'<text x="'+num(x)+'" y="'+num(y)+'" font-size="'+num(size)+'" fill="'+fill+'" '+extra+'>'+escape(value)+'</text>';
    let svg='<rect width="'+width+'" height="'+height+'" fill="#0e151f"/>'+text(12,19,(data.date||stamp(data.after).slice(0,10))+' 独立实验窗口 · '+scopeNames[data.scope]+' · '+(multi?nodes+' 机同组':'单机'),14)+text(12,36,stamp(data.after)+' — '+stamp(data.before)+' · '+data.machineCount+' 个节点 · 覆盖 '+Number(data.coveragePercent).toFixed(1)+'%',10,'#95a6ba');
    const names=[multi?'有效组网参与':'完整空闲窗口',multi?'零散／未编组空闲':'碎片空闲','已观测占用','未观测'];
    const legendY=width<1000?53:33,legendX=width<1000?12:Math.max(510,width-620),legendStep=width<1000?Math.min(150,(width-24)/4):150;
    names.forEach((name,i)=>{const x=legendX+i*legendStep;svg+='<circle cx="'+x+'" cy="'+(legendY-3)+'" r="4" fill="'+colors[i]+'"/>'+text(x+9,legendY,name,width<700?8:10,'#a9b7ca');});
    durations.forEach((duration,panel)=>{
      const x=padding+panel*(panelWidth+gap),y=top,spaceWidth=panelWidth-12,spaceHeight=panelHeight-58;
      const capacity=data.theoreticalCounts?.[duration.key]??'—';
      svg+='<g class="availability-duration" data-duration="'+duration.minutes+'" role="group" aria-label="'+duration.label+'窗口图"><rect x="'+num(x)+'" y="'+y+'" width="'+num(panelWidth)+'" height="'+num(panelHeight)+'" rx="7" fill="#121c28" stroke="#2b394c"/>'+text(x+10,y+21,duration.label,Math.min(17,panelWidth*.075),'#f1f6ff')+text(x+10,y+39,(data.historyAvailable===false?'无历史数据':(multi?'可排布 ':'独立空闲窗 ')+(data.counts[duration.key]||0)+' / '+capacity+(multi?' 次':' 个')+' · '+percent(data.counts[duration.key]||0,data.theoreticalCounts?.[duration.key])),Math.min(11,panelWidth*.052),'#a7e1c6');
      if(!rows.length){svg+=text(x+panelWidth/2,y+panelHeight/2,data.historyAvailable===false?'无历史数据':'没有节点',10,'#a9b7ca','text-anchor="middle"')+'</g>';return;}
      const grid=fit(rows.length,spaceWidth,spaceHeight),size=Math.max(1,grid.size),gridWidth=grid.columns*size+(grid.columns-1)*3,gridHeight=grid.rows*size+(grid.rows-1)*3;
      const left=x+(panelWidth-gridWidth)/2,startY=y+49+(spaceHeight-gridHeight)/2;
      rows.forEach((row,i)=>{
        const tx=left+(i%grid.columns)*(size+3),ty=startY+Math.floor(i/grid.columns)*(size+3),cx=tx+size/2,cy=ty+size*.55,radius=size*.285,stroke=Math.max(1,size*.09),circumference=2*Math.PI*radius,count=row.counts[duration.key]||0;
        const green=count*duration.minutes*60,values=[green,Math.max(0,row.idleSeconds-green),row.busySeconds,row.unknownSeconds],expected=data.expectedSeconds;
        const label=shortName(row.name),qualifier=multi?'参与':'窗口',title=row.name+' · '+duration.label+' · '+qualifier+' '+count+' · 覆盖 '+Number(row.coveragePercent).toFixed(1)+'%';
        svg+='<g class="availability-node" data-node-resource="'+escape(row.resourceKey)+'" data-node-duration="'+duration.minutes+'" role="button" tabindex="0" aria-label="'+escape(title)+'"><title>'+escape(title)+'</title><rect x="'+num(tx)+'" y="'+num(ty)+'" width="'+num(size)+'" height="'+num(size)+'" rx="4" fill="#192534" stroke="#2b3c50"/>'+text(cx,ty+size*.15,label,Math.max(4,Math.min(11,size*.145)),'#dce6f2','text-anchor="middle"')+'<circle cx="'+num(cx)+'" cy="'+num(cy)+'" r="'+num(radius)+'" fill="none" stroke="#344152" stroke-width="'+num(stroke)+'"/>';
        let offset=0;
        values.forEach((value,k)=>{const length=expected?Math.max(0,Math.min(expected,value))/expected*circumference:0;if(length>0)svg+='<circle cx="'+num(cx)+'" cy="'+num(cy)+'" r="'+num(radius)+'" fill="none" stroke="'+colors[k]+'" stroke-width="'+num(stroke)+'" stroke-dasharray="'+num(length)+' '+num(circumference)+'" stroke-dashoffset="'+num(-offset)+'" transform="rotate(-90 '+num(cx)+' '+num(cy)+')"/>';offset+=length;});
        svg+=text(cx,cy+size*.065,multi&&row.multiReason?'—':count,Math.max(5,Math.min(17,size*.235)),'#eff8f5','text-anchor="middle"')+text(cx,ty+size*.93,'覆盖 '+Number(row.coveragePercent).toFixed(1)+'%',Math.max(3.8,Math.min(9,size*.11)),'#9badc4','text-anchor="middle"')+'</g>';
      });
      svg+='</g>';
    });
    return '<svg xmlns="http://www.w3.org/2000/svg" width="'+width+'" height="'+height+'" viewBox="0 0 '+width+' '+height+'" role="group" aria-label="五档实验窗口节点圆环总览" font-family="system-ui, -apple-system, Segoe UI, sans-serif">'+svg+'</svg>';
  }
  const api={render,percent,durations,shortName,escape,stamp,scopeNames};root.AvailabilityCharts=api;
  if(typeof module!=='undefined'&&module.exports)module.exports=api;
})(typeof window==='undefined'?globalThis:window);
