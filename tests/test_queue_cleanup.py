"""Guard and client tests use synthetic observations; they never access SSH or NPUs."""
import copy,contextlib,importlib.util,io,json,os
from pathlib import Path
import sys,tempfile,unittest
from unittest import mock
ROOT=Path(__file__).resolve().parents[1]
def module(name,path):
    s=importlib.util.spec_from_file_location(name,path);m=importlib.util.module_from_spec(s);s.loader.exec_module(m);return m
helper=module('cleanup_remote',ROOT/'internal/watch/cleanup_remote.py')
client=module('cleanup_client',ROOT/'skills/cluster-task-controller/scripts/taskctl.py')

class RemoteGuardTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.base=dict(schema='simplehtmlwatch.cleanup.remote.v1',bootId='boot',pid=42,startTicks=100,parentPid=40,parentStartTicks=80,commandHash='b'*64,parentCommandHash='c'*64,cwd='/test/moe',state='S',uptime=1000,ageSeconds=900,cpuSeconds=1,orphanForkserver=True,failure=True,debug=False,interactive=False,children=False,siblings=False,npu='fixture',logs=[dict(fd='1',size=100,mtimeNS=10,device=1,inode=2,hash='d'*64,tail='[ERROR] Task failed!')])
        self.current=copy.deepcopy(self.base);self.current.update(uptime=1301,cpuSeconds=1.08,ageSeconds=1201)
        self.request=dict(mode='terminate',id='residue-'+'a'*32,pid=42,baseline=self.base,last=copy.deepcopy(self.base),requirements=dict(model='910B',devicesPerMachine=2,minFreeHbmMB=0))
        self.signal=mock.Mock()
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(helper.Path,'home',return_value=Path(self.temp.name)).start()
        mock.patch.object(helper,'snapshot',side_effect=lambda pid:copy.deepcopy(self.current)).start()
        mock.patch.object(helper,'single_zero').start()
        mock.patch.object(helper.os,'pidfd_open',side_effect=lambda pid:os.open(os.devnull,os.O_RDONLY),create=True).start()
        mock.patch.object(helper.signal,'pidfd_send_signal',self.signal,create=True).start()
        poller=mock.Mock();poller.poll.return_value=[(1,1)]
        mock.patch.object(helper.select,'poll',return_value=poller,create=True).start()
    @unittest.skipUnless(hasattr(os, 'O_DIRECTORY'), 'Linux remote helper requires POSIX directory fsync')
    def test_term_only_after_durable_intent_and_never_replay(self):
        def sent(*args):
            p=Path(self.temp.name)/'.simplehtmlwatch/cleanup'/(self.request['id']+'.json')
            self.assertEqual(json.loads(p.read_text())['state'],'signal_intent')
            self.assertEqual(args[1],helper.signal.SIGTERM)
        self.signal.side_effect=sent
        self.assertTrue(helper.run(self.request)['exited'])
        self.assertEqual(helper.run(self.request)['state'],'previous_intent_no_replay')
        self.assertEqual(self.signal.call_count,1)
    def test_rejects_debug_unknown_reuse_and_progress(self):
        variants=[dict(debug=True),dict(interactive=True),dict(state='T'),dict(orphanForkserver=False),dict(failure=False),dict(children=True),dict(siblings=True),dict(startTicks=101),dict(bootId='new'),dict(parentStartTicks=81),dict(commandHash='a'*64),dict(uptime=1299),dict(cpuSeconds=2),dict(logs=[])]
        for changes in variants:
            with self.subTest(changes=changes):
                self.current=copy.deepcopy(self.base);self.current.update(uptime=1301,cpuSeconds=1.08);self.current.update(changes)
                with self.assertRaises((RuntimeError,KeyError)):helper.run(self.request)
                self.signal.assert_not_called()
    def test_changed_log_or_storage_failure_cannot_signal(self):
        self.current['logs'][0]['size']+=1
        with self.assertRaises(RuntimeError):helper.run(self.request)
        self.signal.assert_not_called()
        self.current['logs'][0]['size']-=1
        with mock.patch.object(helper,'durable',side_effect=OSError('disk full')):
            with self.assertRaises(OSError):helper.run(self.request)
        self.signal.assert_not_called()
    def test_pending_receipt_after_crash_is_not_replayed(self):
        root=Path(self.temp.name)/'.simplehtmlwatch/cleanup';root.mkdir(parents=True)
        (root/(self.request['id']+'.json')).write_text('{"state":"signal_intent"}')
        self.assertEqual(helper.run(self.request)['state'],'previous_intent_no_replay');self.signal.assert_not_called()
    def test_redaction(self):
        value=helper.redacted('token=abc password:xyz api_key secret secret=x')
        self.assertNotIn('abc',value);self.assertNotIn('xyz',value)

class SingleDeviceGuardTests(unittest.TestCase):
    def test_a5_and_a2_tables_and_nonzero_or_multiple_processes(self):
        a5='| NPU ID | Name | Health | Power Temp |\n| | | Bus-Id | NPU Util(%) Memory-Usage(MB) HBM-Usage(MB) |\n| 0 | Ascend950DT | Warning | 300 40 |\n| | | NA | 0 0 / 0 5000 / 98304 |\n| NPU ID | Process id | Process name | Process memory(MB) |\n| 0 | 42 | python | 302 |'
        req=dict(model='Ascend950DT',devicesPerMachine=1,minFreeHbmMB=60000)
        helper.single_zero(a5,42,req)
        for text in [a5.replace('0 0 / 0','80 0 / 0'),a5+'\n| 0 | 99 | python | 302 |',a5.replace('42','43'),a5.replace('Ascend950DT','unknown')]:
            with self.assertRaises(RuntimeError):helper.single_zero(text,42,req)
        a2='| NPU Name | Health | Power Temp |\n| Chip Device | Bus-Id | AICore(%) Memory-Usage(MB) HBM-Usage(MB) |\n| 0 910B | OK | 100 40 |\n| 0 0 | bus | 0 0 / 0 1000 / 65536 |\n| NPU ID | Chip ID | Process id | Process name | Process memory(MB) |\n| 0 | 0 | 42 | python | 302 |'
        helper.single_zero(a2,42,dict(model='910B',devicesPerMachine=1,minFreeHbmMB=60000))

