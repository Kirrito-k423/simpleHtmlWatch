"""Executed through the existing trusted SSH transport. Unknown facts never signal."""
import hashlib,json,os,re,signal,stat,subprocess,sys,time,select
from pathlib import Path

def read(path):
    return Path(path).read_text(errors='replace')
def identity(pid):
    p=Path('/proc')/str(pid); fields=read(p/'stat').rsplit(') ',1)[1].split()
    cmd=(p/'cmdline').read_bytes()
    return dict(pid=pid,parentPid=int(fields[1]),startTicks=int(fields[19]),state=fields[0],cpuSeconds=(int(fields[11])+int(fields[12]))/os.sysconf('SC_CLK_TCK'),commandHash=hashlib.sha256(cmd).hexdigest(),command=cmd.decode(errors='replace').replace('\0',' ').strip())
def npu_output():
    r=subprocess.run(['bash','-lc','npu-smi info'],capture_output=True,text=True,timeout=8)
    if r.returncode or len(r.stdout)>32768:raise RuntimeError('npu_sample_unknown')
    return r.stdout

def redacted(text):
    text=re.sub(r'(?i)(authorization[\s:=]+)(?:Bearer\s+|Basic\s+)?\S+',r'\1<redacted>',text)
    return re.sub(r'(?i)((?:password|token|secret|api[_-]?key)[\s:=]+)\S+',r'\1<redacted>',text)[:4096]
def snapshot(pid):
    out=dict(schema='simplehtmlwatch.cleanup.remote.v1',bootId=read('/proc/sys/kernel/random/boot_id').strip(),uptime=float(read('/proc/uptime').split()[0]),npu=npu_output(),pid=pid)
    p=Path('/proc')/str(pid)
    try:r=identity(pid)
    except FileNotFoundError:out['missing']=True;return out
    command=r.pop('command');out.update(r);out['ageSeconds']=out['uptime']-r['startTicks']/os.sysconf('SC_CLK_TCK')
    if out['state']=='Z':return out
    out['cwd']=os.readlink(p/'cwd');out['command']=redacted(command)
    parent=identity(out['parentPid']);parent_cmd=parent.pop('command')
    out['parentStartTicks']=parent['startTicks'];out['parentCommandHash']=parent['commandHash']
    out['orphanForkserver']=parent['parentPid']==1 and 'multiprocessing.forkserver' in command and 'multiprocessing.forkserver' in parent_cmd
    out['children']=False;out['siblings']=False;out['debug']=False
    # Inspect only related processes; the diagnostic shell must not match itself.
    for d in Path('/proc').iterdir():
        if not d.name.isdigit():continue
        try:r=identity(int(d.name))
        except (OSError,ValueError,IndexError):continue
        if r['state']!='Z':
            if r['parentPid']==pid:out['children']=True
            if r['pid']!=pid and r['parentPid']==out['parentPid']:out['siblings']=True
    for q,cmd in ((p,command),(Path('/proc')/str(out['parentPid']),parent_cmd)):
        if re.search(r'\b(gdb|lldb|debugpy|pydevd|pdb|msdebug|ascend-dbg)\b',cmd,re.I):out['debug']=True
        if identity(int(q.name))['state'] in ('T','t'):out['debug']=True
        for t in (q/'task').iterdir():
            m=re.search(r'^TracerPid:\s*(\d+)',read(t/'status'),re.M)
            if not m:raise RuntimeError('thread_trace_unknown')
            if int(m.group(1)):out['debug']=True
        env=(q/'environ').read_bytes().decode(errors='replace').split('\0')
        for e in env:
            if e.startswith('SHW_DEBUG_PROTECT=') and e.split('=',1)[1].lower() in ('1','true','yes'):out['debug']=True
            if e.startswith('PYTHONBREAKPOINT=') and e.split('=',1)[1] not in ('','0'):out['debug']=True
        try:
            (Path(os.readlink(q/'cwd'))/'.shw-debug-protect').stat();out['debug']=True
        except FileNotFoundError:pass
    out['interactive']=os.readlink(p/'fd/0')!='/dev/null'
    out['logs']=[]
    for name in ('1','2'):
        with open(p/'fd'/name,'rb') as f:
            s=os.fstat(f.fileno())
            if not stat.S_ISREG(s.st_mode):continue
            f.seek(max(0,s.st_size-8192));tail=f.read(8192)
            out['logs'].append(dict(fd=name,size=s.st_size,mtimeNS=s.st_mtime_ns,device=s.st_dev,inode=s.st_ino,hash=hashlib.sha256(tail).hexdigest(),tail=redacted(tail[-4096:].decode(errors='replace'))))
    out['failure']=any(re.search(r'(\[ERROR\]\s*Task failed!|HCCL function error:|torch\.distributed\.DistNetworkError:|ConnectionRefusedError: \[Errno 111\])',l['tail']) for l in out['logs'])
    # The start identity must still match after reading every piece of evidence.
    end=identity(pid);end_parent=identity(out['parentPid'])
    if end['startTicks']!=out['startTicks'] or end['commandHash']!=out['commandHash'] or end_parent['startTicks']!=out['parentStartTicks'] or end_parent['commandHash']!=out['parentCommandHash']:raise RuntimeError('identity_changed_during_inspection')
    return out

def viable(s):
    return not s.get('missing') and s.get('state')=='S' and s.get('orphanForkserver') and s.get('failure') and s.get('ageSeconds',0)>300 and not any(s.get(k) for k in ('debug','interactive','children','siblings')) and bool(s.get('logs'))
def signatures(s):
    return [{k:l[k] for k in ('fd','size','mtimeNS','device','inode','hash')} for l in s['logs']]
def single_zero(text,pid,requirements):
    # Header indices support A2/A3 and A5. Unknown layouts fail closed.
    rows=text.splitlines();header=next(i for i,l in enumerate(rows) if 'Process id' in l)
    cells=lambda l:[c.strip() for c in l.split('|')[1:-1]]
    h=cells(rows[header]);pi=h.index('Process id');processes=[]
    for l in rows[header+1:]:
        c=cells(l)
        if not c or 'No running process' in l:continue
        if len(c)!=len(h) or not c[0].isdigit() or not c[pi].isdigit():raise RuntimeError('process_table_unknown')
        processes.append(int(c[pi]))
    if processes!=[pid]:raise RuntimeError('occupancy_changed')
    u=None;hb=None;current=None;model=None;values=[];models=[]
    for l in rows[:header]:
        c=cells(l)
        if not c:continue
        for i,v in enumerate(c):
            if 'AICore(%)' in v or 'NPU Util(%)' in v:u=i
            if 'HBM-Usage(MB)' in v:hb=i
        if re.match(r'^\d+(?:\s+\S+)?$',c[0]) and len(c)>1 and (c[1].startswith('Ascend') or re.search(r'\b910\w*',c[0])):
            current=c[0].split()[0];model=c[1] if c[1].startswith('Ascend') else c[0].split()[1];models.append(model);continue
        if current is not None and u is not None and hb is not None and len(c)>max(u,hb) and re.search(r'\d+\s*/\s*\d+',c[hb]):
            util=float(c[u].split()[0]);used,total=map(int,re.findall(r'(\d+)\s*/\s*(\d+)',c[hb])[-1])
            if util!=0 or total-used<requirements['minFreeHbmMB']:raise RuntimeError('resource_changed')
            values.append(util)
    if len(values)!=requirements['devicesPerMachine'] or not models or any(m!=requirements['model'] for m in models):raise RuntimeError('device_model_or_count_changed')

def durable(path,value,exclusive=False):
    data=json.dumps(value,ensure_ascii=False).encode()
    if exclusive:
        fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
        with os.fdopen(fd,'wb') as f:f.write(data);f.flush();os.fsync(f.fileno())
    else:
        temp=path.with_suffix('.tmp')
        with open(temp,'wb') as f:os.chmod(temp,0o600);f.write(data);f.flush();os.fsync(f.fileno())
        os.replace(temp,path)
    fd=os.open(path.parent,os.O_RDONLY|os.O_DIRECTORY)
    try:os.fsync(fd)
    finally:os.close(fd)

def run(request):
    op=request['id'];pid=request['pid']
    if not re.fullmatch(r'residue-[a-f0-9]{32}',op) or not isinstance(pid,int) or pid<=1:raise ValueError('invalid_identity')
    root=Path.home()/'.simplehtmlwatch'/'cleanup';receipt=root/(op+'.json')
    if request['mode']=='inspect':
        out=snapshot(pid)
        if receipt.exists():out['receipt']=json.loads(read(receipt))
        return out
    if request['mode']!='terminate':raise ValueError('invalid_mode')
    if receipt.exists():return dict(state='previous_intent_no_replay',receipt=json.loads(read(receipt)))
    if not hasattr(os,'pidfd_open') or not hasattr(signal,'pidfd_send_signal'):raise RuntimeError('pidfd_unavailable')
    fd=os.pidfd_open(pid)
    try:
        s=snapshot(pid);base=request['baseline'];last=request['last']
        for k in ('bootId','pid','startTicks','parentPid','parentStartTicks','commandHash','parentCommandHash','cwd'):
            if s[k]!=last[k] or last[k]!=base[k]:raise RuntimeError('identity_changed')
        if not viable(s) or s['uptime']-base['uptime']<300 or not 0<=s['cpuSeconds']-base['cpuSeconds']<1 or signatures(s)!=signatures(base) or signatures(s)!=signatures(last):raise RuntimeError('residue_not_confirmed')
        single_zero(s['npu'],pid,request['requirements'])
        root.mkdir(parents=True,exist_ok=True,mode=0o700)
        record=dict(state='signal_intent',id=op,signal='SIGTERM',baseline=base,last=s)
        durable(receipt,record,True)
        # pidfd binds the signal to the process inspected above, even if its PID exits/reuses.
        signal.pidfd_send_signal(fd,signal.SIGTERM)
        poller=select.poll();poller.register(fd,select.POLLIN);exited=bool(poller.poll(3000))
        record.update(state='terminated' if exited else 'uncertain',exited=exited)
        durable(receipt,record)
        return dict(state=record['state'],exited=exited,id=op)
    finally:os.close(fd)

if __name__=='__main__':
    try:print(json.dumps(run(json.loads(sys.argv[1])),ensure_ascii=False))
    except Exception as e:
        print(json.dumps(dict(error=type(e).__name__+': '+str(e)),ensure_ascii=False));sys.exit(1)