class CleanupClientTests(unittest.TestCase):
    def call(self,args):
        fake=mock.Mock();fake.capabilities=set();fake.api.return_value={'policy':{'enabled':True}}
        with mock.patch.object(sys,'argv',['taskctl.py']+args),mock.patch.object(client,'Client',return_value=fake),contextlib.redirect_stdout(io.StringIO()):
            result=client.main()
        return fake,result
    def test_status_and_log_require_capability(self):
        f,r=self.call(['cleanup-status','--id','residue-id']);self.assertEqual(r,0)
        self.assertIn('queue-residue-cleanup-v1',f.capabilities);f.api.assert_called_once_with('/cleanup?id=residue-id')
    def test_enable_authorized_and_disable(self):
        f,_=self.call(['cleanup-policy','--enable','--authorization-ref','user-request'])
        f.api.assert_called_once_with('/cleanup','POST',{'enabled':True,'authorizationRef':'user-request'})
        f,_=self.call(['cleanup-policy','--disable']);self.assertFalse(f.api.call_args.args[2]['enabled'])
    def test_no_authorization_or_legacy_never_writes(self):
        for args in [['cleanup-policy','--enable'],['--url','http://127.0.0.1:1234','--allow-legacy','cleanup-policy','--disable']]:
            with self.assertRaises(ValueError):self.call(args)

class InspectionTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name);self.proc=self.root/'proc';self.proc.mkdir();self.cwd=self.root/'work';self.cwd.mkdir()
        p=self.proc/'sys/kernel/random';p.mkdir(parents=True);(p/'boot_id').write_text('boot');(self.proc/'uptime').write_text('1000 0')
        self.log=self.root/'failed.log';self.log.write_text('[ERROR] Task failed!')
        self.make_proc(42,40);self.make_proc(40,1)
        original=Path
        def mapped(value,*args):
            text=str(value)
            if text=='/proc' or text.startswith('/proc/'):text=str(self.proc)+text[5:]
            return original(text,*args)
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(helper,'Path',side_effect=mapped).start()
        mock.patch.object(helper,'npu_output',return_value='synthetic NPU fixture').start()
        # The /proc fixture represents a Linux host, including its clock ticks.
        # Windows controllers do not expose os.sysconf; no real host clock is used.
        mock.patch.object(helper.os,'sysconf',return_value=100,create=True).start()
    def make_proc(self,pid,ppid):
        p=self.proc/str(pid);p.mkdir();fields=['0']*30;fields[0]='S';fields[1]=str(ppid);fields[19]='100';fields[11]='10';fields[12]='10'
        (p/'stat').write_text(str(pid)+' (python worker) '+' '.join(fields));(p/'cmdline').write_bytes(b'python\0-c\0from multiprocessing.forkserver import main;main()\0')
        (p/'environ').write_bytes(b'');(p/'cwd').symlink_to(self.cwd);(p/'fd').mkdir();(p/'fd/0').symlink_to('/dev/null');(p/'fd/1').symlink_to(self.log);(p/'fd/2').symlink_to(self.log)
        thread=p/'task'/str(pid);thread.mkdir(parents=True);(thread/'status').write_text('TracerPid:\t0\n')
    def test_unattended_orphan_failure_and_thread_debugger(self):
        s=helper.snapshot(42);self.assertTrue(s['orphanForkserver']);self.assertTrue(s['failure']);self.assertFalse(s['debug']);self.assertFalse(s['interactive'])
        (self.proc/'42/task/42/status').write_text('TracerPid:\t999\n');self.assertTrue(helper.snapshot(42)['debug'])
    def test_failure_marker_at_actual_end_of_large_log(self):
        self.log.write_text('ordinary progress line\n'*1000+'[ERROR] Task failed!')
        s=helper.snapshot(42)
        self.assertTrue(s['failure'])
        self.assertTrue(s['logs'][0]['tail'].endswith('[ERROR] Task failed!'))
        self.assertLessEqual(len(s['logs'][0]['tail']),4096)
    def test_protection_marker_and_environment(self):
        (self.cwd/'.shw-debug-protect').touch();self.assertTrue(helper.snapshot(42)['debug'])
        (self.cwd/'.shw-debug-protect').unlink();(self.proc/'42/environ').write_bytes(b'SHW_DEBUG_PROTECT=1\0');self.assertTrue(helper.snapshot(42)['debug'])
    def test_missing_trace_field_or_identity_change_is_unknown(self):
        (self.proc/'42/task/42/status').write_text('Name: worker\n')
        with self.assertRaises(RuntimeError):helper.snapshot(42)
        (self.proc/'42/task/42/status').write_text('TracerPid:\t0\n');original=helper.identity;calls={}
        def reused(pid):
            r=original(pid);calls[pid]=calls.get(pid,0)+1
            if pid==42 and calls[pid]>=3:r['startTicks']+=1
            return r
        with mock.patch.object(helper,'identity',side_effect=reused):
            with self.assertRaises(RuntimeError):helper.snapshot(42)

if __name__=='__main__':unittest.main()
